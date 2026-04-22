package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
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

const demoAPIKey = "DEMO_KEY"
const invalidDateErrorMessage = "Invalid date format, expected YYYY-MM-DD"

type demoUsage struct {
	count     int
	windowBgn time.Time
}

type demoIPLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	records map[string]demoUsage
}

var demoLimiter *demoIPLimiter

func newDemoIPLimiter(limit int, window time.Duration) *demoIPLimiter {
	if limit <= 0 {
		limit = 5
	}
	if window <= 0 {
		window = 24 * time.Hour
	}
	return &demoIPLimiter{limit: limit, window: window, records: make(map[string]demoUsage, 128)}
}

func (d *demoIPLimiter) allow(ip string) bool {
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
			if now.Sub(rec.windowBgn) >= d.window {
				delete(d.records, k)
			}
		}
	}

	rec, ok := d.records[ip]
	if !ok || now.Sub(rec.windowBgn) >= d.window {
		d.records[ip] = demoUsage{count: 1, windowBgn: now}
		return true
	}

	if rec.count >= d.limit {
		return false
	}
	rec.count++
	d.records[ip] = rec
	return true
}

func (d *demoIPLimiter) rollback(ip string) {
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
	if now.Sub(rec.windowBgn) >= d.window {
		delete(d.records, ip)
		return
	}
	if rec.count <= 1 {
		delete(d.records, ip)
		return
	}
	rec.count--
	d.records[ip] = rec
}

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
	prometheus.MustRegister(imageDownloadTotal)
	prometheus.MustRegister(imageDownloadDuration)
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
		ip := realIP(c.Request)
		provided, mode, msg := readAPIKeyFromHeader(c)
		if msg != "" {
			l.Warn("auth failed", zap.String("method", c.Request.Method), zap.String("ip", ip), zap.String("path", c.Request.URL.Path), zap.Int("status", http.StatusUnauthorized), zap.String("reason", msg))
			c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "msg": msg})
			c.Abort()
			return
		}

		if mode == "demo" {
			if demoLimiter != nil && !demoLimiter.allow(ip) {
				l.Warn("demo key quota exceeded", zap.String("method", c.Request.Method), zap.String("ip", ip), zap.String("path", c.Request.URL.Path), zap.Int("status", http.StatusTooManyRequests))
				c.JSON(http.StatusTooManyRequests, gin.H{"code": 429, "msg": fmt.Sprintf("DEMO_KEY limit exceeded: %d requests per 24 hours for this IP", demoLimiter.limit)})
				c.Abort()
				return
			}
			c.Next()
			if demoLimiter != nil && c.Writer.Status() != http.StatusOK {
				demoLimiter.rollback(ip)
			}
			return
		}

		if subtle.ConstantTimeEq(int32(len(provided)), int32(len(requiredBytes))) != 1 || subtle.ConstantTimeCompare([]byte(provided), requiredBytes) != 1 {
			l.Warn("auth failed", zap.String("method", c.Request.Method), zap.String("ip", ip), zap.String("path", c.Request.URL.Path), zap.Int("status", http.StatusUnauthorized), zap.String("reason", "invalid API key"))
			c.JSON(http.StatusUnauthorized, gin.H{"code": 401, "msg": "invalid API key"})
			c.Abort()
			return
		}
		c.Next()
	}
}

