package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func performRequest(r http.Handler, method, path, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestDemoIPLimiterConcurrentAllowRollbackNoLeak(t *testing.T) {
	limiter := newDemoIPLimiter(100000, 24*time.Hour)
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
				if limiter.allow(ip) {
					limiter.rollback(ip)
				}
			}
		}()
	}

	close(start)
	wg.Wait()

	limiter.mu.Lock()
	_, exists := limiter.records[ip]
	limiter.mu.Unlock()
	if exists {
		t.Fatalf("expected no remaining usage record for %s", ip)
	}
}

func TestAPIKeyAuthMiddlewareConcurrentFailuresNoQuotaLeak(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger = zap.NewNop()

	oldLimiter := demoLimiter
	t.Cleanup(func() {
		demoLimiter = oldLimiter
	})

	demoLimiter = newDemoIPLimiter(1, 24*time.Hour)

	r := gin.New()
	r.Use(apiKeyAuthMiddleware("ignored"))
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

	demoLimiter.mu.Lock()
	_, existsAfterFailures := demoLimiter.records[ipAddr]
	demoLimiter.mu.Unlock()
	if existsAfterFailures {
		t.Fatalf("expected no remaining usage record for %s after failed requests", ipAddr)
	}

	w := performRequest(r, http.MethodGet, "/ok", remoteAddr)
	if w.Code != http.StatusOK {
		t.Fatalf("expected follow-up request status %d, got %d", http.StatusOK, w.Code)
	}

	demoLimiter.mu.Lock()
	rec, existsAfterSuccess := demoLimiter.records[ipAddr]
	demoLimiter.mu.Unlock()
	if !existsAfterSuccess {
		t.Fatalf("expected usage record for %s after successful request", ipAddr)
	}
	if rec.count != 1 {
		t.Fatalf("expected usage count 1 for %s, got %d", ipAddr, rec.count)
	}
}
