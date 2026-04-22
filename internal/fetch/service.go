package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
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

// --- NASA API response ---

type nasaAPIResponse struct {
	Date           string `json:"date"`
	Title          string `json:"title"`
	Copyright      string `json:"copyright"`
	Explanation    string `json:"explanation"`
	URL            string `json:"url"`
	HDURL          string `json:"hdurl"`
	MediaType      string `json:"media_type"`
	ServiceVersion string `json:"service_version"`
}

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
		apod, source, fetchErr := s.realFetchLogic(ctx, dateStr, date)
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

func (s *Service) realFetchLogic(ctx context.Context, dateStr string, date time.Time) (*model.APOD, string, error) {
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

	if last := s.Cache.GetLast(); last != nil {
		return last, "memory-fallback", nil
	}
	if last := s.KV.GetLast(); last != nil {
		s.Cache.Set(last.Date, last)
		return last, "redis-fallback", nil
	}
	apodFetchFailTotal.WithLabelValues("all").Inc()
	l.Warn("all apod sources failed", zap.String("date", dateStr))
	return nil, "failed", fmt.Errorf("all sources failed")
}

// --- NASA API fetch ---

func (s *Service) fetchFromNASA(ctx context.Context, date string) (*model.APOD, error) {
	l := applog.LoggerFromCtx(ctx)
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := s.Limiter.Wait(fetchCtx); err != nil {
		apodFetchFailTotal.WithLabelValues("nasa_limiter").Inc()
		return nil, err
	}

	apiKey := s.NASAKey
	if apiKey == "" {
		apiKey = "DEMO_KEY"
	}
	url := fmt.Sprintf("https://api.nasa.gov/planetary/apod?api_key=%s&date=%s", neturl.QueryEscape(apiKey), date)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(fetchCtx)
	req.Header.Set("User-Agent", s.UserAgent)

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		apodFetchFailTotal.WithLabelValues("nasa").Inc()
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		apodFetchFailTotal.WithLabelValues("nasa").Inc()
		return nil, fmt.Errorf("NASA API error: status %d", resp.StatusCode)
	}

	var result nasaAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	imgURL := strings.TrimSpace(result.URL)
	apod := &model.APOD{
		Date:           strings.TrimSpace(result.Date),
		Title:          strings.TrimSpace(result.Title),
		Copyright:      strings.TrimSpace(result.Copyright),
		Explanation:    strings.TrimSpace(result.Explanation),
		ImageURL:       imgURL,
		OriginImage:    imgURL,
		MediaType:      strings.TrimSpace(result.MediaType),
		ServiceVersion: strings.TrimSpace(result.ServiceVersion),
	}
	if apod.ServiceVersion == "" {
		apod.ServiceVersion = "v1"
	}
	if len(apod.Explanation) < 50 {
		apodParseFailTotal.WithLabelValues("nasa").Inc()
		l.Warn("invalid nasa payload", zap.String("date", date))
		return nil, fmt.Errorf("invalid NASA data")
	}
	return apod, nil
}

// --- Web scrape fetch ---

func (s *Service) fetchFromWeb(ctx context.Context, date time.Time) (*model.APOD, error) {
	l := applog.LoggerFromCtx(ctx)
	fetchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	url := fmt.Sprintf("https://apod.nasa.gov/apod/ap%s.html", date.Format("060102"))
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(fetchCtx)
	req.Header.Set("User-Agent", s.UserAgent)

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		apodFetchFailTotal.WithLabelValues("web").Inc()
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		apodFetchFailTotal.WithLabelValues("web").Inc()
		return nil, fmt.Errorf("web error: %d", resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, err
	}

	apod := &model.APOD{Date: date.Format("2006-01-02")}
	apod.Title = strings.TrimSpace(doc.Find("center b").First().Text())
	apod.ImageURL, apod.MediaType = extractMedia(doc)
	apod.OriginImage = apod.ImageURL
	apod.Copyright = extractCopyright(doc)
	apod.Explanation = extractExplanation(doc)
	apod.ServiceVersion = "v1"

	if len(apod.Explanation) < 80 {
		apodParseFailTotal.WithLabelValues("web").Inc()
		l.Warn("parse apod page failed", zap.String("date", apod.Date))
		return nil, fmt.Errorf("parse failed")
	}
	return apod, nil
}

// --- HTML parsers ---

func extractMedia(doc *goquery.Document) (string, string) {
	if img := doc.Find("center img").First(); img.Length() > 0 {
		if src, ok := img.Attr("src"); ok {
			return "https://apod.nasa.gov/apod/" + src, "image"
		}
	}
	if iframe := doc.Find("iframe").First(); iframe.Length() > 0 {
		if src, ok := iframe.Attr("src"); ok {
			return src, "video"
		}
	}
	return "", "other"
}

func extractCopyright(doc *goquery.Document) string {
	var result string
	doc.Find("body *").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		text := strings.TrimSpace(s.Text())
		if !strings.Contains(strings.ToLower(text), "copyright") {
			return true
		}
		if c := parseCopyrightText(text); c != "" {
			result = c
			return false
		}
		if c := parseCopyrightText(s.Parent().Text()); c != "" {
			result = c
			return false
		}
		return true
	})
	if result != "" {
		return result
	}
	for _, line := range strings.Split(doc.Find("body").Text(), "\n") {
		if c := parseCopyrightText(line); c != "" {
			return c
		}
	}
	return ""
}

func parseCopyrightText(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r", " "))
	text = strings.TrimSpace(strings.Join(strings.Fields(text), " "))
	if text == "" {
		return ""
	}
	lower := strings.ToLower(text)
	idx := strings.Index(lower, "copyright")
	if idx == -1 {
		return ""
	}
	value := strings.TrimSpace(text[idx+len("copyright"):])
	value = strings.TrimLeft(value, ":：- ")
	if value == "" {
		return ""
	}
	markers := []string{"explanation:", "tomorrow"}
	for _, marker := range markers {
		if cut := strings.Index(strings.ToLower(value), marker); cut != -1 {
			value = strings.TrimSpace(value[:cut])
		}
	}
	return value
}

func extractExplanation(doc *goquery.Document) string {
	if exp := extractByKeyword(doc); exp != "" {
		return exp
	}
	if exp := extractByLength(doc); exp != "" {
		return exp
	}
	if exp := extractFallback(doc); exp != "" {
		return exp
	}
	return ""
}

func extractByKeyword(doc *goquery.Document) string {
	var result string
	doc.Find("b").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		if !strings.Contains(s.Text(), "Explanation") {
			return true
		}
		text := s.Parent().Text()
		if idx := strings.Index(text, "Explanation:"); idx != -1 {
			text = text[idx+len("Explanation:"):]
		}
		text = cleanText(text)
		if len(text) > 100 {
			result = text
			return false
		}
		return true
	})
	return result
}

func extractByLength(doc *goquery.Document) string {
	var best string
	doc.Find("p").Each(func(_ int, s *goquery.Selection) {
		text := cleanText(s.Text())
		if len(text) > len(best) {
			best = text
		}
	})
	if len(best) > 120 {
		return best
	}
	return ""
}

func extractFallback(doc *goquery.Document) string {
	text := cleanText(doc.Text())
	if len(text) > 200 {
		return text
	}
	return ""
}

func cleanText(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if idx := strings.Index(s, "Tomorrow"); idx != -1 {
		s = s[:idx]
	}
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}
