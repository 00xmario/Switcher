package desktoprelay

import (
	"bytes"
	"compress/gzip"
	"net/http/httptest"
	goruntime "runtime"
	"testing"
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
