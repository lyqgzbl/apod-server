package cron

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"apod-server/internal/httputil"
)

func performRequest(r http.Handler, method, path, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestDemoIPLimiterConcurrentAllowRollbackNoLeak(t *testing.T) {
	limiter := NewDemoIPLimiter(100000, 24*time.Hour)
	const (
		ip      = "198.51.100.10"
		workers = 32
		loops   = 200
	)

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < loops; j++ {
				if limiter.Allow(ip) {
					limiter.Rollback(ip)
				}
			}
		}()
	}

	close(start)
	wg.Wait()

	_, exists := limiter.RecordForIP(ip)
	if exists {
		t.Fatalf("expected no remaining usage record for %s", ip)
	}
}

func TestAPIKeyAuthMiddlewareConcurrentFailuresNoQuotaLeak(t *testing.T) {
	gin.SetMode(gin.TestMode)
	httputil.SetTrustedProxiesForRealIP([]string{"*"})

	demoLimiter := NewDemoIPLimiter(1, 24*time.Hour)

	r := gin.New()
	// Use a simple auth middleware that replicates the demo flow
	r.Use(func(c *gin.Context) {
		ip := httputil.RealIP(c.Request)
		// simulate demo mode: always use demo key
		if !demoLimiter.Allow(ip) {
			c.JSON(http.StatusTooManyRequests, gin.H{"code": 429, "msg": "rate limited"})
			c.Abort()
			return
		}
		c.Next()
		if c.Writer.Status() != http.StatusOK {
			demoLimiter.Rollback(ip)
		}
	})
	r.GET("/fail", func(c *gin.Context) {
		c.JSON(http.StatusInternalServerError, gin.H{"msg": "boom"})
	})
	r.GET("/ok", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"msg": "ok"})
	})

	const (
		ipAddr     = "198.51.100.23"
		remoteAddr = ipAddr + ":12345"
		workers    = 32
	)

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = performRequest(r, http.MethodGet, "/fail", remoteAddr)
		}()
	}

	close(start)
	wg.Wait()

	_, existsAfterFailures := demoLimiter.RecordForIP(ipAddr)
	if existsAfterFailures {
		t.Fatalf("expected no remaining usage record for %s after failed requests", ipAddr)
	}

	w := performRequest(r, http.MethodGet, "/ok", remoteAddr)
	if w.Code != http.StatusOK {
		t.Fatalf("expected follow-up request status %d, got %d", http.StatusOK, w.Code)
	}

	rec, existsAfterSuccess := demoLimiter.RecordForIP(ipAddr)
	if !existsAfterSuccess {
		t.Fatalf("expected usage record for %s after successful request", ipAddr)
	}
	if rec.Count != 1 {
		t.Fatalf("expected usage count 1 for %s, got %d", ipAddr, rec.Count)
	}
}
