package static

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheEviction(t *testing.T) {
	for _, tc := range []struct {
		mode    Eviction
		evicted string
	}{
		{EvictFIFO, "a"},
		{EvictLIFO, "c"},
		{EvictLargest, "b"},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			c, err := NewCache(CacheOptions{MaxBytes: 10, Eviction: tc.mode})
			must(t, err)
			c.put("a", make([]byte, 3), time.Time{})
			c.put("b", make([]byte, 5), time.Time{})
			c.put("c", make([]byte, 2), time.Time{})
			c.put("d", make([]byte, 4), time.Time{}) // needs room
			if _, ok := c.get(tc.evicted); ok {
				t.Errorf("%q should have been evicted", tc.evicted)
			}
			if _, ok := c.get("d"); !ok {
				t.Error("new entry missing")
			}
			if s := c.Stats(); s.Bytes > 10 || s.Evictions == 0 {
				t.Errorf("stats = %+v", s)
			}
		})
	}
}

func TestCacheTTLAndDisabled(t *testing.T) {
	c, err := NewCache(CacheOptions{MaxBytes: 10, TTL: time.Millisecond})
	must(t, err)
	c.put("a", []byte("x"), time.Time{})
	time.Sleep(5 * time.Millisecond)
	if _, ok := c.get("a"); ok {
		t.Error("expired entry served")
	}
	if c, _ := NewCache(CacheOptions{}); c != nil {
		t.Error("zero MaxBytes should disable the cache")
	}
	if _, err := NewCache(CacheOptions{MaxBytes: 1, Eviction: "lru"}); err == nil {
		t.Error("unknown eviction accepted")
	}
}

func TestHandlerServesFromCache(t *testing.T) {
	base := t.TempDir()
	must(t, os.WriteFile(filepath.Join(base, "a.txt"), []byte("one"), 0o644))
	must(t, os.WriteFile(filepath.Join(base, "big.txt"), make([]byte, 100), 0o644))
	root, err := os.OpenRoot(base)
	must(t, err)
	t.Cleanup(func() { root.Close() })
	cache, err := NewCache(CacheOptions{MaxBytes: 1000, MaxEntryBytes: 50})
	must(t, err)
	h, err := NewHandler(root, Options{Cache: cache})
	must(t, err)

	get := func(p string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		return rec
	}
	get("/a.txt")
	must(t, os.WriteFile(filepath.Join(base, "a.txt"), []byte("two"), 0o644))
	if rec := get("/a.txt"); rec.Body.String() != "one" || rec.Code != 200 {
		t.Errorf("want cached body, got %d %q", rec.Code, rec.Body.String())
	}
	if rec := get("/big.txt"); rec.Body.Len() != 100 {
		t.Errorf("oversized file not served: %d", rec.Body.Len())
	}
	if rec := get("/missing.txt"); rec.Code != 404 {
		t.Errorf("missing = %d", rec.Code)
	}
	if s := cache.Stats(); s.Entries != 1 || s.Hits != 1 {
		t.Errorf("stats = %+v", s)
	}
}