func readAPIKeyFromHeader(c *gin.Context) (string, string, string) {
	authorization := strings.TrimSpace(c.GetHeader("Authorization"))
	if authorization != "" {
		parts := strings.SplitN(authorization, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return "", "", "Authorization must use Bearer token"
		}
		token := strings.TrimSpace(parts[1])
		if token == "" {
			return "", "", "Bearer token is required"
		}
		return token, "header", ""
	}

	apiKey := strings.TrimSpace(c.GetHeader("X-API-Key"))
	if apiKey != "" {
		return apiKey, "header", ""
	}

	return demoAPIKey, "demo", ""
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
		ip := realIP(c.Request)
		fields := []zap.Field{
			zap.String("method", method),
			zap.String("path", path),
			zap.Int("status", status),
			latencyFieldForAccessLog(latency),
			zap.String("ip", ip),
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

func prefetchWithRetry(traceID string, maxRetries int) {
	date := getNasaTime().Format("2006-01-02")
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			logger.Info("cron prefetch retry scheduled", zap.String("date", date), zap.Int("attempt", attempt+1), zap.Int("max_attempts", maxRetries+1))
			time.Sleep(5 * time.Minute)
			date = getNasaTime().Format("2006-01-02")
		}
		ctx, cancel := context.WithTimeout(withLogger(context.Background(), logger.With(zap.String("trace_id", traceID))), 20*time.Second)
		apod, source, err := getAPOD(ctx, date)
		cancel()
		if err == nil {
			logger.Info("prefetch success", zap.String("trace_id", traceID), zap.String("date", apod.Date), zap.String("source", source), zap.Int("attempt", attempt+1))
			return
		}
		logger.Warn("prefetch attempt failed", zap.String("trace_id", traceID), zap.String("date", date), zap.Int("attempt", attempt+1), zap.Int("max_attempts", maxRetries+1), zap.Error(err))
	}
	logger.Warn("prefetch exhausted all retries", zap.String("trace_id", traceID), zap.String("date", date), zap.Int("max_attempts", maxRetries+1))
}

func startPrefetchCron() *cron.Cron {
	loc, _ := time.LoadLocation("America/New_York")
	c := cron.New(cron.WithLocation(loc))
	_, err := c.AddFunc("30 0 * * *", func() {
		prefetchWithRetry("cron-prefetch", 3)
	})
	if err != nil {
		logger.Fatal("start cron failed", zap.Error(err))
	}
	c.Start()

	go func() {
		time.Sleep(3 * time.Second)
		prefetchWithRetry("startup-prefetch", 0)
	}()

	return c
}

