package cron

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"

	"apod-server/internal/fetch"
	"apod-server/internal/httputil"
	"apod-server/internal/image"
	applog "apod-server/internal/log"
	"apod-server/internal/store"
)

// Manager manages all background tasks.
type Manager struct {
	Fetch  *fetch.Service
	Cache  store.Cache
	Image  *image.Service
	Logger *zap.Logger
}

// --- Prefetch ---

func (m *Manager) prefetchWithRetry(traceID string, maxRetries int) {
	date := httputil.GetNasaTime().Format("2006-01-02")
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			m.Logger.Info("cron prefetch retry scheduled", zap.String("date", date), zap.Int("attempt", attempt+1), zap.Int("max_attempts", maxRetries+1))
			time.Sleep(5 * time.Minute)
			date = httputil.GetNasaTime().Format("2006-01-02")
		}
		ctx, cancel := context.WithTimeout(applog.WithLogger(context.Background(), m.Logger.With(zap.String("trace_id", traceID))), 20*time.Second)
		apod, source, err := m.Fetch.GetAPOD(ctx, date)
		cancel()
		if err == nil {
			m.Logger.Info("prefetch success", zap.String("trace_id", traceID), zap.String("date", apod.Date), zap.String("source", source), zap.Int("attempt", attempt+1))
			return
		}
		m.Logger.Warn("prefetch attempt failed", zap.String("trace_id", traceID), zap.String("date", date), zap.Int("attempt", attempt+1), zap.Int("max_attempts", maxRetries+1), zap.Error(err))
	}
	m.Logger.Warn("prefetch exhausted all retries", zap.String("trace_id", traceID), zap.String("date", date), zap.Int("max_attempts", maxRetries+1))
}

// StartPrefetchCron starts the daily prefetch cron and fires a startup prefetch.
func (m *Manager) StartPrefetchCron() *cron.Cron {
	loc, _ := time.LoadLocation("America/New_York")
	c := cron.New(cron.WithLocation(loc))
	_, err := c.AddFunc("30 0 * * *", func() {
		m.prefetchWithRetry("cron-prefetch", 3)
	})
	if err != nil {
		m.Logger.Fatal("start cron failed", zap.Error(err))
	}
	c.Start()

	go func() {
		time.Sleep(3 * time.Second)
		m.prefetchWithRetry("startup-prefetch", 0)
	}()

	return c
}

// StartImageCleanupCron starts a daily image cache cleanup cron.
func (m *Manager) StartImageCleanupCron() *cron.Cron {
	loc, _ := time.LoadLocation("America/New_York")
	c := cron.New(cron.WithLocation(loc))
	_, err := c.AddFunc("20 3 * * *", func() { m.Image.Cleanup() })
	if err != nil {
		m.Logger.Fatal("start image cleanup cron failed", zap.Error(err))
	}
	c.Start()
	go m.Image.Cleanup()
	return c
}

// StartMemoryCleanupTicker starts a periodic memory cache cleanup.
func (m *Manager) StartMemoryCleanupTicker(intervalMinutes int) {
	if intervalMinutes <= 0 {
		intervalMinutes = 15
	}
	interval := time.Duration(intervalMinutes) * time.Minute
	go m.Cache.Cleanup()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			m.Cache.Cleanup()
		}
	}()
}

// --- DemoIPLimiter ---

// DemoUsage tracks per-IP demo key usage (exported for testing).
type DemoUsage struct {
	Count     int
	WindowBgn time.Time
}

// DemoIPLimiter rate-limits DEMO_KEY usage per IP.
type DemoIPLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	records map[string]DemoUsage
}

// NewDemoIPLimiter creates a new DemoIPLimiter.
func NewDemoIPLimiter(limit int, window time.Duration) *DemoIPLimiter {
	if limit <= 0 {
		limit = 5
	}
	if window <= 0 {
		window = 24 * time.Hour
	}
	return &DemoIPLimiter{limit: limit, window: window, records: make(map[string]DemoUsage, 128)}
}

// Limit returns the configured limit.
func (d *DemoIPLimiter) Limit() int { return d.limit }

// Allow checks if the IP is allowed to make a request.
func (d *DemoIPLimiter) Allow(ip string) bool {
	if d == nil {
		return true
	}
	ip = strings.TrimSpace(ip)
	if ip == "" {
		ip = "unknown"
	}
	now := time.Now()

	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.records) > 1024 {
		for k, rec := range d.records {
			if now.Sub(rec.WindowBgn) >= d.window {
				delete(d.records, k)
			}
		}
	}

	rec, ok := d.records[ip]
	if !ok || now.Sub(rec.WindowBgn) >= d.window {
		d.records[ip] = DemoUsage{Count: 1, WindowBgn: now}
		return true
	}
	if rec.Count >= d.limit {
		return false
	}
	rec.Count++
	d.records[ip] = rec
	return true
}

// Rollback decrements the usage count for the given IP.
func (d *DemoIPLimiter) Rollback(ip string) {
	if d == nil {
		return
	}
	ip = strings.TrimSpace(ip)
	if ip == "" {
		ip = "unknown"
	}
	now := time.Now()

	d.mu.Lock()
	defer d.mu.Unlock()

	rec, ok := d.records[ip]
	if !ok {
		return
	}
	if now.Sub(rec.WindowBgn) >= d.window {
		delete(d.records, ip)
		return
	}
	if rec.Count <= 1 {
		delete(d.records, ip)
		return
	}
	rec.Count--
	d.records[ip] = rec
}

// StartDemoLimiterCleanup starts periodic cleanup of expired demo limiter records.
func StartDemoLimiterCleanup(limiter *DemoIPLimiter) {
	if limiter == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			limiter.mu.Lock()
			for k, rec := range limiter.records {
				if now.Sub(rec.WindowBgn) >= limiter.window {
					delete(limiter.records, k)
				}
			}
			limiter.mu.Unlock()
		}
	}()
}

// RecordForIP returns the usage record for an IP (for testing).
func (d *DemoIPLimiter) RecordForIP(ip string) (DemoUsage, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rec, ok := d.records[ip]
	return rec, ok
}
