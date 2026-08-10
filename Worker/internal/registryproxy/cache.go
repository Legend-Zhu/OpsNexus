package registryproxy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// blobDigestRe matches a content-addressable blob key (the registry /v2 path
// segment after "blobs/"). Only verified digests are ever used as cache keys,
// so untrusted URL input cannot escape the cache directory.
var blobDigestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// BlobCache is a content-addressed disk cache for registry blob pulls. Blobs
// are stored as <dir>/blobs/sha256/<hex>/data (same layout as the server-side
// embedded registry), keyed by their digest — content-addressable storage needs
// no invalidation. The worker side of the registry relay caches the first pull
// of each layer so subsequent pulls by other cluster nodes (and re-pulls) are
// served locally without crossing the tunnel.
//
// Access is LRU-flavored: hits touch the mtime; when the total cache size
// exceeds maxBytes, the least-recently-touched blobs are evicted on the next
// Commit. Eviction is best-effort (never fails a pull).
type BlobCache struct {
	dir      string
	maxBytes int64

	mu sync.Mutex
}

// OpenBlobCache creates (or opens) the cache root. maxBytes <= 0 disables
// eviction (unbounded cache).
func OpenBlobCache(dir string, maxBytes int64) (*BlobCache, error) {
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		return nil, fmt.Errorf("registry cache init: %w", err)
	}
	return &BlobCache{dir: dir, maxBytes: maxBytes}, nil
}

// validDigest reports whether d is a safe, addressable cache key.
func validDigest(d string) bool { return blobDigestRe.MatchString(d) }

// blobPath returns the on-disk path for a verified digest.
func (c *BlobCache) blobPath(digest string) string {
	return filepath.Join(c.dir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"), "data")
}

// Get returns the cached blob body (if present) together with its size. The
// caller owns the returned reader and must Close it. A hit refreshes the
// mtime (LRU). ok=false when the blob is not cached.
func (c *BlobCache) Get(digest string) (size int64, r io.ReadCloser, ok bool) {
	if !validDigest(digest) {
		return 0, nil, false
	}
	path := c.blobPath(digest)

	c.mu.Lock()
	defer c.mu.Unlock()
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return 0, nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, false
	}
	// Best-effort LRU touch; failures (read-only fs) just skip refresh.
	_ = os.Chtimes(path, time.Now(), time.Now())
	return fi.Size(), f, true
}

// PutWriter opens a temporary writer for digest. The caller streams the blob
// into it, then calls Publish on the returned writer to atomically publish it
// (and run eviction); calling Close instead (or returning without Publish)
// discards the temp file, so a half-fetched blob never appears as cached.
func (c *BlobCache) PutWriter(digest string) (*cacheTmpWriter, error) {
	if !validDigest(digest) {
		return nil, fmt.Errorf("invalid blob digest: %q", digest)
	}
	target := c.blobPath(digest)
	blobDir := filepath.Dir(target)
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		return nil, fmt.Errorf("registry cache mkdir: %w", err)
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	tmp := filepath.Join(blobDir, ".tmp-"+hex.EncodeToString(b))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("registry cache temp create: %w", err)
	}
	return &cacheTmpWriter{f: f, cache: c, target: target, tmp: tmp}, nil
}

// cacheTmpWriter is the io.WriteCloser over the temp blob file. Publish
// flushes + atomically renames it into place; Close discards it. Either is
// idempotent, so a handler can Publish on full EOF and defer Close safely.
type cacheTmpWriter struct {
	f      *os.File
	cache  *BlobCache
	target string // final data path
	tmp    string
	done   bool
}

func (w *cacheTmpWriter) Write(p []byte) (int, error) { return w.f.Write(p) }

// Publish atomically publishes the blob and enforces the size cap.
func (w *cacheTmpWriter) Publish() error {
	if w.done {
		return nil
	}
	w.done = true
	if err := w.f.Close(); err != nil {
		_ = os.Remove(w.tmp)
		return err
	}
	if err := os.Rename(w.tmp, w.target); err != nil {
		_ = os.Remove(w.tmp)
		return err
	}
	w.cache.mu.Lock()
	w.cache.enforceLimitLocked()
	w.cache.mu.Unlock()
	return nil
}

// Close discards the temp file unless already published.
func (w *cacheTmpWriter) Close() error {
	if w.done {
		return nil
	}
	w.done = true
	_ = w.f.Close()
	return os.Remove(w.tmp)
}

// enforceLimitLocked evicts least-recently-touched blobs until the cache is at
// or below maxBytes (half the cap as a threshold, so we don't evict on every
// commit). Caller holds c.mu.
func (c *BlobCache) enforceLimitLocked() {
	if c.maxBytes <= 0 {
		return
	}
	type item struct {
		path  string
		size  int64
		atime time.Time
	}
	var (
		items []item
		total int64
	)
	// Walk blobs/<algo>/<hex>/data.
	algos, err := os.ReadDir(filepath.Join(c.dir, "blobs"))
	if err != nil {
		return
	}
	for _, a := range algos {
		if !a.IsDir() {
			continue
		}
		hexes, err := os.ReadDir(filepath.Join(c.dir, "blobs", a.Name()))
		if err != nil {
			continue
		}
		for _, h := range hexes {
			p := filepath.Join(c.dir, "blobs", a.Name(), h.Name(), "data")
			fi, err := os.Stat(p)
			if err != nil || fi.IsDir() {
				continue
			}
			items = append(items, item{path: p, size: fi.Size(), atime: fi.ModTime()})
			total += fi.Size()
		}
	}
	if total <= c.maxBytes {
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].atime.Before(items[j].atime) })
	for _, it := range items {
		if total <= c.maxBytes/2 {
			break
		}
		if err := os.Remove(it.path); err == nil {
			total -= it.size
		}
	}
}
