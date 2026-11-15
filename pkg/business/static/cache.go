package static

import (
	"container/list"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Eviction chooses which entry is removed when the cache is full.
type Eviction string

const (
	// EvictFIFO removes the entry that was added first.
	EvictFIFO Eviction = "fifo"
	// EvictLIFO removes the entry that was added last.
	EvictLIFO Eviction = "lifo"
	// EvictLargest removes the largest entry.
	EvictLargest Eviction = "largest"
)

// Valid reports whether e is a known eviction mode.
func (e Eviction) Valid() bool {
	return e == EvictFIFO || e == EvictLIFO || e == EvictLargest
}

// CacheOptions configures a Cache.
type CacheOptions struct {
	// MaxBytes is the total size of cached bodies. Zero disables the cache.
	MaxBytes int64
	// MaxEntryBytes is the largest file that is cached; bigger files are
	// always served from disk. Zero means MaxBytes.
	MaxEntryBytes int64
	// TTL is how long an entry is served before it is reloaded from disk.
	// Zero keeps entries until they are evicted.
	TTL time.Duration
	// Eviction defaults to EvictFIFO.
	Eviction Eviction
}

// CacheStats is a snapshot of cache counters.
type CacheStats struct {
	Hits, Misses, Evictions uint64
	Entries                 int
	Bytes                   int64
}

type entry struct {
	name    string
	data    []byte
	modTime time.Time
	expires time.Time
	elem    *list.Element
}

// Cache is an in-memory, size-bounded store of file contents keyed by name.
// It is safe for concurrent use.
type Cache struct {
	opts CacheOptions

	mu      sync.RWMutex
	entries map[string]*entry
	order   *list.List // insertion order, oldest at the front
	bytes   int64

	hits, misses, evictions atomic.Uint64
}

// NewCache returns a cache, or nil when opts.MaxBytes is zero.
func NewCache(opts CacheOptions) (*Cache, error) {
	if opts.Eviction == "" {
		opts.Eviction = EvictFIFO
	}
	if !opts.Eviction.Valid() {
		return nil, fmt.Errorf("unknown cache eviction %q", opts.Eviction)
	}
	if opts.MaxBytes < 0 || opts.MaxEntryBytes < 0 || opts.TTL < 0 {
		return nil, fmt.Errorf("cache limits must not be negative")
	}
	if opts.MaxBytes == 0 {
		return nil, nil
	}
	if opts.MaxEntryBytes == 0 || opts.MaxEntryBytes > opts.MaxBytes {
		opts.MaxEntryBytes = opts.MaxBytes
	}
	return &Cache{opts: opts, entries: map[string]*entry{}, order: list.New()}, nil
}

// cacheable reports whether a file of size bytes may be cached.
func (c *Cache) cacheable(size int64) bool { return size <= c.opts.MaxEntryBytes }

// get returns the entry for name unless it is missing or expired.
func (c *Cache) get(name string) (*entry, bool) {
	c.mu.RLock()
	e, ok := c.entries[name]
	c.mu.RUnlock()
	if ok && (e.expires.IsZero() || time.Now().Before(e.expires)) {
		c.hits.Add(1)
		return e, true
	}
	c.misses.Add(1)
	return nil, false
}

// put stores data, evicting other entries as needed. data must not be
// modified afterwards. It returns the stored entry.
func (c *Cache) put(name string, data []byte, modTime time.Time) *entry {
	e := &entry{name: name, data: data, modTime: modTime}
	if c.opts.TTL > 0 {
		e.expires = time.Now().Add(c.opts.TTL)
	}
	size := int64(len(data))

	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[name]; ok {
		c.remove(old)
	}
	c.purgeExpired()
	for c.bytes+size > c.opts.MaxBytes && c.order.Len() > 0 {
		c.remove(c.victim())
		c.evictions.Add(1)
	}
	e.elem = c.order.PushBack(e)
	c.entries[name] = e
	c.bytes += size
	return e
}

// Stats returns the current counters.
func (c *Cache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return CacheStats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
		Entries:   len(c.entries),
		Bytes:     c.bytes,
	}
}

func (c *Cache) victim() *entry {
	switch c.opts.Eviction {
	case EvictLIFO:
		return c.order.Back().Value.(*entry)
	case EvictLargest:
		var big *entry
		for e := c.order.Front(); e != nil; e = e.Next() {
			if v := e.Value.(*entry); big == nil || len(v.data) > len(big.data) {
				big = v
			}
		}
		return big
	}
	return c.order.Front().Value.(*entry)
}

func (c *Cache) purgeExpired() {
	if c.opts.TTL == 0 {
		return
	}
	now := time.Now()
	for el := c.order.Front(); el != nil; {
		next := el.Next()
		if e := el.Value.(*entry); now.After(e.expires) {
			c.remove(e)
		}
		el = next
	}
}

func (c *Cache) remove(e *entry) {
	c.order.Remove(e.elem)
	delete(c.entries, e.name)
	c.bytes -= int64(len(e.data))
}
