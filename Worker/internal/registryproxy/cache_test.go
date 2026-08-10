package registryproxy

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// digest64 builds a valid 64-hex-char digest: "sha256:" + 64 hex chars.
func digest64(hex string) string {
	for len(hex) < 64 {
		hex = hex + "0"
	}
	return "sha256:" + hex[:64]
}

func TestCachePutGet(t *testing.T) {
	c, err := OpenBlobCache(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("OpenBlobCache: %v", err)
	}
	d := digest64("a1")
	payload := []byte("layer-bytes")

	w, err := c.PutWriter(d)
	if err != nil {
		t.Fatalf("PutWriter: %v", err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Publish(); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	size, rc, ok := c.Get(d)
	if !ok {
		t.Fatal("Get after publish: miss")
	}
	defer rc.Close()
	if size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", size, len(payload))
	}
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
}

func TestCacheMissAndInvalidDigest(t *testing.T) {
	c, err := OpenBlobCache(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("OpenBlobCache: %v", err)
	}
	if _, _, ok := c.Get(digest64("ff")); ok {
		t.Fatal("Get on empty cache should miss")
	}
	// Unverified digest must never be accepted as a key (path traversal guard).
	if _, _, ok := c.Get("../../etc/passwd"); ok {
		t.Fatal("invalid digest must miss")
	}
	if _, err := c.PutWriter("../evil"); err == nil {
		t.Fatal("PutWriter must reject invalid digest")
	}
}

func TestCacheUnpublishedWriterLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenBlobCache(dir, 0)
	if err != nil {
		t.Fatalf("OpenBlobCache: %v", err)
	}
	d := digest64("b2")
	w, _ := c.PutWriter(d)
	_, _ = w.Write([]byte("partial"))
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, _, ok := c.Get(d); ok {
		t.Fatal("unpublished blob must not be visible")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "blobs", "sha256", d[7:]))
	if len(entries) != 0 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestCacheLRUEviction(t *testing.T) {
	dir := t.TempDir()
	// Cap such that two blobs fit (40 ≤ 45) but three exceed it; eviction
	// trims to half the cap (22.5), dropping the oldest ones.
	c, err := OpenBlobCache(dir, 45)
	if err != nil {
		t.Fatalf("OpenBlobCache: %v", err)
	}
	payload := bytes.Repeat([]byte("x"), 20)
	d1, d2, d3 := digest64("c1"), digest64("c2"), digest64("c3")

	put := func(d string) {
		t.Helper()
		w, _ := c.PutWriter(d)
		_, _ = w.Write(payload)
		if err := w.Publish(); err != nil {
			t.Fatalf("publish %s: %v", d, err)
		}
	}
	// has reports cached presence, always closing the returned reader so the
	// file handle is released (eviction deletes files; Windows cannot remove
	// an open file).
	has := func(d string) bool {
		t.Helper()
		_, rc, ok := c.Get(d)
		if rc != nil {
			_ = rc.Close()
		}
		return ok
	}
	put(d1) // 20
	put(d2) // 40 ≤ 45 → no eviction
	if !has(d1) {
		t.Fatal("d1 should still be cached")
	}
	if !has(d2) {
		t.Fatal("d2 should still be cached")
	}
	put(d3) // 60 > 45 → evict oldest until ≤ 22.5 → d1, then d2
	if has(d1) {
		t.Fatal("d1 should have been evicted")
	}
	if has(d2) {
		t.Fatal("d2 should have been evicted")
	}
	if !has(d3) {
		t.Fatal("d3 should remain")
	}
}

func TestBlobDigestFromPath(t *testing.T) {
	d := digest64("ab")
	cases := []struct {
		path string
		want string
		ok   bool
	}{
		{"/v2/library/data-server/blobs/" + d, d, true},
		{"/v2/ns/myapp/blobs/" + d + "?query=1", d, true},
		{"/v2/library/x/manifests/latest", "", false},
		{"/v2/_catalog", "", false},
		{"/v2/library/x/blobs/notadigest", "", false},
		{"/v2", "", false},
	}
	for _, tc := range cases {
		got, ok := blobDigestFromPath(tc.path)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("blobDigestFromPath(%q) = %q, %v; want %q, %v", tc.path, got, ok, tc.want, tc.ok)
		}
	}
}
