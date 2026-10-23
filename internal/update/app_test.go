package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// appFixture models an installed Switcher.app and a release disk image as
// plain folders. Every effect goes through appOps; nothing is mounted,
// signed, executed or restarted.
type appFixture struct {
	checker     *Checker
	ops         appOps
	bundle      string
	exe         string
	imageApp    string // what the "disk image" holds
	imageBytes  []byte
	sum         string
	restarts    []string
	adminSwaps  int
	downloads   int
	writable    bool
	versionSeen map[string]string
}

func writeApp(t *testing.T, app, version string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"Contents/Info.plist":           version,
		"Contents/MacOS/Switcher":       "menu " + version,
		"Contents/MacOS/SwitcherServer": "switcher " + version,
	} {
		if err := os.WriteFile(filepath.Join(app, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func readFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func newAppFixture(t *testing.T) *appFixture {
	t.Helper()
	root := t.TempDir()
	f := &appFixture{checker: New("1.11.0"), writable: true,
		bundle:     filepath.Join(root, "Applications", "Switcher.app"),
		imageApp:   filepath.Join(root, "image", "Switcher.app"),
		imageBytes: []byte("disk image fixture")}
	f.checker.latest = "v1.12.0"
	f.checker.lastCheck = time.Now() // so State() never starts a real check
	f.exe = filepath.Join(f.bundle, "Contents", "MacOS", "SwitcherServer")
	writeApp(t, f.bundle, "1.11.0")
	writeApp(t, f.imageApp, "1.12.0")
	sum := sha256.Sum256(f.imageBytes)
	f.sum = hex.EncodeToString(sum[:]) + "  Switcher_1.12.0.dmg\n"
	tmp := filepath.Join(root, "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	f.ops = appOps{
		tempDir: func(dir, pattern string) (string, error) {
			if dir == "" {
				dir = tmp
			}
			return os.MkdirTemp(dir, pattern)
		},
		download: func(ctx context.Context, tag, asset, dir string, progress func(done, total int64)) error {
			f.downloads++
			if tag != "v1.12.0" || asset != "Switcher_1.12.0.dmg" {
				t.Errorf("download %s %s", tag, asset)
			}
			if err := os.WriteFile(filepath.Join(dir, asset), f.imageBytes, 0o600); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, asset+".sha256"), []byte(f.sum), 0o600)
		},
		attach: func(ctx context.Context, image, mountpoint string) error {
			if data, _ := os.ReadFile(image); string(data) != string(f.imageBytes) {
				t.Errorf("attached an unverified image")
			}
			return copyTree(f.imageApp, filepath.Join(mountpoint, "Switcher.app"))
		},
		detach: func(string) error { return nil },
		version: func(ctx context.Context, app string) (string, error) {
			data, err := os.ReadFile(filepath.Join(app, "Contents", "Info.plist"))
			return string(data), err
		},
		verify: func(ctx context.Context, app string) error { return nil },
		smoke: func(ctx context.Context, server string) ([]byte, error) {
			return os.ReadFile(server)
		},
		copyApp:   func(ctx context.Context, from, to string) error { return copyTree(from, to) },
		writable:  func(string) bool { return f.writable },
		rename:    os.Rename,
		removeAll: os.RemoveAll,
		adminSwap: func(ctx context.Context, from, current string) error {
			f.adminSwaps++
			if err := os.RemoveAll(current); err != nil {
				return err
			}
			return copyTree(from, current)
		},
		restart: func(bundle, exe string) error {
			f.restarts = append(f.restarts, bundle)
			return nil
		},
	}
	return f
}

func copyTree(from, to string) error {
	return filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		target := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func (f *appFixture) install() error { return f.checker.installApp(f.bundle, f.exe, f.ops) }

func (f *appFixture) assertUntouched(t *testing.T) {
	t.Helper()
	if got := readFixture(t, filepath.Join(f.bundle, "Contents", "Info.plist")); got != "1.11.0" {
		t.Errorf("installed app is %q, want the original 1.11.0", got)
	}
	if len(f.restarts) != 0 {
		t.Errorf("restarted after a failed install: %v", f.restarts)
	}
}

func TestAppBundleFromExecutable(t *testing.T) {
	for exe, want := range map[string]string{
		"/Applications/Switcher.app/Contents/MacOS/SwitcherServer":          "/Applications/Switcher.app",
		"/Users/me/Applications/Switcher.app/Contents/MacOS/SwitcherServer": "/Users/me/Applications/Switcher.app",
		"/usr/local/bin/switcher":                                           "",
	} {
		got, ok := appBundle(exe)
		if got != want || ok != (want != "") {
			t.Errorf("appBundle(%q) = %q, %v", exe, got, ok)
		}
	}
}

func TestAppInstallReplacesTheWholeAppAndRestarts(t *testing.T) {
	f := newAppFixture(t)
	if err := f.install(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"Contents/Info.plist":           "1.12.0",
		"Contents/MacOS/Switcher":       "menu 1.12.0",
		"Contents/MacOS/SwitcherServer": "switcher 1.12.0",
	} {
		if got := readFixture(t, filepath.Join(f.bundle, name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if len(f.restarts) != 1 || f.restarts[0] != f.bundle {
		t.Errorf("restarts = %v", f.restarts)
	}
	entries, _ := os.ReadDir(filepath.Dir(f.bundle))
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("Applications holds %v; staging and backup must be gone", names)
	}
	if f.adminSwaps != 0 {
		t.Error("a writable folder must not ask for an administrator")
	}
}

func TestAppInstallReportsEachStepAndRefusesASecondInstall(t *testing.T) {
	f := newAppFixture(t)
	var steps []string
	step := func() *Progress {
		install := f.checker.State().Install
		if install == nil {
			t.Fatal("no install progress while installing")
		}
		steps = append(steps, install.Step)
		return install
	}
	download, attach, copyApp, restart := f.ops.download, f.ops.attach, f.ops.copyApp, f.ops.restart
	f.ops.download = func(ctx context.Context, tag, asset, dir string, progress func(done, total int64)) error {
		progress(5, 20)
		if got := step(); got.Fraction != downloadShare/4 || got.Done != 5 || got.Total != 20 {
			t.Errorf("a quarter downloaded reports %+v", got)
		}
		progress(9, -1) // a server that does not say the size
		if got := step(); got.Fraction != downloadShare/4 || got.Done != 9 || got.Total != 0 {
			t.Errorf("an unknown size moved the bar: %+v", got)
		}
		return download(ctx, tag, asset, dir, progress)
	}
	f.ops.attach = func(ctx context.Context, image, mountpoint string) error {
		if got := step(); got.Fraction != downloadShare {
			t.Errorf("checking reports %+v", got)
		}
		return attach(ctx, image, mountpoint)
	}
	f.ops.copyApp = func(ctx context.Context, from, to string) error {
		if got := step(); got.Fraction != installingShare {
			t.Errorf("installing reports %+v", got)
		}
		return copyApp(ctx, from, to)
	}
	f.ops.restart = func(bundle, exe string) error {
		if got := step(); got.Fraction != 1 {
			t.Errorf("restarting reports %+v", got)
		}
		return restart(bundle, exe)
	}
	if err := f.install(); err != nil {
		t.Fatal(err)
	}
	want := []string{"downloading", "downloading", "checking", "installing", "restarting"}
	if strings.Join(steps, " ") != strings.Join(want, " ") {
		t.Errorf("steps = %v, want %v", steps, want)
	}
	if got := f.checker.State().Install; got == nil || got.Step != "restarting" || got.Fraction != 1 {
		t.Errorf("a finished install must keep reporting its restart, got %+v", got)
	}
	if err := f.install(); err == nil || !strings.Contains(err.Error(), "restarting") || f.downloads != 1 || len(f.restarts) != 1 {
		t.Errorf("a second install while Switcher restarts must be refused before downloading: %v, downloads=%d", err, f.downloads)
	}

	failed := newAppFixture(t)
	failed.ops.verify = func(context.Context, string) error { return errors.New("fixture signature") }
	if err := failed.install(); err == nil || failed.checker.State().Install != nil {
		t.Errorf("a failed install left progress behind: %v", err)
	}
}

func TestAppInstallRefusesAMismatchedOrDamagedRelease(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*appFixture)
		want  string
	}{
		{"checksum", func(f *appFixture) { f.sum = strings.Repeat("0", 64) + "  Switcher_1.12.0.dmg\n" }, "checksum mismatch"},
		{"bundle version", func(f *appFixture) {
			_ = os.WriteFile(filepath.Join(f.imageApp, "Contents", "Info.plist"), []byte("1.11.0"), 0o644)
		}, "not release 1.12.0"},
		{"server version", func(f *appFixture) {
			_ = os.WriteFile(filepath.Join(f.imageApp, "Contents", "MacOS", "SwitcherServer"), []byte("switcher 1.0.0"), 0o755)
		}, "reports"},
		{"signature", func(f *appFixture) {
			f.ops.verify = func(context.Context, string) error { return errors.New("invalid signature") }
		}, "signature"},
		{"no app", func(f *appFixture) { _ = os.RemoveAll(f.imageApp); _ = os.MkdirAll(f.imageApp+"-other", 0o755) }, "no Switcher.app"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAppFixture(t)
			if tc.name == "no app" {
				f.ops.attach = func(context.Context, string, string) error { return nil }
			}
			tc.setup(f)
			err := f.install()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			f.assertUntouched(t)
		})
	}
}

func TestAppInstallRestoresTheOriginalWhenTheSwapFails(t *testing.T) {
	f := newAppFixture(t)
	f.ops.rename = func(from, to string) error {
		if to == f.bundle && strings.Contains(from, ".switcher-update-") {
			return errors.New("fixture rename failure")
		}
		return os.Rename(from, to)
	}
	err := f.install()
	if err == nil || !strings.Contains(err.Error(), "put the new app in place") {
		t.Fatalf("error = %v", err)
	}
	f.assertUntouched(t)
}

func TestAppInstallRefusesTranslocatedAndDiskImageCopies(t *testing.T) {
	f := newAppFixture(t)
	err := f.checker.installApp("/private/var/folders/x/AppTranslocation/ABC/d/Switcher.app", f.exe, f.ops)
	if err == nil || !strings.Contains(err.Error(), "Move Switcher.app to Applications") || f.downloads != 0 {
		t.Errorf("translocated: err=%v downloads=%d", err, f.downloads)
	}
	f.writable = false
	err = f.checker.installApp("/Volumes/Switcher 1.11.0/Switcher.app", f.exe, f.ops)
	if err == nil || !strings.Contains(err.Error(), "disk image") || f.downloads != 0 {
		t.Errorf("disk image: err=%v downloads=%d", err, f.downloads)
	}
}

func TestAppInstallAsksForAnAdministratorOnlyWhenTheFolderIsLocked(t *testing.T) {
	f := newAppFixture(t)
	f.writable = false
	if err := f.install(); err != nil {
		t.Fatal(err)
	}
	if f.adminSwaps != 1 || readFixture(t, filepath.Join(f.bundle, "Contents", "Info.plist")) != "1.12.0" {
		t.Errorf("admin swaps=%d", f.adminSwaps)
	}
}

func TestAppInstallNeedsANewerRelease(t *testing.T) {
	f := newAppFixture(t)
	f.checker.latest = "v1.11.0"
	if err := f.install(); err == nil || f.downloads != 0 {
		t.Errorf("err=%v downloads=%d", err, f.downloads)
	}
}

func TestAdminScriptQuotesPaths(t *testing.T) {
	quoted := shellQuote("/Applications/it's here/Switcher.app")
	if quoted != `'/Applications/it'\''s here/Switcher.app'` {
		t.Errorf("shellQuote = %s", quoted)
	}
	if got := appleScriptEscape(`a "b" \c`); got != `a \"b\" \\c` {
		t.Errorf("appleScriptEscape = %s", got)
	}
}

// withReleaseServer points the release and API hosts at a fixture server.
func withReleaseServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	oldRelease, oldAPI := releaseHost, apiHost
	releaseHost, apiHost = server.URL, server.URL
	t.Cleanup(func() { releaseHost, apiHost = oldRelease, oldAPI })
}

