package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"switcher/internal/store"
)

// An end date is set, shown and removed through the account; what is not a
// day from today on is refused with nothing saved.
func TestAccountEndDateRoundTrip(t *testing.T) {
	h := newAutoResetHarness(t)
	if err := h.store.Save(store.Account{ID: "fake-aaaa", Provider: "fake", Email: "a@example.com"}); err != nil {
		t.Fatal(err)
	}
	day := time.Now().AddDate(0, 0, 7).Format(time.DateOnly)
	read := func(body string) accountView {
		t.Helper()
		rec := h.patch(t, "fake-aaaa", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH %s: %d %s", body, rec.Code, rec.Body.String())
		}
		var answer struct {
			Account accountView `json:"account"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
		return answer.Account
	}
	if got := read(`{"end_date":"` + day + `"}`); got.EndDate != day || !got.Ending {
		t.Fatalf("set: %+v", got)
	}
	if got := read(`{"usage_limit":10}`); got.EndDate != day || got.UsageLimit != 10 {
		t.Fatalf("a limit change dropped the end date: %+v", got)
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format(time.DateOnly)
	for _, body := range []string{`{"end_date":"` + yesterday + `"}`, `{"end_date":"11/29/2026"}`, `{"end_date":29}`,
		`{"end_date":"` + day + `","usage_limit":96}`, `{"end_date":"","usage_limit":-1}`} {
		if rec := h.patch(t, "fake-aaaa", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("PATCH %s: %d, want 400", body, rec.Code)
		}
	}
	if got := h.api.Proxy.EndDate("fake-aaaa"); got != day {
		t.Fatalf("a refused request changed the date to %q", got)
	}
	if got := read(`{"end_date":""}`); got.EndDate != "" || got.Ending {
		t.Fatalf("removed: %+v", got)
	}
	if rec := h.patch(t, "missing-account", `{"end_date":"`+day+`"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account: %d, want 404", rec.Code)
	}
}
