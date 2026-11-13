package desktoprelay

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http/httptest"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// A small packed body can hold gigabytes. Reading the routing fields must
// not unpack more than a request may be.
func TestIdentityDoesNotUnpackABomb(t *testing.T) {
	pack := func(write func(z *gzip.Writer)) []byte {
		var packed bytes.Buffer
		z := gzip.NewWriter(&packed)
		write(z)
		z.Close()
		return packed.Bytes()
	}
	// 1 GiB of zeros packs to about a megabyte.
	bomb := pack(func(z *gzip.Writer) {
		chunk := make([]byte, 1<<20)
		for i := 0; i < 1024; i++ {
			z.Write(chunk)
		}
	})
	if len(bomb) > 4<<20 {
		t.Fatalf("fixture is %d bytes", len(bomb))
	}
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("Content-Encoding", "gzip")
	var before, after goruntime.MemStats
	goruntime.GC()
	goruntime.ReadMemStats(&before)
	view := identity(r, bomb)
	goruntime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 6*maxRequestBytes {
		t.Fatalf("unpacking allocated %d MiB", grew>>20)
	}
	if view.Model != "" || view.Thread != "" {
		t.Fatalf("an oversized body was read: %+v", view)
	}
	// An ordinary packed request is read as before.
	packed := pack(func(z *gzip.Writer) {
		z.Write([]byte(`{"model":"claude-fable-5-2","thread":{"type":"continue"},"metadata":{"user_id":"{\"session_id\":\"0f8fad5b-d9cb-469f-a165-70867728950e\"}"}}`))
	})
	view = identity(r, packed)
	if view.Model != "claude-fable-5-2" || view.Thread != "continue" || view.SessionID != "0f8fad5b-d9cb-469f-a165-70867728950e" {
		t.Fatalf("packed request: %+v", view)
	}
}

// Many small packed bodies at once must not add up either: only a few are
// unpacked at a time, and the rest wait. Plain bodies do not wait.
func TestPackedBodiesAreUnpackedAFewAtATime(t *testing.T) {
	if cap(unpacking) > 4 {
		t.Fatalf("%d requests may be unpacked at once", cap(unpacking))
	}
	var packed bytes.Buffer
	z := gzip.NewWriter(&packed)
	z.Write([]byte(`{"model":"m"}`))
	z.Close()
	for i := 0; i < cap(unpacking); i++ {
		unpacking <- struct{}{} // as many unpacking as may
	}
	plain := httptest.NewRequest("POST", "/v1/messages", nil)
	if identity(plain, []byte(`{"model":"plain"}`)).Model != "plain" {
		t.Fatal("a plain body waited or was not read")
	}
	done := make(chan string, 1)
	go func() {
		r := httptest.NewRequest("POST", "/v1/messages", nil)
		r.Header.Set("Content-Encoding", "gzip")
		done <- identity(r, packed.Bytes()).Model
	}()
	select {
	case <-done:
		t.Fatal("one more packed body was unpacked beyond the bound")
	case <-time.After(150 * time.Millisecond):
	}
	for i := 0; i < cap(unpacking); i++ {
		<-unpacking
	}
	select {
	case model := <-done:
		if model != "m" {
			t.Fatalf("the waiting body was not read: %q", model)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting body never got its turn")
	}
	if len(unpacking) != 0 {
		t.Fatal("a turn was not given back")
	}
	// A paired Mac's requests wait for turns of their own: all of them
	// taken does not hold this computer's own requests up.
	for i := 0; i < cap(unpackingPaired); i++ {
		unpackingPaired <- struct{}{}
	}
	own := httptest.NewRequest("POST", "/v1/messages", nil)
	own.Header.Set("Content-Encoding", "gzip")
	if identity(own, packed.Bytes()).Model != "m" {
		t.Fatal("this computer's own packed request was not read")
	}
	// A request that was given up on does not wait, or take a turn later.
	ctx, cancel := context.WithCancel(context.Background())
	gone := httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)
	gone.Header.Set("Content-Encoding", "gzip")
	cancel()
	answered := make(chan string, 1)
	go func() { answered <- identityIn(unpackingPaired, gone, packed.Bytes()).Model }()
	select {
	case model := <-answered:
		if model != "" {
			t.Fatalf("an abandoned request was unpacked: %q", model)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an abandoned request kept waiting for a turn")
	}
	for i := 0; i < cap(unpackingPaired); i++ {
		<-unpackingPaired
	}
}

// A request larger than one may be is answered as Anthropic answers it,
// not by a dropped connection the caller would retry.
func TestOversizedRequestIsAnsweredNotDropped(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/messages", io.LimitReader(zeros{}, maxRequestBytes+1))
	body, ok := readRequest(w, r)
	if ok || body != nil || w.Code != 413 || !bytes.Contains(w.Body.Bytes(), []byte(`"request_too_large"`)) {
		t.Fatalf("oversized request: ok=%v status=%d body=%s", ok, w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	body, ok = readRequest(w, httptest.NewRequest("POST", "/v1/messages", bytes.NewReader([]byte(`{"model":"m"}`))))
	if !ok || string(body) != `{"model":"m"}` {
		t.Fatalf("ordinary request: %v %q", ok, body)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// Routing fields far longer than Claude Code's are not decoded or
// rewritten: decoding text copies it, several times over for bytes that
// are not text, and a body full of one field would multiply in memory.
func TestOversizedRoutingFieldsAreNotDecoded(t *testing.T) {
	huge := strings.Repeat("\xff", 8<<20) // 8 MiB of bytes that are not text
	body := []byte(`{"model":"m","metadata":{"user_id":"` + huge + `"},"thread":{"type":"continue"}}`)
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	var before, after goruntime.MemStats
	goruntime.GC()
	goruntime.ReadMemStats(&before)
	view := identity(r, body)
	rewritten := withAccountUUID(body, "0f8fad5b-d9cb-469f-a165-70867728950e")
	goruntime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 3*uint64(len(body)) {
		t.Fatalf("reading an %d MiB field allocated %d MiB", len(huge)>>20, grew>>20)
	}
	if view.Model != "m" || view.Thread != "continue" || view.SessionID != "" {
		t.Fatalf("the other fields: %+v", view)
	}
	if &rewritten[0] != &body[0] || len(rewritten) != len(body) {
		t.Fatal("a body with an oversized user_id was rewritten")
	}
	// A long model name or thread type is no model or thread type.
	long := identity(r, []byte(`{"model":"`+strings.Repeat("m", maxFieldBytes+1)+`","thread":{"type":"`+strings.Repeat("c", maxFieldBytes+1)+`"}}`))
	if long.Model != "" || long.Thread != "create" {
		t.Fatalf("oversized fields were read: model %d bytes, thread %q", len(long.Model), long.Thread)
	}
	// Claude Code's own request is read and rewritten as before.
	own := []byte(`{"model":"claude-fable-5-2","metadata":{"user_id":"{\"session_id\":\"0f8fad5b-d9cb-469f-a165-70867728950e\",\"account_uuid\":\"11111111-2222-3333-4444-555555555555\"}"}}`)
	view = identity(r, own)
	if view.Model != "claude-fable-5-2" || view.AccountUUID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("an ordinary request: %+v", view)
	}
	if out := withAccountUUID(own, "99999999-2222-3333-4444-555555555555"); !bytes.Contains(out, []byte("99999999-2222")) {
		t.Fatalf("an ordinary request was not rewritten: %s", out)
	}
}
