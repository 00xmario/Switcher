package tray

import "testing"

func TestNotificationTextIsPlain(t *testing.T) {
	got := notificationText("reset for <a href=\"https://attacker.example\">you@example.com</a> & <img src=x>\x1b[2J‮")
	want := "reset for &lt;a href=\"https://attacker.example\"&gt;you@example.com&lt;/a&gt; &amp; &lt;img src=x&gt;[2J"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if notificationText("Codex usage window") != "Codex usage window" {
		t.Fatal("plain text changed")
	}
}
