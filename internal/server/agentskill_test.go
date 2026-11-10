package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"switcher/internal/agentskill"
)

func TestAgentSkillInstallsOnlyFromThisComputer(t *testing.T) {
	home := t.TempDir()
	a := &API{Home: home}
	mux := http.NewServeMux()
	a.Register(mux)
	call := func(method, body, remote string) (int, agentskill.Status) {
		t.Helper()
		req := httptest.NewRequest(method, "/api/agent-skill", bytes.NewBufferString(body))
		req.RemoteAddr, req.Host = remote, "127.0.0.1:8787"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var st agentskill.Status
		json.Unmarshal(rec.Body.Bytes(), &st)
		return rec.Code, st
	}
	if code, st := call(http.MethodGet, "", "127.0.0.1:1"); code != 200 || st.Installed {
		t.Fatalf("status: %d %+v", code, st)
	}
	if code, _ := call(http.MethodPost, `{"installed":true}`, "192.0.2.1:1"); code != http.StatusForbidden {
		t.Fatalf("a paired device installed the skill: %d", code)
	}
	if _, err := os.Stat(agentskill.Path(home)); !os.IsNotExist(err) {
		t.Fatal("the skill was written for a remote request")
	}
	for _, bad := range []string{`{}`, `{"installed":"yes"}`, `nope`} {
		if code, _ := call(http.MethodPost, bad, "127.0.0.1:1"); code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if code, st := call(http.MethodPost, `{"installed":true}`, "127.0.0.1:1"); code != 200 || !st.Current || !st.Managed {
		t.Fatalf("install: %d %+v", code, st)
	}
	if code, st := call(http.MethodPost, `{"installed":false}`, "127.0.0.1:1"); code != 200 || st.Installed {
		t.Fatalf("remove: %d %+v", code, st)
	}
	if code, _ := call(http.MethodGet, "", "127.0.0.1:1"); code != 200 {
		t.Fatal("status after removal")
	}
	// Without a home directory there is nowhere to keep it.
	empty := http.NewServeMux()
	(&API{}).Register(empty)
	rec := httptest.NewRecorder()
	empty.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agent-skill", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("without a home: %d", rec.Code)
	}
}
