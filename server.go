package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-contrib/gzip"
	ginzap "github.com/gin-contrib/zap"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

func registerMetrics() {
	prometheus.MustRegister(apodRequestTotal)
	prometheus.MustRegister(apodRequestDuration)
	prometheus.MustRegister(apodSourceTotal)
	prometheus.MustRegister(apodFetchFailTotal)
	prometheus.MustRegister(apodParseFailTotal)
	prometheus.MustRegister(apodCacheHitTotal)
	prometheus.MustRegister(apodCacheMissTotal)
	prometheus.MustRegister(apodCacheHitRatio)
	prometheus.MustRegister(imageCacheHitTotal)
	prometheus.MustRegister(imageCacheMissTotal)
}

func newRateLimiter() *rate.Limiter {
	rps := getenvFloat64("API_RATE_LIMIT_RPS", 8)
	burst := getenvInt("API_RATE_LIMIT_BURST", 16)
	if rps <= 0 {
		rps = 8
	}
	if burst <= 0 {
		burst = 16
	}
	return rate.NewLimiter(rate.Limit(rps), burst)
}

func rateLimitMiddleware(limiter *rate.Limiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if limiter != nil && !limiter.Allow() {
			apodRequestTotal.WithLabelValues("rate_limited", "rate-limit").Inc()
			c.JSON(http.StatusTooManyRequests, gin.H{"code": 429, "msg": "too many requests"})
			c.Abort()
			return
		}
		c.Next()
	}
}

func apiKeyAuthMiddleware(requiredKey string) gin.HandlerFunc {
	requiredBytes := []byte(requiredKey)
	return func(c *gin.Context) {
		l := requestLogger(c)
		provided, msg := readAPIKeyFromHeader(c)
		if msg != "" {
			l.Warn("auth failed", zap.String("method", c.Request.Method), zap.String("ip", c.ClientIP()), zap.String("path", c.Request.URL.Path), zap.Int("status", http.StatusUnauthorized), zap.String("reason", msg))
			c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "msg": msg})
			c.Abort()
			return
		}
		if len(provided) != len(requiredBytes) || subtle.ConstantTimeCompare([]byte(provided), requiredBytes) != 1 {
			l.Warn("auth failed", zap.String("method", c.Request.Method), zap.String("ip", c.ClientIP()), zap.String("path", c.Request.URL.Path), zap.Int("status", http.StatusUnauthorized), zap.String("reason", "invalid API key"))
			c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "msg": "invalid API key"})
			c.Abort()
			return
		}
		c.Next()
	}
}

func readAPIKeyFromHeader(c *gin.Context) (string, string) {
	authorization := strings.TrimSpace(c.GetHeader("Authorization"))
	if authorization != "" {
		parts := strings.SplitN(authorization, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return "", "Authorization must use Bearer token"
		}
		token := strings.TrimSpace(parts[1])
		if token == "" {
			return "", "Bearer token is required"
		}
		return token, ""
	}

	apiKey := strings.TrimSpace(c.GetHeader("X-API-Key"))
	if apiKey != "" {
		return apiKey, ""
	}

	return "", "Authorization: Bearer <token> is required"
}

func traceIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Trace-ID")
		if id == "" {
			id = uuid.NewString()
		}
		requestLog := logger.With(zap.String("trace_id", id))
		c.Set("request_logger", requestLog)
		c.Writer.Header().Set("X-Trace-ID", id)
		c.Request = c.Request.WithContext(withLogger(c.Request.Context(), requestLog))
		c.Next()
	}
}

func requestLogger(c *gin.Context) *zap.Logger {
	if v, ok := c.Get("request_logger"); ok {
		if l, ok := v.(*zap.Logger); ok && l != nil {
			return l
		}
	}
	return loggerFromCtx(c.Request.Context())
}

func latencyFieldForAccessLog(d time.Duration) zap.Field {
	if logEncoding() == "console" {
		ms := float64(d) / float64(time.Millisecond)
		return zap.String("latency", fmt.Sprintf("%8.3fms", ms))
	}
	return zap.Duration("latency", d)
}

func accessLogMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		method := c.Request.Method
		path := c.Request.URL.Path

		c.Next()

		status := c.Writer.Status()
		latency := time.Since(started)
		fields := []zap.Field{
			zap.String("method", method),
			zap.String("path", path),
			zap.Int("status", status),
			latencyFieldForAccessLog(latency),
			zap.String("ip", c.ClientIP()),
		}
		if len(c.Errors) > 0 {
			fields = append(fields, zap.String("errors", c.Errors.String()))
		}

		l := requestLogger(c)
		switch {
		case status >= http.StatusInternalServerError:
			l.Error("http_request", fields...)
		case status >= http.StatusBadRequest:
			l.Warn("http_request", fields...)
		default:
			l.Info("http_request", fields...)
		}
	}
}

func startPrefetchCron() {
	loc, _ := time.LoadLocation("America/New_York")
	c := cron.New(cron.WithLocation(loc))
	_, err := c.AddFunc("5 0 * * *", func() {
		date := getNasaTime().Format("2006-01-02")
		ctx, cancel := context.WithTimeout(withLogger(context.Background(), logger.With(zap.String("trace_id", "cron-prefetch"))), 15*time.Second)
		defer cancel()
		apod, source, err := getAPOD(ctx, date)
		if err != nil {
			logger.Error("cron prefetch failed", zap.String("date", date), zap.Error(err))
			return
		}
		logger.Info("cron prefetch success", zap.String("date", apod.Date), zap.String("source", source))
	})
	if err != nil {
		logger.Fatal("start cron failed", zap.Error(err))
	}
	c.Start()

	go func() {
		date := getNasaTime().Format("2006-01-02")
		ctx, cancel := context.WithTimeout(withLogger(context.Background(), logger.With(zap.String("trace_id", "startup-prefetch"))), 15*time.Second)
		defer cancel()
		_, source, err := getAPOD(ctx, date)
		if err != nil {
			logger.Warn("startup prefetch failed", zap.String("date", date), zap.Error(err))
			return
		}
		logger.Info("startup prefetch success", zap.String("date", date), zap.String("source", source))
	}()
}

