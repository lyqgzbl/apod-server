package store

import (
	"sort"
	"sync"
	"time"

	"apod-server/internal/httputil"
	"apod-server/internal/model"
)

type cacheItem struct {
	data       *model.APOD
	expiredAt  time.Time
	permanent  bool
	lastAccess time.Time
}

// MemoryCache is an in-memory LRU+TTL cache implementing the Cache interface.
type MemoryCache struct {
	mu       sync.RWMutex
	data     map[string]cacheItem
	todayTTL time.Duration
	maxItems int
	evictPct int
}

// NewMemoryCache creates a new MemoryCache.
func NewMemoryCache(ttlMinutes, maxItems, evictPct int) *MemoryCache {
	if ttlMinutes <= 0 {
		ttlMinutes = 180
	}
	if maxItems <= 0 {
		maxItems = 2000
	}
	if evictPct <= 0 {
		evictPct = 10
	}
	return &MemoryCache{
		data:     make(map[string]cacheItem),
		todayTTL: time.Duration(ttlMinutes) * time.Minute,
		maxItems: maxItems,
		evictPct: evictPct,
	}
}

func (c *MemoryCache) Get(key string) *model.APOD {
	now := time.Now()
	c.mu.RLock()
	item, ok := c.data[key]
	c.mu.RUnlock()
	if !ok {
		return nil
	}
	if !item.permanent && now.After(item.expiredAt) {
		c.mu.Lock()
		if latest, exists := c.data[key]; exists && !latest.permanent && now.After(latest.expiredAt) {
			delete(c.data, key)
		}
		c.mu.Unlock()
		return nil
	}
	// Update lastAccess for LRU accuracy
	c.mu.Lock()
	if cur, exists := c.data[key]; exists {
		cur.lastAccess = now
		c.data[key] = cur
	}
	c.mu.Unlock()
	cp := *item.data
	cp.Cached = true
	return &cp
}

func (c *MemoryCache) Set(key string, value *model.APOD) {
	if value == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := *value
	cp.Cached = false
	item := cacheItem{data: &cp, permanent: !httputil.IsToday(key), lastAccess: time.Now()}
	if !item.permanent {
		item.expiredAt = time.Now().Add(c.todayTTL)
	}
	c.data[key] = item
	c.evictLocked()
}

func (c *MemoryCache) GetLast() *model.APOD {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	var latest *model.APOD
	for k, item := range c.data {
		if !item.permanent && now.After(item.expiredAt) {
			delete(c.data, k)
			continue
		}
		if latest == nil || item.data.Date > latest.Date {
			latest = item.data
		}
	}
	if latest == nil {
		return nil
	}
	cp := *latest
	cp.Cached = true
	return &cp
}

func (c *MemoryCache) Cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, item := range c.data {
		if !item.permanent && now.After(item.expiredAt) {
			delete(c.data, k)
		}
	}
	c.evictLocked()
}

func (c *MemoryCache) evictLocked() {
	if c.maxItems <= 0 || len(c.data) <= c.maxItems {
		return
	}
	overflow := len(c.data) - c.maxItems
	batch := c.maxItems * c.evictPct / 100
	if batch < 1 {
		batch = 1
	}
	removeCount := overflow
	if removeCount < batch {
		removeCount = batch
	}
	if removeCount > len(c.data) {
		removeCount = len(c.data)
	}
	items := make([]struct {
		key        string
		lastAccess time.Time
	}, 0, len(c.data))
	for k, v := range c.data {
		items = append(items, struct {
			key        string
			lastAccess time.Time
		}{key: k, lastAccess: v.lastAccess})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].lastAccess.Before(items[j].lastAccess) })
	for i := 0; i < removeCount; i++ {
		delete(c.data, items[i].key)
	}
}
