package app

import (
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"

	"apod-server/internal/config"
	"apod-server/internal/fetch"
	"apod-server/internal/image"
	"apod-server/internal/server/api"
	"apod-server/internal/server/cron"
	"apod-server/internal/store"
)

// NewApp assembles all dependencies and returns a ready-to-run Server.
// cronStop is a cleanup function that the caller should invoke on shutdown.
func NewApp(logger *zap.Logger) (srv *api.Server, cronStop func(), err error) {
	// --- Auth keys ---
	authKey := strings.TrimSpace(config.Getenv("API_AUTH_KEY", "changeme"))
	if authKey == "changeme" {
		if config.IsProdEnv() {
			logger.Fatal("API_AUTH_KEY must be set in production", zap.String("current", "changeme"))
		} else {
			logger.Warn("using default API_AUTH_KEY, please override in production")
		}
	}
	metricsKey := strings.TrimSpace(config.Getenv("METRICS_AUTH_KEY", ""))
	if metricsKey == "" {
		metricsKey = authKey
		logger.Warn("METRICS_AUTH_KEY not set, falling back to API_AUTH_KEY")
	}

	// --- Shared transport ---
	sharedTransport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}

	// --- Store: memory cache ---
	memCache := store.NewMemoryCache(
		config.GetenvInt("MEMORY_CACHE_TTL_MINUTES", 180),
		config.GetenvInt("MEMORY_CACHE_MAX_ITEMS", 2000),
		10,
	)

	// --- Store: redis ---
	redisStore := store.NewRedisStore(store.RedisStoreConfig{
		Addr:       config.Getenv("REDIS_ADDR", "127.0.0.1:6379"),
		Password:   config.Getenv("REDIS_PASSWORD", ""),
		DB:         config.GetenvInt("REDIS_DB", 0),
		TTL:        30 * 24 * time.Hour,
		TodayTTL:   48 * time.Hour,
		FailWindow: 5 * time.Second,
		FailMax:    3,
		Logger:     logger,
	})

	// --- Image service ---
	var fetchSF singleflight.Group
	imgSem := make(chan struct{}, 5)
	imgSvc := image.NewService(image.Config{
		Dir:             config.Getenv("IMAGE_CACHE_DIR", "cache/images"),
		MaxFiles:        config.GetenvInt("IMAGE_CACHE_MAX_FILES", 1000),
		MaxAgeHours:     config.GetenvInt("IMAGE_CACHE_MAX_AGE_HOURS", 720),
		HotDays:         7,
		DownloadTimeout: 20 * time.Second,
		UserAgent:       "apod-mirror/1.0",
		Transport:       sharedTransport,
		SF:              &fetchSF,
		Sem:             imgSem,
		Logger:          logger,
	})

	// --- Fetch service ---
	fetchSvc := &fetch.Service{
		Cache:      memCache,
		KV:         redisStore,
		Image:      imgSvc,
		SF:         &fetchSF,
		NASAKey:    config.Getenv("NASA_API_KEY", "DEMO_KEY"),
		UserAgent:  "apod-mirror/1.0",
		HTTPClient: &http.Client{Timeout: 10 * time.Second, Transport: sharedTransport},
		Limiter:    rate.NewLimiter(1, 2),
		Logger:     logger,
	}

	// --- Prometheus metrics (explicit registration) ---
	reg := prometheus.DefaultRegisterer
	fetch.RegisterMetrics(reg)
	image.RegisterMetrics(reg)

	// --- Rate limiter ---
	rps := config.GetenvFloat64("API_RATE_LIMIT_RPS", 8)
	burst := config.GetenvInt("API_RATE_LIMIT_BURST", 16)
	if rps <= 0 {
		rps = 8
	}
	if burst <= 0 {
		burst = 16
	}
	apiLimiter := rate.NewLimiter(rate.Limit(rps), burst)

	// --- Demo limiter ---
	demoLimiter := cron.NewDemoIPLimiter(config.GetenvInt("DEMO_KEY_LIMIT_PER_24H", 5), 24*time.Hour)

	// --- Cron manager ---
	cronMgr := &cron.Manager{
		Fetch:  fetchSvc,
		Cache:  memCache,
		Image:  imgSvc,
		Logger: logger,
	}
	prefetchCron := cronMgr.StartPrefetchCron()
	cleanupCron := cronMgr.StartImageCleanupCron()
	cronMgr.StartMemoryCleanupTicker(config.GetenvInt("MEMORY_CACHE_CLEANUP_MINUTES", 15))
	cron.StartDemoLimiterCleanup(demoLimiter)

	cronStop = func() {
		prefetchCron.Stop()
		cleanupCron.Stop()
	}

	// --- HTTP server ---
	srv = api.NewServer(api.ServerConfig{
		Fetch:       fetchSvc,
		Cache:       memCache,
		KV:          redisStore,
		Image:       imgSvc,
		Logger:      logger,
		AuthKey:     authKey,
		MetricsKey:  metricsKey,
		DemoLimiter: demoLimiter,
		RateLimiter: apiLimiter,
		Addr:        ":8080",
	})

	return srv, cronStop, nil
}
