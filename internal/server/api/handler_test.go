package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"apod-server/internal/image"
)

func TestStaticImagePublicOnlyServesCachedFiles(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	imgSvc := image.NewService(image.Config{Dir: dir, Logger: zap.NewNop()})
	srv := NewServer(ServerConfig{
		Image:      imgSvc,
		Logger:     zap.NewNop(),
		AuthKey:    "secret",
		MetricsKey: "metrics",
	})
	router := srv.setupRouter()

	miss := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/apod/2026-01-01.jpg", nil)
	router.ServeHTTP(miss, req)
	if miss.Code != http.StatusNotFound {
		t.Fatalf("expected cache miss status %d, got %d", http.StatusNotFound, miss.Code)
	}

	if err := os.WriteFile(filepath.Join(dir, "2026-01-01.jpg"), []byte("jpg"), 0o644); err != nil {
		t.Fatalf("write cached image: %v", err)
	}

	hit := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/static/apod/2026-01-01.jpg", nil)
	router.ServeHTTP(hit, req)
	if hit.Code != http.StatusOK {
		t.Fatalf("expected cache hit status %d, got %d", http.StatusOK, hit.Code)
	}
	if hit.Body.String() != "jpg" {
		t.Fatalf("expected cached image body, got %q", hit.Body.String())
	}
}

func TestImageRedirectRejectsInvalidBearerBeforeFetch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	imgSvc := image.NewService(image.Config{Dir: t.TempDir(), Logger: zap.NewNop()})
	srv := NewServer(ServerConfig{
		Image:      imgSvc,
		Logger:     zap.NewNop(),
		AuthKey:    "secret",
		MetricsKey: "metrics",
	})
	router := srv.setupRouter()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/apod/image", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, w.Code)
	}
}
