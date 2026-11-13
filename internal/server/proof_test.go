package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"switcher/internal/deviceproof"
	"switcher/internal/settings"
)

func signed(method, target, body, token string, at time.Time, nonce string) *http.Request {
	r := httptest.NewRequest(method, "http://127.0.0.1:8787"+target, strings.NewReader(body))
	seconds := fmt.Sprint(at.Unix())
	r.Header.Set(proofHeader, "v1."+seconds+"."+nonce+"."+deviceproof.MAC(token, method, r.URL.Path+map[bool]string{true: "?" + r.URL.RawQuery}[r.URL.RawQuery != ""], []byte(body), seconds, nonce))
	return r
}

// The menu bar app signs requests with the device token instead of sending
// it. This vector is checked by the Swift side too (menubar_layout_test.swift),
// so both compute the same proof.
func TestProofVectorMatchesTheMenuBarApp(t *testing.T) {
	got := deviceproof.MAC(strings.Repeat("ab", 32), "POST", "/api/accounts/claude-work/activate", []byte(`{"native":true}`), "1794571200", strings.Repeat("0f", 16))
	if want := "155d3f70dc25bca1cc74e6fdf9e3b4ee689b4788b526e94966daacb563f0f535"; got != want {
		t.Fatalf("vector: %s, want %s", got, want)
	}
}

// A proof counts for one request, once, within its window, and only with the
// method, path and body it was made for.
func TestProofsAreBoundToTheirRequest(t *testing.T) {
	token := strings.Repeat("cd", 32)
	now := time.Now()
	nonce := func(i int) string { return fmt.Sprintf("%032x", now.UnixNano()+int64(i)) }
	good := signed("POST", "/api/accounts/a/activate?x=1", `{"native":true}`, token, now, nonce(1))
	if !deviceAuthorized(good, token) {
		t.Fatal("a good proof was refused")
	}
	if body, _ := io.ReadAll(good.Body); string(body) != `{"native":true}` {
		t.Fatalf("the handler lost the body: %q", body)
	}
	if !deviceAuthorized(signed("GET", "/api/state", "", token, now, nonce(2)), token) {
		t.Fatal("a good proof without a body was refused")
	}
	// Used once.
	again := signed("POST", "/api/accounts/a/activate?x=1", `{"native":true}`, token, now, nonce(1))
	if deviceAuthorized(again, token) {
		t.Fatal("a proof was used twice")
	}
	cases := map[string]*http.Request{
		"another token":   signed("GET", "/api/state", "", strings.Repeat("ef", 32), now, nonce(3)),
		"too old":         signed("GET", "/api/state", "", token, now.Add(-3*time.Minute), nonce(4)),
		"from the future": signed("GET", "/api/state", "", token, now.Add(3*time.Minute), nonce(5)),
	}
	moved := signed("POST", "/api/accounts/a/activate", `{}`, token, now, nonce(6))
	moved.URL.Path = "/api/accounts/b/activate"
	cases["another path"] = moved
	method := signed("POST", "/api/state", "", token, now, nonce(7))
	method.Method = "DELETE"
	cases["another method"] = method
	body := signed("PATCH", "/api/settings", `{"bind_lan":false}`, token, now, nonce(8))
	body.Body = io.NopCloser(strings.NewReader(`{"bind_lan":true}`))
	cases["another body"] = body
	for _, header := range []string{"", "v2.1.2.3", "v1.x." + nonce(9) + "." + strings.Repeat("0", 64), "v1.1." + nonce(9), "v1.1.ZZ." + strings.Repeat("0", 64)} {
		r := httptest.NewRequest("GET", "/api/state", nil)
		r.Header.Set(proofHeader, header)
		cases["malformed "+header] = r
	}
	big := signed("POST", "/api/x", strings.Repeat("a", proofBodyLimit+1), token, now, nonce(10))
	cases["a body too large"] = big
	for name, r := range cases {
		if deviceAuthorized(r, token) {
			t.Errorf("%s was taken", name)
		}
	}
	// A bearer token still works, and only with its prefix.
	bearer := httptest.NewRequest("GET", "/api/state", nil)
	bearer.Header.Set("Authorization", "Bearer "+token)
	raw := httptest.NewRequest("GET", "/api/state", nil)
	raw.Header.Set("Authorization", token)
	if !deviceAuthorized(bearer, token) || deviceAuthorized(raw, token) || deviceAuthorized(bearer, "") {
		t.Fatal("bearer handling changed")
	}
}

// With a password set, the menu bar app's signed requests get in, without
// the token ever being sent.
func TestTheGateTakesProofs(t *testing.T) {
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	token, err := prefs.EnsureDeviceToken()
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s %s", AuthKind(r), body)
	})
	gated := (&AuthGate{Store: prefs}).Wrap(next)
	r := signed("POST", "/api/accounts/a/activate", `{"native":true}`, token, time.Now(), fmt.Sprintf("%032x", time.Now().UnixNano()))
	w := httptest.NewRecorder()
	gated.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != `device {"native":true}` {
		t.Fatalf("a signed request: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	gated.ServeHTTP(w, httptest.NewRequest("GET", "/api/state", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("an unsigned request: %d", w.Code)
	}
}

// The device token counts on the local port only: on the LAN listener it
// opens nothing, as a bearer token or a proof, and a made-up proof makes the
// gate read no body before refusing.
func TestTheDeviceTokenOpensNothingOnTheLAN(t *testing.T) {
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	token, err := prefs.EnsureDeviceToken()
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, AuthKind(r)) })
	lan := (&AuthGate{Store: prefs}).WrapLAN(next)
	bearer := httptest.NewRequest("GET", "/api/state", nil)
	bearer.Header.Set("Authorization", "Bearer "+token)
	proof := signed("GET", "/api/state", "", token, time.Now(), fmt.Sprintf("%032x", time.Now().UnixNano()))
	for name, r := range map[string]*http.Request{"a bearer token": bearer, "a proof": proof} {
		r.RemoteAddr = "192.0.2.9:5000"
		w := httptest.NewRecorder()
		lan.ServeHTTP(w, r)
		if w.Code == http.StatusOK && w.Body.String() == string(AuthDevice) {
			t.Errorf("%s opened the LAN listener", name)
		}
	}
	// A body larger than a proof may cover is refused before it is read.
	read := 0
	big := httptest.NewRequest("POST", "/api/x", readCounter{&read})
	big.ContentLength = proofBodyLimit + 1
	big.Header.Set(proofHeader, "v1."+fmt.Sprint(time.Now().Unix())+"."+strings.Repeat("0", 32)+"."+strings.Repeat("0", 64))
	if validProof(big, token, time.Now()) || read != 0 {
		t.Fatalf("an oversized body was read (%d bytes) or taken", read)
	}
}

type readCounter struct{ n *int }

func (c readCounter) Read(p []byte) (int, error) {
	*c.n += len(p)
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}