func startImageCleanupCron() *cron.Cron {
	loc, _ := time.LoadLocation("America/New_York")
	c := cron.New(cron.WithLocation(loc))
	_, err := c.AddFunc("20 3 * * *", func() { imageStore.Cleanup() })
	if err != nil {
		logger.Fatal("start image cleanup cron failed", zap.Error(err))
	}
	c.Start()
	go imageStore.Cleanup()
	return c
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

func isValidISODate(date string) bool {
	if date == "" {
		return false
	}
	_, err := time.Parse("2006-01-02", date)
	return err == nil
}

func badDateRequest(c *gin.Context) {
	c.JSON(http.StatusBadRequest, gin.H{"error": invalidDateErrorMessage})
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
		if isProdEnv() {
			logger.Fatal("API_AUTH_KEY must be set in production", zap.String("current", "changeme"))
		} else {
			logger.Warn("using default API_AUTH_KEY, please override in production")
		}
	}

	metricsKey := strings.TrimSpace(getenv("METRICS_AUTH_KEY", ""))
	if metricsKey == "" {
		metricsKey = authKey
		logger.Warn("METRICS_AUTH_KEY not set, falling back to API_AUTH_KEY")
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
	setTrustedProxiesForRealIP(proxies)
	if err := r.SetTrustedProxies(proxies); err != nil {
		logger.Warn("set trusted proxies failed", zap.Error(err), zap.Strings("trusted_proxies", proxies))
	}

	r.Use(gzip.Gzip(gzip.DefaultCompression))
	r.Use(traceIDMiddleware())
	r.Use(accessLogMiddleware())
	r.Use(ginzap.RecoveryWithZap(logger, true))

	r.GET("/metrics", apiKeyAuthMiddleware(metricsKey), gin.WrapH(promhttp.Handler()))
	r.GET("/healthz", healthHandler)
	r.GET("/readyz", readinessHandler)
	r.GET("/static/apod/:filename", func(c *gin.Context) {
		filename := strings.TrimSpace(c.Param("filename"))
		lowerFilename := strings.ToLower(filename)
		if !strings.HasSuffix(lowerFilename, ".jpg") {
			badDateRequest(c)
			return
		}
		date := strings.TrimSpace(filename[:len(filename)-4])
		if !isValidISODate(date) {
			badDateRequest(c)
			return
		}

		apod, source, err := getAPOD(c.Request.Context(), date)
		if err != nil {
			if source == "invalid" {
				badDateRequest(c)
				return
			}
			requestLogger(c).Warn("get apod for static image failed", zap.String("date", date), zap.String("source", source), zap.Error(err))
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
		c.Header("Cache-Control", "public, max-age=86400")
		imageStore.Serve(c, apod.Date, origin)
	})

	r.GET("/v1/apod", apiKeyAuthMiddleware(authKey), rateLimitMiddleware(apiLimiter), func(c *gin.Context) {
		l := requestLogger(c)
		started := time.Now()
		date := strings.TrimSpace(c.Query("date"))
		if date != "" && !isValidISODate(date) {
			badDateRequest(c)
			return
		}
		c.Header("Cache-Control", "public, max-age=3600")

		apod, source, err := getAPOD(c.Request.Context(), date)
		if err != nil {
			if source == "invalid" {
				badDateRequest(c)
				return
			}
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
		tag := buildETag(out.Date, out.URL)
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
		date := strings.TrimSpace(c.Query("date"))
		if date == "" {
			date = getNasaTime().Format("2006-01-02")
		} else if !isValidISODate(date) {
			badDateRequest(c)
			return
		}
		apod, source, err := getAPOD(c.Request.Context(), date)
		if err != nil {
			if source == "invalid" {
				badDateRequest(c)
				return
			}
			l.Warn("get apod for image failed", zap.String("date", date), zap.String("source", source), zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "msg": err.Error()})
			return
		}
		if apod.MediaType != "image" {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "msg": "media type is not image"})
			return
		}
		c.Header("Cache-Control", "public, max-age=86400")
		c.Redirect(http.StatusFound, fmt.Sprintf("/static/apod/%s.jpg", apod.Date))
	})

	return r
}

func startDemoLimiterCleanup() {
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			if demoLimiter == nil {
				continue
			}
			now := time.Now()
			demoLimiter.mu.Lock()
			for k, rec := range demoLimiter.records {
				if now.Sub(rec.windowBgn) >= demoLimiter.window {
					delete(demoLimiter.records, k)
				}
			}
			demoLimiter.mu.Unlock()
		}
	}()
}

func runServer() error {
	ginMode := configureGinMode()
	logger.Info("runtime configured", zap.String("app_env", appEnv()), zap.String("gin_mode", ginMode), zap.String("log_encoding", logEncoding()))
	demoLimiter = newDemoIPLimiter(getenvInt("DEMO_KEY_LIMIT_PER_24H", 5), 24*time.Hour)
	registerMetrics()
	redisStore = NewRedisStore()
	imageStore = NewImageStore(getenv("IMAGE_CACHE_DIR", "cache/images"))
	apiLimiter = newRateLimiter()
	prefetchCron := startPrefetchCron()
	cleanupCron := startImageCleanupCron()
	startMemoryCleanupTicker()
	startDemoLimiterCleanup()

	r := setupRouter()
	srv := &http.Server{Addr: ":8080", Handler: r}

	go func() {
		logger.Info("APOD service running", zap.String("addr", ":8080"))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server listen failed", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	logger.Info("received shutdown signal", zap.String("signal", sig.String()))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	prefetchCron.Stop()
	cleanupCron.Stop()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("server shutdown error", zap.Error(err))
		return err
	}
	logger.Info("server exited gracefully")
	return nil
}