func TestLatestReleaseFromTheAPIThenTheReleasesPage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		apiCode  int
		location string
		want     string
		wantErr  bool
	}{
		{"api", 200, "", "v1.12.0", false},
		{"rate limited", 403, "/" + Repo + "/releases/tag/v1.13.0", "v1.13.0", false},
		{"neither", 403, "/" + Repo + "/releases", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("User-Agent") != userAgent {
					t.Errorf("missing user agent on %s", r.URL)
				}
				switch r.URL.Path {
				case "/repos/" + Repo + "/releases/latest":
					w.WriteHeader(tc.apiCode)
					_, _ = w.Write([]byte(`{"tag_name":"v1.12.0","assets":[]}`))
				case "/" + Repo + "/releases/latest":
					http.Redirect(w, r, tc.location, http.StatusFound)
				default:
					http.NotFound(w, r)
				}
			})
			got, err := latestReleaseTag(context.Background(), &http.Client{})
			if string(got) != tc.want || (err != nil) != tc.wantErr {
				t.Errorf("tag=%q err=%v", got, err)
			}
		})
	}
}

func TestDownloadReleaseFetchesAssetAndChecksum(t *testing.T) {
	withReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + Repo + "/releases/download/v1.12.0/Switcher_1.12.0.dmg":
			_, _ = w.Write([]byte("image"))
		case "/" + Repo + "/releases/download/v1.12.0/Switcher_1.12.0.dmg.sha256":
			_, _ = w.Write([]byte("sum"))
		default:
			http.NotFound(w, r)
		}
	})
	dir := t.TempDir()
	var reports [][2]int64
	if err := downloadRelease(context.Background(), "v1.12.0", "Switcher_1.12.0.dmg", dir, 1<<20, func(done, total int64) {
		reports = append(reports, [2]int64{done, total})
	}); err != nil {
		t.Fatal(err)
	}
	if len(reports) < 2 || reports[0] != [2]int64{0, 5} || reports[len(reports)-1] != [2]int64{5, 5} {
		t.Errorf("progress reports %v, want the image's 5 bytes from 0 to 5", reports)
	}
	if readFixture(t, filepath.Join(dir, "Switcher_1.12.0.dmg")) != "image" ||
		readFixture(t, filepath.Join(dir, "Switcher_1.12.0.dmg.sha256")) != "sum" {
		t.Error("downloaded files differ")
	}
	if err := downloadRelease(context.Background(), "v1.12.0", "missing", t.TempDir(), 1<<20, nil); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("missing asset: %v", err)
	}
	if err := downloadRelease(context.Background(), "v1.12.0", "Switcher_1.12.0.dmg", t.TempDir(), 2, nil); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("oversized asset: %v", err)
	}
	if err := downloadRelease(context.Background(), "--help", "x", t.TempDir(), 2, nil); err == nil {
		t.Error("a malformed tag must not reach a URL")
	}
}

func TestRelaunchScriptStopsBothProcessesThenOpensTheApp(t *testing.T) {
	var pids []string
	for range 2 {
		cmd := exec.Command("/bin/sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
		pids = append(pids, strconv.Itoa(cmd.Process.Pid))
		go func() { _, _ = cmd.Process.Wait() }() // reap, so kill -0 sees it gone
	}
	script := strings.Replace(relaunchScript, "exec /usr/bin/open", "exec echo opened", 1)
	script = strings.Replace(script, "sleep 1\n", "", 1)
	out, err := exec.Command("/bin/sh", "-c", script, "switcher-relaunch", "/Applications/Switcher.app", pids[0], pids[1]).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "opened /Applications/Switcher.app" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	for _, pid := range pids {
		if exec.Command("/bin/kill", "-0", pid).Run() == nil {
			t.Errorf("process %s still runs", pid)
		}
	}
}
