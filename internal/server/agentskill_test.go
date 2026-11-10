package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"switcher/internal/agentskill"
	"switcher/internal/remote"
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

// skillPair is a host Switcher and a computer paired with it, each with its
// own home, served by their real handlers.
func skillPair(t *testing.T) (hostHome, clientHome string, client http.Handler, paired *remote.Client) {
	t.Helper()
	hostHome, paired, _, _ = pairedSkillHost(t)
	clientHome = t.TempDir()
	clientMux := http.NewServeMux()
	(&API{Home: clientHome, RemoteClient: paired}).Register(clientMux)
	return hostHome, clientHome, clientMux, paired
}

// pairedHost is a host Switcher with a paired computer's token.
func pairedHost(t *testing.T) (hostHome string, host *remote.Host, token string) {
	t.Helper()
	hostHome, _, host, token = pairedSkillHost(t)
	return hostHome, host, token
}

func pairedSkillHost(t *testing.T) (hostHome string, paired *remote.Client, host *remote.Host, token string) {
	t.Helper()
	hostHome = t.TempDir()
	hostMux := http.NewServeMux()
	(&API{Home: hostHome}).Register(hostMux)
	host, err := remote.NewHost(t.TempDir(), 0, hostMux)
	if err != nil {
		t.Fatal(err)
	}
	host.Quiet = true
	t.Cleanup(host.Close)
	if err := host.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	code, err := host.NewPairingCode()
	if err != nil {
		t.Fatal(err)
	}
	clientDir := t.TempDir()
	paired = remote.NewClient(clientDir, "client-key")
	if _, err := paired.Connect(context.Background(), "127.0.0.1", host.Status().Port, strings.ToLower(code.Code), "Laptop"); err != nil {
		t.Fatal(err)
	}
	var file struct {
		Token string `json:"token"`
	}
	raw, err := os.ReadFile(filepath.Join(clientDir, "client.json"))
	if err != nil || json.Unmarshal(raw, &file) != nil || file.Token == "" {
		t.Fatalf("the pairing's token: %v %s", err, raw)
	}
	return hostHome, paired, host, file.Token
}

type pairView struct {
	agentskill.Status
	Error string            `json:"error"`
	Skill *pairView         `json:"skill"`
	Host  *remote.HostSkill `json:"host"`
}

func skillCall(t *testing.T, h http.Handler, method, body string) (int, pairView) {
	t.Helper()
	target := "/api/agent-skill"
	if m, ok := strings.CutSuffix(method, " wait"); ok {
		method, target = m, target+"?wait=0"
	}
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	req.RemoteAddr, req.Host = "127.0.0.1:1", "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var v pairView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("%s %s: %d %s", method, body, rec.Code, rec.Body)
	}
	return rec.Code, v
}

