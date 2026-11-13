package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type installFixture struct {
	checker       *Checker
	ops           installOps
	current       string
	stage         string
	checksum      func(string, string) *string
	version       string
	smokeCalls    int
	execCalls     int
	requestedTemp string
}

func newInstallFixture(t *testing.T) *installFixture {
	t.Helper()
	f := &installFixture{checker: New("0.5.5"), current: filepath.Join(t.TempDir(), "switcher"), version: "switcher 0.5.6\n"}
	f.checker.latest = "v0.5.6"
	f.checker.lastCheck = time.Now() // so State() never starts a real check
	if err := os.WriteFile(f.current, []byte("old fixture"), 0o711); err != nil {
		t.Fatal(err)
	}
	current, err := filepath.EvalSymlinks(f.current)
	if err != nil {
		t.Fatal(err)
	}
	f.current = current
	f.checksum = func(sum, asset string) *string {
		value := sum + "  " + asset + "\n"
		return &value
	}
	f.ops = installOps{
		tempDir: func(parent, pattern string) (string, error) {
			f.requestedTemp = parent
			// Even buggy implementations remain confined to fixture storage.
			dir, err := os.MkdirTemp(filepath.Dir(f.current), pattern)
			f.stage = dir
			return dir, err
		},
		download: func(ctx context.Context, tag, asset, dir string, progress func(done, total int64)) error {
			data := []byte("new fixture")
			if err := os.WriteFile(filepath.Join(dir, asset), data, 0o600); err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			if checksum := f.checksum(hex.EncodeToString(sum[:]), asset); checksum != nil {
				writeSignedSums(t, dir, *checksum)
			}
			return nil
		},
		smoke: func(ctx context.Context, path string) ([]byte, error) {
			f.smokeCalls++
			return []byte(f.version), nil
		},
		executable: func() (string, error) { return f.current, nil },
		rename:     os.Rename,
		chmod:      os.Chmod,
		syncDir:    syncDirectory,
		exec: func(path string) error {
			f.execCalls++
			return nil // No process is executed, replaced, or restarted.
		},
	}
	return f
}

func assertFixtureFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content || info.Mode().Perm() != mode {
		t.Errorf("%s: content=%q mode=%o, want %q and %o", path, data, info.Mode().Perm(), content, mode)
	}
}

func TestChecksumRequiresExactlyOneValidEntryBeforeExecution(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(string, string) *string
	}{
		{"missing", func(string, string) *string { return nil }},
		{"empty", func(string, string) *string { s := ""; return &s }},
		{"whitespace", func(string, string) *string { s := " \n\t"; return &s }},
		{"short digest", func(string, string) *string { s := "abcd"; return &s }},
		{"nonhex digest", func(string, string) *string { s := strings.Repeat("g", 64); return &s }},
		{"mismatch", func(string, string) *string { s := strings.Repeat("0", 64); return &s }},
		{"multiple entries", func(sum, asset string) *string { s := sum + "  " + asset + "\n" + sum + "  " + asset + "\n"; return &s }},
		{"wrong asset", func(sum, asset string) *string { s := sum + "  other-binary\n"; return &s }},
		{"extra fields", func(sum, asset string) *string { s := sum + "  " + asset + " extra"; return &s }},
		{"split entry", func(sum, asset string) *string { s := sum + "\n" + asset; return &s }},
		// A release's list names every file; a digest alone names none.
		{"digest only", func(sum, asset string) *string { return &sum }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInstallFixture(t)
			f.checksum = tc.make
			if err := f.checker.installAndRestart(f.ops); err == nil {
				t.Error("invalid checksum was accepted")
			}
			if f.smokeCalls != 0 || f.execCalls != 0 {
				t.Errorf("unverified binary reached execution hooks: smoke=%d exec=%d", f.smokeCalls, f.execCalls)
			}
			assertFixtureFile(t, f.current, "old fixture", 0o711)
		})
	}
}

