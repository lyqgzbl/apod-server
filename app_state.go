package main

import (
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
)

const (
	redisAPODPrefix   = "apod:data:"
	redisLastDate     = "apod:last_date"
	redisTTL          = 30 * 24 * time.Hour
	defaultUA         = "apod-mirror/1.0"
	redisFailWindow   = 5 * time.Second
	redisFailMax      = 3
	imageHotDays      = 7
	imageMaxAgeHours  = 24 * 30
	imageMaxFiles     = 1000
	memoryEvictStep   = 10
	imageDownloadTimeout = 20 * time.Second
	imageSemCount     = 5
)

var (
	envLoadErr error
	_          = func() struct{} {
		envLoadErr = loadDotEnv()
		return struct{}{}
	}()

	cache  = NewCache()
	logger *zap.Logger

	fetchSF singleflight.Group
	imgSem  = make(chan struct{}, imageSemCount)

	cacheHits   atomic.Uint64
	cacheMisses atomic.Uint64

	sharedTransport = &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}

	apiHTTPClient = &http.Client{Timeout: 10 * time.Second, Transport: sharedTransport}
	nasaLimiter   = rate.NewLimiter(1, 2)

	apodRequestTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "apod_request_total", Help: "Total APOD API requests"},
		[]string{"status", "source"},
	)
	apodRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "apod_request_duration_seconds", Help: "APOD handler latency", Buckets: prometheus.DefBuckets},
		[]string{"source"},
	)
	apodSourceTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "apod_source_total", Help: "Total APOD responses by source"},
		[]string{"source"},
	)
	apodFetchFailTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "apod_fetch_fail_total", Help: "Total APOD fetch failures by source"},
		[]string{"source"},
	)
	apodParseFailTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "apod_parse_fail_total", Help: "Total APOD parse/validation failures"},
		[]string{"stage"},
	)
	apodCacheHitTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "apod_cache_hit_total", Help: "Total APOD cache hits"},
		[]string{"layer"},
	)
	apodCacheMissTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "apod_cache_miss_total", Help: "Total APOD cache misses"},
	)
	apodCacheHitRatio = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{Name: "apod_cache_hit_ratio", Help: "APOD cache hit ratio"},
		func() float64 {
			hits := cacheHits.Load()
			misses := cacheMisses.Load()
			total := hits + misses
			if total == 0 {
				return 0
			}
			return float64(hits) / float64(total)
		},
	)
	imageCacheHitTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "apod_image_cache_hit_total", Help: "Total APOD image cache hits"},
	)
	imageCacheMissTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "apod_image_cache_miss_total", Help: "Total APOD image cache misses"},
	)
	imageDownloadTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "apod_image_download_total", Help: "Total APOD image download attempts"},
		[]string{"status"},
	)
	imageDownloadDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "apod_image_download_duration_seconds", Help: "APOD image download latency", Buckets: []float64{1, 2, 5, 10, 15, 20}},
		[]string{"status"},
	)

	redisStore *RedisStore
	imageStore *ImageStore
	apiLimiter *rate.Limiter
)
