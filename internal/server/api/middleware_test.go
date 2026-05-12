package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"apod-server/internal/server/cron"
)

func newAuthTestRouter(allowDemoKey bool, demoLimiter *cron.DemoIPLimiter) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(traceIDMiddleware(zap.NewNop()))
	r.GET("/protected", apiKeyAuthMiddleware("secret", allowDemoKey, demoLimiter), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	r.GET("/strict", strictAuthMiddleware("metrics"), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func TestAPIKeyAuthRejectsMissingTokenWhenDemoDisabled(t *testing.T) {
	r := newAuthTestRouter(false, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, w.Code)
	}
}

func TestAPIKeyAuthAllowsMissingTokenOnlyWhenDemoEnabled(t *testing.T) {
	r := newAuthTestRouter(true, cron.NewDemoIPLimiter(1, 0))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, w.Code)
	}
}

func TestStrictAuthRejectsMissingToken(t *testing.T) {
	r := newAuthTestRouter(true, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/strict", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, w.Code)
	}
}
