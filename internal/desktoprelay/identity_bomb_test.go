package desktoprelay

import (
	"bytes"
	"compress/gzip"
	"net/http/httptest"
	goruntime "runtime"
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
}
