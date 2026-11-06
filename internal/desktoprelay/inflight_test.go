package desktoprelay_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// A Remote Control session keeps a request open through the relay for as long
// as it lives, so counting it would leave the count above zero for good.
func TestInFlightCountsOnlyModelRequests(t *testing.T) {
	for _, tc := range []struct {
		path string
		want uint64
	}{
		{"/v1/session_ingress/session/cse_fixture/events", 0},
		{"/api/oauth/usage", 0},
		{"/v1/messages", 1},
		{"/v1/messages/count_tokens", 1},
	} {
		t.Run(tc.path, func(t *testing.T) {
			finish := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(finish) }) }
			defer release()
			cfg := fixtureConfig(t)
			cfg.Source = fixtureSource()
			cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				pr, pw := io.Pipe()
				go func() {
					stop := context.AfterFunc(r.Context(), func() { _ = pw.CloseWithError(r.Context().Err()) })
					defer stop()
					defer pw.Close()
					_, _ = io.WriteString(pw, "open\n")
					select {
					case <-finish:
					case <-r.Context().Done():
					}
				}()
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: pr}, nil
			})
			m, s := startFixture(t, cfg)
			c, br := tunnel(t, s)
			r := send(t, c, br, tc.path, `{}`, sessionA, nil)
			first := make([]byte, len("open\n"))
			if _, err := io.ReadFull(r.Body, first); err != nil {
				t.Fatal(err)
			}
			if got := m.Status().InFlight; got != tc.want {
				t.Fatalf("in flight while %s is open: %d, want %d", tc.path, got, tc.want)
			}
			release()
			if got := drain(t, r); strings.Contains(got, "open") {
				t.Fatalf("stream replayed: %q", got)
			}
			deadline := time.Now().Add(2 * time.Second)
			for m.Status().InFlight != 0 {
				if time.Now().After(deadline) {
					t.Fatal("a finished request stayed in flight")
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}