func TestValidChecksumFormats(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(string, string) *string
	}{
		{"shasum", func(sum, asset string) *string { s := sum + "  " + asset + "\n"; return &s }},
		{"among the release's other files", func(sum, asset string) *string {
			s := strings.Repeat("1", 64) + "  Switcher_0.5.6.dmg\n" + sum + "  " + asset + "\n" + strings.Repeat("2", 64) + "  " + asset + ".sha256\n"
			return &s
		}},
		{"binary marker and uppercase", func(sum, asset string) *string { s := strings.ToUpper(sum) + " *" + asset + "\r\n"; return &s }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInstallFixture(t)
			f.checksum = tc.make
			if err := f.checker.installAndRestart(f.ops); err != nil {
				t.Fatal(err)
			}
			assertFixtureFile(t, f.current, "new fixture", 0o755)
			assertFixtureFile(t, f.current+".previous", "old fixture", 0o711)
		})
	}
}

// Only a list signed by a release key is trusted: unsigned, signed by
// another key or for another purpose, or changed after signing, nothing
// downloaded runs or replaces the install.
func TestOnlySignedReleasesInstall(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, dir, sums string)
	}{
		{"unsigned", func(t *testing.T, dir, sums string) {
			_ = os.WriteFile(filepath.Join(dir, sumsName), []byte(sums), 0o600)
		}},
		{"another key", func(t *testing.T, dir, sums string) {
			_ = os.WriteFile(filepath.Join(dir, sumsName), []byte(sums), 0o600)
			_ = os.WriteFile(filepath.Join(dir, signatureName), signSums(otherKey, []byte(sums), signatureNamespace), 0o600)
		}},
		{"another purpose", func(t *testing.T, dir, sums string) {
			_ = os.WriteFile(filepath.Join(dir, sumsName), []byte(sums), 0o600)
			_ = os.WriteFile(filepath.Join(dir, signatureName), signSums(testKey, []byte(sums), "file"), 0o600)
		}},
		{"changed after signing", func(t *testing.T, dir, sums string) {
			writeSignedSums(t, dir, strings.Repeat("0", 64)+"  other\n")
			_ = os.WriteFile(filepath.Join(dir, sumsName), []byte(sums), 0o600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInstallFixture(t)
			download := f.ops.download
			f.ops.download = func(ctx context.Context, tag, asset, dir string, progress func(done, total int64)) error {
				f.checksum = func(string, string) *string { return nil }
				if err := download(ctx, tag, asset, dir, progress); err != nil {
					return err
				}
				sum, _ := sha256sum(filepath.Join(dir, asset))
				tc.write(t, dir, sum+"  "+asset+"\n")
				return nil
			}
			err := f.checker.installAndRestart(f.ops)
			if err == nil {
				t.Fatal("installed")
			}
			if f.smokeCalls != 0 || f.execCalls != 0 {
				t.Errorf("an unsigned download was run: smoke=%d exec=%d", f.smokeCalls, f.execCalls)
			}
			assertFixtureFile(t, f.current, "old fixture", 0o711)
		})
	}
}

func TestDownloadedVersionMustMatchRelease(t *testing.T) {
	for _, output := range []string{"switcher 0.5.5\n", "", "0.5.6", "switcher 0.5.6\nextra output"} {
		t.Run(fmt.Sprintf("output=%q", output), func(t *testing.T) {
			f := newInstallFixture(t)
			f.version = output
			if err := f.checker.installAndRestart(f.ops); err == nil {
				t.Error("wrong version was accepted")
			}
			if f.execCalls != 0 {
				t.Error("wrong version reached restart hook")
			}
			assertFixtureFile(t, f.current, "old fixture", 0o711)
		})
	}
}

