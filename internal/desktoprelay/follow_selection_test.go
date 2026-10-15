package desktoprelay_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// selectingSource is a failover source that also names the account selected
// in Switcher.
type selectingSource struct {
	*failoverSource
	mu       sync.Mutex
	selected string
}

func (s *selectingSource) Selected() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.selected
}

func (s *selectingSource) choose(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selected = id
}

func TestUnpickedConversationFollowsSwitcherSelection(t *testing.T) {
	cfg := fixtureConfig(t)
	source := &selectingSource{failoverSource: newFailoverSource(""), selected: "A"}
	cfg.Source = source
	cfg.Transport = limited(false)
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil)); !strings.HasPrefix(got, "Bearer token-A ") {
		t.Fatalf("unpicked conversation used %q, want Switcher's selection A", got)
	}
	source.choose("B")
	if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil)); !strings.HasPrefix(got, "Bearer token-B ") {
		t.Fatalf("after switching in Switcher: %q", got)
	}
	session := observed(t, m, s.ID, sessionA)
	if session.AccountID != "" || session.LastResponse == nil || session.LastResponse.AccountID != "B" {
		t.Fatalf("following the selection pinned the session or lost evidence: %+v", session)
	}
	if got := drain(t, send(t, c, br, "/api/oauth/profile", ``, sessionA, nil)); got != "Bearer caller-token " {
		t.Fatalf("control traffic must keep Desktop's login: %q", got)
	}
	// A pick still wins over the selection.
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", session.Revision); err != nil {
		t.Fatal(err)
	}
	if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil)); !strings.HasPrefix(got, "Bearer token-A ") {
		t.Fatalf("picked conversation used %q", got)
	}
}

func TestSelectionOutOfUsageHandsOverWithoutPinning(t *testing.T) {
	cfg := fixtureConfig(t)
	source := &selectingSource{failoverSource: newFailoverSource("B", "A"), selected: "A"}
	cfg.Source = source
	cfg.Transport = limited(false, "token-A")
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	resp := send(t, c, br, "/v1/messages", `{"n":1}`, sessionA, nil)
	if got := drain(t, resp); resp.StatusCode != 200 || !strings.HasPrefix(got, "Bearer token-B ") || !strings.Contains(got, `"n":1`) {
		t.Fatalf("status %d body %q; Desktop must get B's answer", resp.StatusCode, got)
	}
	if got := observed(t, m, s.ID, sessionA); got.AccountID != "" {
		t.Fatalf("hand-off pinned a conversation that follows Switcher: %+v", got)
	}
	if len(source.moves) != 1 || source.moves[0] != [2]string{"A", "B"} {
		t.Fatalf("moves %v", source.moves)
	}
	// The selection's stand-in answers next; here Switcher now selects B.
	source.choose("B")
	if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil)); !strings.HasPrefix(got, "Bearer token-B ") {
		t.Fatalf("next request %q", got)
	}
}

func TestUnavailableSelectionUsesAnotherAccountThenDesktopLogin(t *testing.T) {
	cfg := fixtureConfig(t)
	source := &selectingSource{failoverSource: newFailoverSource("B"), selected: "BROKEN"}
	cfg.Source = source
	cfg.Transport = limited(false)
	_, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil)); !strings.HasPrefix(got, "Bearer token-B ") {
		t.Fatalf("broken selection: %q, want the account with usage left", got)
	}
	source.next = ""
	resp := send(t, c, br, "/v1/messages", `{}`, sessionB, nil)
	if got := drain(t, resp); resp.StatusCode != 200 || !strings.HasPrefix(got, "Bearer caller-token ") {
		t.Fatalf("no usable account: status %d body %q, want Desktop's own login", resp.StatusCode, got)
	}
}

func TestConnectedHostKeepsItsOwnSelection(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = &selectingSource{failoverSource: newFailoverSource(""), selected: "A"}
	cfg.Transport = limited(false)
	m, s := startFixture(t, cfg)
	var gotAccount string
	m.SetRemote(func(ctx context.Context, account string, r *http.Request, body []byte) (*http.Response, bool, error) {
		gotAccount = account
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("from host"))}, true, nil
	})
	c, br := tunnel(t, s)
	if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil)); got != "from host" || gotAccount != "" {
		t.Fatalf("host got account %q (%q); it must apply its own selection", gotAccount, got)
	}
}

// laterTakeover finds no stand-in when the request is routed, but one when
// Anthropic refuses it.
type laterTakeover struct {
	*selectingSource
	calls int
}

func (s *laterTakeover) Takeover(ctx context.Context, exclude string) string {
	s.calls++
	if s.calls == 1 {
		return ""
	}
	return s.selectingSource.Takeover(ctx, exclude)
}

func TestHandOverNeverPinsAConversationFollowingSwitcher(t *testing.T) {
	cfg := fixtureConfig(t)
	source := &laterTakeover{selectingSource: &selectingSource{failoverSource: newFailoverSource("B"), selected: "BROKEN"}}
	cfg.Source = source
	cfg.Transport = limited(true, "caller-token")
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	resp := send(t, c, br, "/v1/messages", `{}`, sessionA, nil)
	if got := drain(t, resp); resp.StatusCode != 200 || !strings.HasPrefix(got, "Bearer token-B ") {
		t.Fatalf("status %d body %q", resp.StatusCode, got)
	}
	if got := observed(t, m, s.ID, sessionA); got.AccountID != "" {
		t.Fatalf("hand-off pinned a conversation that follows Switcher: %+v", got)
	}
}