// From a computer that uses a host, the skill is set up on this computer,
// on the host, or on both, and each shows in the status.
func TestAgentSkillOnTheHostFromAPairedComputer(t *testing.T) {
	hostHome, clientHome, client, paired := skillPair(t)
	installed := func(home string) bool {
		_, err := os.Stat(agentskill.Path(home))
		return err == nil
	}
	code, v := skillCall(t, client, http.MethodGet, "")
	if code != 200 || v.Installed || v.Host == nil || v.Host.Skill == nil || v.Host.Skill.Installed || v.Host.Host != paired.Status().HostName {
		t.Fatalf("status: %d %+v host %+v", code, v, v.Host)
	}
	if code, v = skillCall(t, client, http.MethodPost, `{"installed":true,"where":"host"}`); code != 200 || v.Installed || !v.Host.Skill.Current {
		t.Fatalf("install on the host: %d %+v %+v", code, v, v.Host)
	}
	if !installed(hostHome) || installed(clientHome) {
		t.Fatal("where=host wrote the wrong computer")
	}
	if code, v = skillCall(t, client, http.MethodPost, `{"installed":true,"where":"both"}`); code != 200 || !v.Current || !v.Host.Skill.Current {
		t.Fatalf("install on both: %d %+v %+v", code, v, v.Host)
	}
	if !installed(hostHome) || !installed(clientHome) {
		t.Fatal("where=both missed a computer")
	}
	// An edited copy on the host is kept unless replacing was asked for.
	if err := os.WriteFile(agentskill.Path(hostHome), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, v = skillCall(t, client, http.MethodPost, `{"installed":true,"where":"both"}`); code != http.StatusConflict || v.Error == "" || v.Skill == nil || !v.Skill.Current {
		t.Fatalf("an edited host skill was not refused: %d %+v", code, v)
	}
	if raw, _ := os.ReadFile(agentskill.Path(hostHome)); string(raw) != "mine" {
		t.Fatal("the host's edited skill was overwritten")
	}
	// Only the host replaces a copy edited there.
	if code, v = skillCall(t, client, http.MethodPost, `{"installed":true,"where":"host","replace":true}`); code != http.StatusConflict || !strings.Contains(v.Error, "replace it there") {
		t.Fatalf("replaced on the host from here: %d %+v", code, v)
	}
	if err := os.Remove(agentskill.Path(hostHome)); err != nil {
		t.Fatal(err)
	}
	if code, v = skillCall(t, client, http.MethodPost, `{"installed":false,"where":"both"}`); code != 200 || v.Installed || v.Host.Skill.Installed {
		t.Fatalf("remove from both: %d %+v %+v", code, v, v.Host)
	}
	if installed(hostHome) || installed(clientHome) {
		t.Fatal("remove left a skill behind")
	}
	if code, _ = skillCall(t, client, http.MethodPost, `{"installed":true,"where":"there"}`); code != http.StatusBadRequest {
		t.Fatalf("unknown where: %d", code)
	}
	// Asked not to wait, the host's last reading answers.
	if code, v = skillCall(t, client, http.MethodGet+" wait", ""); code != 200 || v.Host == nil || v.Host.Skill == nil || v.Host.Skill.Installed {
		t.Fatalf("cached status: %d %+v", code, v.Host)
	}
}

// A paired computer's request on the host reaches only the host, and never
// replaces a copy edited there. Requests go through the host's own front
// door with the token the pairing gave.
func TestAPairedComputerSetsUpOnlyTheHostsOwnSkill(t *testing.T) {
	hostHome, host, token := pairedHost(t)
	call := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/agent-skill", strings.NewReader(body))
		req.Header.Set(remote.DeviceHeader, token)
		rec := httptest.NewRecorder()
		host.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, body := range []string{`{"installed":true,"where":"host"}`, `{"installed":true,"where":"both"}`} {
		if code := call(body); code != http.StatusBadRequest {
			t.Errorf("%s from a paired computer: %d", body, code)
		}
	}
	if code := call(`{"installed":true,"replace":true}`); code != http.StatusForbidden {
		t.Errorf("replace from a paired computer: %d", code)
	}
	if code := call(`{"installed":true}`); code != http.StatusOK {
		t.Fatalf("install from a paired computer: %d", code)
	}
	if err := os.WriteFile(agentskill.Path(hostHome), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := call(`{"installed":true}`); code != http.StatusConflict {
		t.Fatalf("an edited copy was replaced from a paired computer: %d", code)
	}
	if code := call(`{"installed":false}`); code != http.StatusConflict {
		t.Fatalf("an edited copy was removed from a paired computer: %d", code)
	}
}

// A computer that is not paired has no host to install on, and a paired
// device's request reaches only the host it was sent to.
func TestAgentSkillHostNeedsAPairing(t *testing.T) {
	mux := http.NewServeMux()
	(&API{Home: t.TempDir()}).Register(mux)
	if code, v := skillCall(t, mux, http.MethodGet, ""); code != 200 || v.Host != nil {
		t.Fatalf("an unpaired status shows a host: %d %+v", code, v.Host)
	}
	if code, _ := skillCall(t, mux, http.MethodPost, `{"installed":true,"where":"both"}`); code != http.StatusConflict {
		t.Fatalf("installed on a host without one: %d", code)
	}
}
