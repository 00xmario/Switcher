package deviceproof

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The same vector as the menu bar app's (build/macos/menubar_layout_test.swift)
// and the server's tests: the three sign alike.
func TestTheProofMatchesTheMenuBarApps(t *testing.T) {
	got := MAC(strings.Repeat("ab", 32), "POST", "/api/accounts/claude-work/activate", []byte(`{"native":true}`), "1794571200", strings.Repeat("0f", 16))
	if got != "155d3f70dc25bca1cc74e6fdf9e3b4ee689b4788b526e94966daacb563f0f535" {
		t.Fatalf("proof: %s", got)
	}
}

// A signed request carries a proof the check takes, for its own method,
// target and body only, and never the token.
func TestSignAndCheck(t *testing.T) {
	r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/x?y=1", nil)
	r.Header.Set("Authorization", "Bearer leftover")
	Sign(r, "token", []byte("body"))
	header := r.Header.Get(Header)
	if r.Header.Get("Authorization") != "" || strings.Contains(header, "token") {
		t.Fatalf("the token went along: %v", r.Header)
	}
	if _, _, ok := Check(header, "token", "POST", "/api/x?y=1", []byte("body"), time.Now()); !ok {
		t.Fatal("a fresh proof was refused")
	}
	for name, ok := range map[string]bool{
		"another token": func() bool {
			_, _, ok := Check(header, "other", "POST", "/api/x?y=1", []byte("body"), time.Now())
			return ok
		}(),
		"another method": func() bool {
			_, _, ok := Check(header, "token", "PUT", "/api/x?y=1", []byte("body"), time.Now())
			return ok
		}(),
		"another query": func() bool {
			_, _, ok := Check(header, "token", "POST", "/api/x?y=2", []byte("body"), time.Now())
			return ok
		}(),
		"another body": func() bool {
			_, _, ok := Check(header, "token", "POST", "/api/x?y=1", []byte("bodY"), time.Now())
			return ok
		}(),
		"later": func() bool {
			_, _, ok := Check(header, "token", "POST", "/api/x?y=1", []byte("body"), time.Now().Add(Window+time.Second))
			return ok
		}(),
		"no token": func() bool {
			_, _, ok := Check(header, "", "POST", "/api/x?y=1", []byte("body"), time.Now())
			return ok
		}(),
	} {
		if ok {
			t.Errorf("%s was taken", name)
		}
	}
	second := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/x?y=1", nil)
	Sign(second, "token", []byte("body"))
	if second.Header.Get(Header) == header {
		t.Fatal("two proofs share a nonce")
	}
}
