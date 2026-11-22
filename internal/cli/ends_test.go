package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEndsSetsShowsAndClears(t *testing.T) {
	day := time.Now().AddDate(0, 0, 7)
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/state":
			io.WriteString(w, strings.Replace(fakeState, `"native_active":true,`, `"native_active":true,"end_date":"`+day.Format(time.DateOnly)+`","ending":true,`, 1))
		case "PATCH /api/accounts/c2":
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(raw))
			mu.Unlock()
			var body map[string]string
			json.Unmarshal(raw, &body)
			io.WriteString(w, `{"status":"ok","account":{"id":"c2","provider":"claude","email":"home@example.com","end_date":`+
				string(mustJSON(body["end_date"]))+`,"ending":`+string(mustJSON(body["end_date"] != ""))+`}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	code, out, _ := run(t, srv.URL, "", "ends", "home", day.Format(time.DateOnly))
	if code != 0 || !strings.Contains(out, "home@example.com ends with "+day.Format("Jan 2")) || !strings.Contains(out, "uses it first") {
		t.Fatalf("set: %d %q", code, out)
	}
	code, out, _ = run(t, srv.URL, "", "ends", "home", "off", "--json")
	var answer struct {
		Account Account `json:"account"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &answer) != nil || answer.Account.EndDate != "" || answer.Account.Ending {
		t.Fatalf("off: %d %q", code, out)
	}
	if got := bodies; len(got) != 2 || got[0] != `{"end_date":"`+day.Format(time.DateOnly)+`"}` || got[1] != `{"end_date":""}` {
		t.Fatalf("requests: %v", got)
	}
	code, out, _ = run(t, srv.URL, "", "ends", "claude:work@example.com")
	if code != 0 || !strings.Contains(out, "(in 7 days), used first") {
		t.Fatalf("show: %d %q", code, out)
	}
	for _, bad := range []string{"29-11-2026", "tomorrow", "2026-02-30"} {
		if code, _, _ := run(t, srv.URL, "", "ends", "home", bad); code != 2 {
			t.Errorf("ends %q: exit %d, want 2", bad, code)
		}
	}
	code, out, _ = run(t, srv.URL, "", "accounts")
	if code != 0 || !strings.Contains(out, "ends "+day.Format("Jan 2")) {
		t.Fatalf("accounts: %q", out)
	}
}

// The days left count the end day itself: a subscription ending today has
// its last day; a paid-through day shows only while ahead.
func TestEndingText(t *testing.T) {
	now := time.Date(2026, 11, 22, 15, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		a    Account
		want string
	}{
		{Account{EndDate: "2026-11-29", Ending: true}, "ends Nov 29 (in 7 days), used first"},
		{Account{EndDate: "2026-11-22", Ending: true}, "ends Nov 22 (today), used first"},
		{Account{EndDate: "2027-01-03", Ending: true}, "ends Jan 3, 2027 (in 42 days), used first"},
		{Account{EndDate: "2026-11-20"}, "ended Nov 20"},
		{Account{PaidThrough: time.Date(2026, 12, 20, 0, 0, 0, 0, time.Local).Unix()}, "paid through Dec 20"},
		{Account{PaidThrough: now.Add(-time.Hour).Unix()}, ""},
		{Account{}, ""},
	} {
		if got := endingText(tc.a, now); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.a, got, tc.want)
		}
	}
}