func startImageCleanupCron() {
	loc, _ := time.LoadLocation("America/New_York")
	c := cron.New(cron.WithLocation(loc))
	_, err := c.AddFunc("20 3 * * *", func() { imageStore.Cleanup() })
	if err != nil {
		logger.Fatal("start image cleanup cron failed", zap.Error(err))
	}
	c.Start()
	go imageStore.Cleanup()
}

func startMemoryCleanupTicker() {
	intervalMinutes := getenvInt("MEMORY_CACHE_CLEANUP_MINUTES", 15)
	if intervalMinutes <= 0 {
		intervalMinutes = 15
	}
	interval := time.Duration(intervalMinutes) * time.Minute
	go cache.Cleanup()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			cache.Cleanup()
		}
	}()
}

func checkFSWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "ready-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func healthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func readinessHandler(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	redisErr := redisStore.Ready(ctx)
	fsErr := checkFSWritable(imageStore.dir)
	redisStatus, fsStatus := "ok", "ok"
	if redisErr != nil {
		redisStatus = redisErr.Error()
	}
	if fsErr != nil {
		fsStatus = fsErr.Error()
	}
	code, status := http.StatusOK, "ready"
	if redisErr != nil || fsErr != nil {
		code, status = http.StatusServiceUnavailable, "not_ready"
	}
	c.JSON(code, gin.H{"status": status, "redis": redisStatus, "fs": fsStatus})
}

func setupRouter() *gin.Engine {
	r := gin.New()
	authKey := strings.TrimSpace(getenv("API_AUTH_KEY", "changeme"))
	if authKey == "changeme" {
		logger.Warn("using default API_AUTH_KEY, please override in production")
	}

	trusted := strings.TrimSpace(getenv("TRUSTED_PROXIES", "127.0.0.1,::1"))
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
	if err := r.SetTrustedProxies(proxies); err != nil {
		logger.Warn("set trusted proxies failed", zap.Error(err), zap.Strings("trusted_proxies", proxies))
	}

	r.Use(gzip.Gzip(gzip.DefaultCompression))
	r.Use(traceIDMiddleware())
	r.Use(accessLogMiddleware())
	r.Use(ginzap.RecoveryWithZap(logger, true))

	r.GET("/metrics", gin.WrapH(promhttp.Handler()))
	r.GET("/healthz", healthHandler)
	r.GET("/readyz", readinessHandler)

	r.GET("/v1/apod", apiKeyAuthMiddleware(authKey), rateLimitMiddleware(apiLimiter), func(c *gin.Context) {
		l := requestLogger(c)
		started := time.Now()
		date := c.Query("date")
		c.Header("Cache-Control", "public, max-age=3600")

		apod, source, err := getAPOD(c.Request.Context(), date)
		if err != nil {
			apodRequestTotal.WithLabelValues("error", source).Inc()
			apodRequestDuration.WithLabelValues(source).Observe(time.Since(started).Seconds())
			if source == "failed" {
				l.Error("get apod failed", zap.String("date", date), zap.String("source", source), zap.Error(err))
			} else {
				l.Warn("get apod failed", zap.String("date", date), zap.String("source", source), zap.Error(err))
			}
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "msg": err.Error()})
			return
		}

		out := presentAPOD(c, apod)
		tag := buildETag(out.Date, out.ImageURL)
		c.Header("ETag", tag)
		if c.GetHeader("If-None-Match") == tag {
			c.Status(http.StatusNotModified)
			return
		}

		apodRequestTotal.WithLabelValues("ok", source).Inc()
		apodRequestDuration.WithLabelValues(source).Observe(time.Since(started).Seconds())
		apodSourceTotal.WithLabelValues(source).Inc()
		c.JSON(http.StatusOK, out)
	})

	r.GET("/v1/apod/image", apiKeyAuthMiddleware(authKey), rateLimitMiddleware(apiLimiter), func(c *gin.Context) {
		l := requestLogger(c)
		date := c.Query("date")
		c.Header("Cache-Control", "public, max-age=3600")
		if date == "" {
			date = getNasaTime().Format("2006-01-02")
		}
		apod, _, err := getAPOD(c.Request.Context(), date)
		if err != nil {
			l.Warn("get apod for image failed", zap.String("date", date), zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "msg": err.Error()})
			return
		}
		if apod.MediaType != "image" {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "msg": "media type is not image"})
			return
		}
		origin := apod.OriginImage
		if origin == "" {
			origin = apod.ImageURL
		}
		tag := buildETag(apod.Date, origin)
		c.Header("ETag", tag)
		if c.GetHeader("If-None-Match") == tag {
			c.Status(http.StatusNotModified)
			return
		}
		imageStore.Serve(c, apod.Date, origin)
	})

	return r
}

func runServer() error {
	ginMode := configureGinMode()
	logger.Info("runtime configured", zap.String("app_env", appEnv()), zap.String("gin_mode", ginMode), zap.String("log_encoding", logEncoding()))
	registerMetrics()
	redisStore = NewRedisStore()
	imageStore = NewImageStore(getenv("IMAGE_CACHE_DIR", "cache/images"))
	apiLimiter = newRateLimiter()
	startPrefetchCron()
	startImageCleanupCron()
	startMemoryCleanupTicker()

	r := setupRouter()
	logger.Info("APOD service running", zap.String("addr", ":8080"))
	return r.Run(":8080")
}