func TestReplacementStagesBesideCurrentAndKeepsOriginalPresent(t *testing.T) {
	f := newInstallFixture(t)
	f.ops.rename = func(from, to string) error {
		if from == f.current {
			return errors.New("original executable must remain in place before atomic replacement")
		}
		if _, err := os.Stat(f.current); err != nil {
			t.Errorf("original executable disappeared: %v", err)
		}
		if to == f.current && filepath.Dir(filepath.Dir(from)) != filepath.Dir(f.current) {
			return syscall.EXDEV
		}
		return os.Rename(from, to)
	}
	if err := f.checker.installAndRestart(f.ops); err != nil {
		t.Fatal(err)
	}
	if f.requestedTemp != filepath.Dir(f.current) {
		t.Errorf("staging parent=%q, want %q", f.requestedTemp, filepath.Dir(f.current))
	}
	if _, err := os.Stat(f.stage); !os.IsNotExist(err) {
		t.Errorf("staging directory not cleaned: %v", err)
	}
	assertFixtureFile(t, f.current, "new fixture", 0o755)
}

func TestExecFailureRestoresOriginal(t *testing.T) {
	f := newInstallFixture(t)
	f.ops.exec = func(path string) error {
		assertFixtureFile(t, path, "new fixture", 0o755)
		return syscall.EACCES
	}
	if err := f.checker.installAndRestart(f.ops); !errors.Is(err, syscall.EACCES) {
		t.Errorf("error=%v, want EACCES", err)
	}
	assertFixtureFile(t, f.current, "old fixture", 0o711)
}

func TestBinaryInstallReportsEachStepThenClears(t *testing.T) {
	f := newInstallFixture(t)
	var steps []string
	step := func() {
		if p := f.checker.State().Install; p != nil {
			steps = append(steps, fmt.Sprintf("%s %.1f", p.Step, p.Fraction))
		} else {
			steps = append(steps, "none")
		}
	}
	download, smoke, rename, exec := f.ops.download, f.ops.smoke, f.ops.rename, f.ops.exec
	f.ops.download = func(ctx context.Context, tag, asset, dir string, progress func(done, total int64)) error {
		progress(1, 2)
		step()
		return download(ctx, tag, asset, dir, progress)
	}
	f.ops.smoke = func(ctx context.Context, path string) ([]byte, error) { step(); return smoke(ctx, path) }
	renames := 0
	f.ops.rename = func(from, to string) error {
		if renames++; renames == 1 {
			step()
		}
		return rename(from, to)
	}
	f.ops.exec = func(path string) error { step(); return exec(path) }
	if err := f.checker.installAndRestart(f.ops); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(steps, ", "), "downloading 0.4, checking 0.8, installing 0.9, restarting 1.0"; got != want {
		t.Errorf("steps = %s, want %s", got, want)
	}
	if f.checker.State().Install != nil {
		t.Error("progress outlived the install")
	}

	failed := newInstallFixture(t)
	failed.ops.exec = func(string) error { return syscall.EACCES }
	if err := failed.checker.installAndRestart(failed.ops); !errors.Is(err, syscall.EACCES) || failed.checker.State().Install != nil {
		t.Errorf("a failed restart left progress behind: %v", err)
	}
}

func TestStageIsCleanedBeforeExec(t *testing.T) {
	f := newInstallFixture(t)
	f.ops.exec = func(string) error {
		// A successful syscall.Exec never runs deferred cleanup.
		if _, err := os.Stat(f.stage); !os.IsNotExist(err) {
			t.Errorf("stage still present at exec boundary: %v", err)
		}
		return nil
	}
	if err := f.checker.installAndRestart(f.ops); err != nil {
		t.Fatal(err)
	}
}

func TestReplacementFailuresPreserveOriginal(t *testing.T) {
	for _, point := range []string{"chmod", "backup", "replacement", "post-swap sync"} {
		t.Run(point, func(t *testing.T) {
			f := newInstallFixture(t)
			fault := errors.New(point + " fixture failure")
			switch point {
			case "chmod":
				f.ops.chmod = func(string, os.FileMode) error { return fault }
			case "backup", "replacement":
				f.ops.rename = func(from, to string) error {
					if point == "backup" && to == f.current+".previous" || point == "replacement" && to == f.current && from != f.current+".previous" {
						return fault
					}
					return os.Rename(from, to)
				}
			case "post-swap sync":
				calls := 0
				f.ops.syncDir = func(string) error {
					calls++
					if calls == 2 {
						return fault
					}
					return nil
				}
			}
			if err := f.checker.installAndRestart(f.ops); !errors.Is(err, fault) {
				t.Errorf("error=%v, want %v", err, fault)
			}
			assertFixtureFile(t, f.current, "old fixture", 0o711)
			if f.execCalls != 0 {
				t.Error("failed replacement reached restart hook")
			}
		})
	}
}

