package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"

	"apod-server/internal/httputil"
	"apod-server/internal/image"
	applog "apod-server/internal/log"
	"apod-server/internal/model"
	"apod-server/internal/store"
)

// Service orchestrates APOD data fetching from NASA API, web scraping and caching.
type Service struct {
	Cache      store.Cache
	KV         store.KVStore
	Image      *image.Service
	SF         *singleflight.Group
	NASAKey    string
	UserAgent  string
	HTTPClient *http.Client
	Limiter    *rate.Limiter
	Logger     *zap.Logger
}

// --- Prometheus metrics (self-managed) ---

var (
	cacheHits   atomic.Uint64
	cacheMisses atomic.Uint64

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
)

var registerOnce sync.Once

// RegisterMetrics registers fetch service metrics. Safe to call multiple times.
func RegisterMetrics(reg prometheus.Registerer) {
	registerOnce.Do(func() {
		reg.MustRegister(apodRequestTotal)
		reg.MustRegister(apodRequestDuration)
		reg.MustRegister(apodSourceTotal)
		reg.MustRegister(apodFetchFailTotal)
		reg.MustRegister(apodParseFailTotal)
		reg.MustRegister(apodCacheHitTotal)
		reg.MustRegister(apodCacheMissTotal)
		reg.MustRegister(apodCacheHitRatio)
	})
}

// RequestTotal returns the request counter for use by HTTP handlers.
func RequestTotal() *prometheus.CounterVec { return apodRequestTotal }

// RequestDuration returns the request duration histogram.
func RequestDuration() *prometheus.HistogramVec { return apodRequestDuration }

// SourceTotal returns the source counter.
func SourceTotal() *prometheus.CounterVec { return apodSourceTotal }

type fetchResult struct {
	apod   *model.APOD
	source string
}

// --- GetAPOD ---

// GetAPOD returns APOD data for the given date (empty = today). Returns (apod, source, error).
func (s *Service) GetAPOD(ctx context.Context, dateStr string) (*model.APOD, string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var date time.Time
	var err error
	explicitDate := dateStr != ""
	if dateStr == "" {
		date = httputil.GetNasaTime()
		dateStr = date.Format("2006-01-02")
	} else {
		date, err = time.Parse("2006-01-02", dateStr)
		if err != nil {
			return nil, "invalid", err
		}
	}

	if data := s.Cache.Get(dateStr); data != nil {
		cacheHits.Add(1)
		apodCacheHitTotal.WithLabelValues("memory").Inc()
		return data, "memory", nil
	}
	if data := s.KV.Get(dateStr); data != nil {
		s.Cache.Set(dateStr, data)
		cacheHits.Add(1)
		apodCacheHitTotal.WithLabelValues("redis").Inc()
		return data, "redis", nil
	}

	cacheMisses.Add(1)
	apodCacheMissTotal.Inc()

	ch := s.SF.DoChan("apod:data:"+dateStr, func() (interface{}, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		apod, source, fetchErr := s.realFetchLogic(ctx, dateStr, date, !explicitDate)
		if fetchErr != nil {
			return nil, fetchErr
		}
		return fetchResult{apod: apod, source: source}, nil
	})

	var resCall singleflight.Result
	select {
	case <-ctx.Done():
		return nil, "canceled", ctx.Err()
	case resCall = <-ch:
	}
	if resCall.Err != nil {
		if errors.Is(resCall.Err, context.Canceled) || errors.Is(resCall.Err, context.DeadlineExceeded) {
			return nil, "canceled", resCall.Err
		}
		return nil, "failed", resCall.Err
	}
	if resCall.Val == nil {
		return nil, "failed", fmt.Errorf("empty singleflight result")
	}
	res, ok := resCall.Val.(fetchResult)
	if !ok || res.apod == nil {
		return nil, "failed", fmt.Errorf("invalid fetch result")
	}
	return res.apod, res.source, nil
}

func (s *Service) realFetchLogic(ctx context.Context, dateStr string, date time.Time, allowFallback bool) (*model.APOD, string, error) {
	l := applog.LoggerFromCtx(ctx)
	if err := ctx.Err(); err != nil {
		return nil, "canceled", err
	}

	if apod, err := s.fetchFromNASA(ctx, dateStr); err == nil {
		s.Cache.Set(dateStr, apod)
		s.KV.Set(dateStr, apod)
		if apod.MediaType == "image" {
			bgCtx := applog.WithLogger(context.Background(), l)
			go s.Image.Ensure(bgCtx, dateStr, apod.OriginImage)
		}
		return apod, "nasa", nil
	} else {
		l.Warn("fetch nasa failed", zap.String("date", dateStr), zap.Error(err))
	}

	if err := ctx.Err(); err != nil {
		return nil, "canceled", err
	}

	if apod, err := s.fetchFromWeb(ctx, date); err == nil {
		s.Cache.Set(dateStr, apod)
		s.KV.Set(dateStr, apod)
		if apod.MediaType == "image" {
			bgCtx := applog.WithLogger(context.Background(), l)
			go s.Image.Ensure(bgCtx, dateStr, apod.OriginImage)
		}
		return apod, "web", nil
	} else {
		l.Warn("fetch web failed", zap.String("date", dateStr), zap.Error(err))
	}

	if err := ctx.Err(); err != nil {
		return nil, "canceled", err
	}

	if allowFallback {
		if last := s.Cache.GetLast(); last != nil {
			return last, "memory-fallback", nil
		}
		if last := s.KV.GetLast(); last != nil {
			s.Cache.Set(last.Date, last)
			return last, "redis-fallback", nil
		}
	}
	apodFetchFailTotal.WithLabelValues("all").Inc()
	l.Warn("all apod sources failed", zap.String("date", dateStr))
	return nil, "failed", fmt.Errorf("all sources failed")
}
