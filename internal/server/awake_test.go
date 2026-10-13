package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAwakeRoutesReportUnsupported(t *testing.T) {
	api := &API{} // no keep-awake manager, as off macOS
	mux := http.NewServeMux()
	api.Register(mux)
	for _, tc := range []struct{ method, body string }{
		{"GET", ""}, {"POST", `{"on":true,"minutes":60}`}, {"PATCH", `{"lid_closed":true}`},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(tc.method, "/api/awake", bytes.NewReader([]byte(tc.body))))
		if w.Code != http.StatusNotImplemented || !strings.Contains(w.Body.String(), "macOS") {
			t.Errorf("%s unsupported: %d %s", tc.method, w.Code, w.Body)
		}
	}
	fields := api.LocalStateFields(httptest.NewRequest("GET", "/api/state", nil))
	if _, ok := fields["awake"]; ok {
		t.Error("state must not offer keep awake where it cannot work")
	}
}
