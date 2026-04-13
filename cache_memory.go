package main

import (
	"sort"
	"sync"
	"time"
)

type cacheItem struct {
	data       *APOD
	expiredAt  time.Time
	permanent  bool
	lastAccess time.Time
}

type Cache struct {
	mu       sync.RWMutex
	data     map[string]cacheItem
	todayTTL time.Duration
	maxItems int
}

func NewCache() *Cache {
	ttlMinutes := getenvInt("MEMORY_CACHE_TTL_MINUTES", 180)
	if ttlMinutes <= 0 {
		ttlMinutes = 180
	}
	maxItems := getenvInt("MEMORY_CACHE_MAX_ITEMS", 2000)
	if maxItems <= 0 {
		maxItems = 2000
	}
	return &Cache{data: make(map[string]cacheItem), todayTTL: time.Duration(ttlMinutes) * time.Minute, maxItems: maxItems}
}

func isToday(date string) bool {
	return date == getNasaTime().Format("2006-01-02")
}

func (c *Cache) Get(key string) *APOD {
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
	copy := *item.data
	copy.Cached = true
	return &copy
}

func (c *Cache) Set(key string, value *APOD) {
	if value == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	copy := *value
	copy.Cached = false
	item := cacheItem{data: &copy, permanent: !isToday(key), lastAccess: time.Now()}
	if !item.permanent {
		item.expiredAt = time.Now().Add(c.todayTTL)
	}
	c.data[key] = item
	c.evictLocked()
}

func (c *Cache) GetLast() *APOD {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	var latest *APOD
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
	copy := *latest
	copy.Cached = true
	return &copy
}

func (c *Cache) Cleanup() {
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

func (c *Cache) evictLocked() {
	if c.maxItems <= 0 || len(c.data) <= c.maxItems {
		return
	}
	overflow := len(c.data) - c.maxItems
	batch := c.maxItems * memoryEvictStep / 100
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
