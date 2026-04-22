package api

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-contrib/gzip"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"apod-server/internal/config"
	"apod-server/internal/fetch"
	"apod-server/internal/httputil"
	"apod-server/internal/image"
	applog "apod-server/internal/log"
	"apod-server/internal/server/cron"
	"apod-server/internal/store"
)

// ServerConfig holds all dependencies for the API server.
type ServerConfig struct {
	Fetch       *fetch.Service
	Cache       store.Cache
	KV          store.KVStore
	Image       *image.Service
	Logger      *zap.Logger
	AuthKey     string
	MetricsKey  string
	DemoLimiter *cron.DemoIPLimiter
	RateLimiter *rate.Limiter
	Addr        string
}

// Server is the HTTP API server.
type Server struct {
	cfg ServerConfig
}

// NewServer creates a Server.
func NewServer(cfg ServerConfig) *Server {
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	return &Server{cfg: cfg}
}

func (s *Server) setupRouter() *gin.Engine {
	r := gin.New()
	cfg := s.cfg

	trusted := strings.TrimSpace(config.Getenv("TRUSTED_PROXIES", "127.0.0.1,::1"))
	proxies := make([]string, 0, 4)
	for _, p := range strings.Split(trusted, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			proxies = append(proxies, p)
		}
	}
	if len(proxies) == 0 {
		proxies = []string{"127.0.0.1", "::1"}
	}
	httputil.SetTrustedProxiesForRealIP(proxies)
	if err := r.SetTrustedProxies(proxies); err != nil {
		cfg.Logger.Warn("set trusted proxies failed", zap.Error(err), zap.Strings("trusted_proxies", proxies))
	}

	logEncoding := applog.LogEncoding()

	r.Use(gzip.Gzip(gzip.DefaultCompression))
	r.Use(traceIDMiddleware(cfg.Logger))
	r.Use(accessLogMiddleware(logEncoding))
	r.Use(recoveryMiddleware(cfg.Logger))

	r.GET("/metrics", strictAuthMiddleware(cfg.MetricsKey), gin.WrapH(promhttp.Handler()))
	r.GET("/healthz", healthHandler)
	r.GET("/readyz", readinessHandler(cfg.KV, cfg.Image))
	r.GET("/static/apod/:filename", staticImageHandler(cfg.Fetch, cfg.Image))

	authMW := apiKeyAuthMiddleware(cfg.AuthKey, cfg.DemoLimiter)
	rateMW := rateLimitMiddleware(cfg.RateLimiter)
	r.GET("/v1/apod", authMW, rateMW, apodHandler(cfg.Fetch))
	r.GET("/v1/apod/image", authMW, rateMW, imageRedirectHandler(cfg.Fetch))

	return r
}

// Run starts the HTTP server and blocks until a shutdown signal is received.
func (s *Server) Run() error {
	r := s.setupRouter()
	srv := &http.Server{Addr: s.cfg.Addr, Handler: r}

	go func() {
		s.cfg.Logger.Info("APOD service running", zap.String("addr", s.cfg.Addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.cfg.Logger.Fatal("server listen failed", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	s.cfg.Logger.Info("received shutdown signal", zap.String("signal", sig.String()))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		s.cfg.Logger.Error("server shutdown error", zap.Error(err))
		return err
	}
	s.cfg.Logger.Info("server exited gracefully")
	return nil
}