func TestRollbackFailureRetainsBackupAndReportsBothErrors(t *testing.T) {
	f := newInstallFixture(t)
	rollbackError := errors.New("rollback fixture failure")
	f.ops.exec = func(string) error { return syscall.EACCES }
	f.ops.rename = func(from, to string) error {
		if from == f.current+".previous" && to == f.current {
			return rollbackError
		}
		return os.Rename(from, to)
	}
	err := f.checker.installAndRestart(f.ops)
	if !errors.Is(err, syscall.EACCES) || !errors.Is(err, rollbackError) || !strings.Contains(err.Error(), f.current+".previous") {
		t.Errorf("missing failure/recovery details: %v", err)
	}
	assertFixtureFile(t, f.current+".previous", "old fixture", 0o711)
}

func TestSmokeTestHasBoundedContext(t *testing.T) {
	f := newInstallFixture(t)
	f.ops.smoke = func(ctx context.Context, path string) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 15*time.Second {
			t.Error("smoke test lacks a 15-second deadline")
		}
		return nil, context.DeadlineExceeded
	}
	if err := f.checker.installAndRestart(f.ops); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error=%v, want deadline exceeded", err)
	}
	assertFixtureFile(t, f.current, "old fixture", 0o711)
}

func TestConcurrentReplacementsAreSerialized(t *testing.T) {
	f := newInstallFixture(t)
	download := f.ops.download
	var active atomic.Int32
	f.ops.download = func(ctx context.Context, tag, asset, dir string, progress func(done, total int64)) error {
		if active.Add(1) != 1 {
			t.Error("overlapping replacements")
		}
		defer active.Add(-1)
		time.Sleep(time.Millisecond)
		return download(ctx, tag, asset, dir, progress)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.checker.installAndRestart(f.ops); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.execCalls != 4 {
		t.Errorf("completed fixture replacements=%d, want 4", f.execCalls)
	}
	assertFixtureFile(t, f.current, "new fixture", 0o755)
}

// Releases out since the last check are skipped over: the install takes the
// newest, not the one cached hours ago.
func TestAnInstallTakesTheNewestRelease(t *testing.T) {
	c := New("1.15.0")
	c.latest, c.lastCheck = "v1.16.0", time.Now()
	c.metadataFetch = func(context.Context) (string, error) { return "v1.20.0", nil }
	c.refreshForInstall(false)
	if got := c.Latest(); got != "v1.20.0" {
		t.Fatalf("installs %s", got)
	}
	// A failed check still installs the newest release known.
	c.metadataFetch = func(context.Context) (string, error) { return "", errors.New("offline") }
	c.refreshForInstall(false)
	if got := c.Latest(); got != "v1.20.0" {
		t.Fatalf("after a failed check installs %s", got)
	}
}

func TestBeforeRestartRunsRightBeforeTheRestart(t *testing.T) {
	f := newInstallFixture(t)
	var order []string
	f.checker.BeforeRestart = func() { order = append(order, "before") }
	f.ops.exec = func(string) error { order = append(order, "exec"); return nil }
	if err := f.checker.installAndRestart(f.ops); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "before,exec" {
		t.Fatalf("order: %v", order)
	}
}

// The smoke-tested binary is flushed without opening it for writing, which
// Linux refuses for a program that is running ("text file busy").
func TestSyncFileNeedsNoWriteAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "switcher")
	if err := os.WriteFile(path, []byte("binary"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := syncFile(path); err != nil {
		t.Fatalf("sync a read-only binary: %v", err)
	}
}
