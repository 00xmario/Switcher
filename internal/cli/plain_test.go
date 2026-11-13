package cli

import (
	"bytes"
	"strings"
	"testing"
)

// Whatever a host, a paired device or the network puts in a name, the
// terminal gets text.
func TestOutputCarriesNoTerminalControl(t *testing.T) {
	hostile := "Studio\x1b]0;owned\a\x1b[2K\x1b]52;c;aGk=\a \u009b31mred ‮gnp.exe⁦ back\b\r\x00\x7f ok"
	var out bytes.Buffer
	w := plain(&out)
	n, err := w.Write([]byte("name\t" + hostile + "\nnext line é 漢 🙂\n"))
	if err != nil || n != len("name\t"+hostile+"\nnext line é 漢 🙂\n") {
		t.Fatal(n, err)
	}
	got := out.String()
	for _, bad := range []string{"\x1b", "\a", "\b", "\r", "\x00", "\x7f", "\u009b", "‮", "⁦"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q reached the terminal: %q", bad, got)
		}
	}
	if want := "name\tStudio]0;owned[2K]52;c;aGk= 31mred gnp.exe back ok\nnext line é 漢 🙂\n"; got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	// A character split across two writes arrives whole; bytes that are not
	// text are dropped.
	out.Reset()
	w = plain(&out)
	text := []byte("é漢🙂\xff\xfe!")
	for i := range text {
		if _, err := w.Write(text[i : i+1]); err != nil {
			t.Fatal(err)
		}
	}
	if out.String() != "é漢🙂!" {
		t.Fatalf("split writes: %q", out.String())
	}
	if plain(w) != w {
		t.Fatal("a plain writer was wrapped again")
	}
}

// The filter is on for every command, on both streams.
func TestRunFiltersWhatItPrints(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run([]string{"no-such\x1b[2Jcommand"}, Options{Stdout: &out, Stderr: &errOut})
	if code == exitOK || strings.Contains(errOut.String(), "\x1b") || !strings.Contains(errOut.String(), "unknown command") {
		t.Fatalf("%d %q", code, errOut.String())
	}
}
