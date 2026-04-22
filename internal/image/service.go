package image

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	applog "apod-server/internal/log"

	"apod-server/internal/httputil"
)

// Config holds configuration for NewService.
type Config struct {
	Dir             string
	MaxFiles        int
	MaxAgeHours     int
	HotDays         int
	DownloadTimeout time.Duration
	UserAgent       string
	Transport       http.RoundTripper
	SF              *singleflight.Group
	Sem             chan struct{}
	Logger          *zap.Logger
}

// Service manages image downloading, caching and serving.
type Service struct {
	dir      string
	client   *http.Client
	maxFiles int
	maxAge   time.Duration
	hotDays  int
	timeout  time.Duration
	ua       string
	sf       *singleflight.Group
	sem      chan struct{}
	logger   *zap.Logger
}

// --- Prometheus metrics (self-managed) ---

var (
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
)

var registerOnce sync.Once

// RegisterMetrics registers image service metrics. Safe to call multiple times.
func RegisterMetrics(reg prometheus.Registerer) {
	registerOnce.Do(func() {
		reg.MustRegister(imageCacheHitTotal)
		reg.MustRegister(imageCacheMissTotal)
		reg.MustRegister(imageDownloadTotal)
		reg.MustRegister(imageDownloadDuration)
	})
}

// NewService creates an image Service.
func NewService(cfg Config) *Service {
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		cfg.Logger.Fatal("create image cache dir failed", zap.Error(err), zap.String("dir", cfg.Dir))
	}
	if cfg.DownloadTimeout <= 0 {
		cfg.DownloadTimeout = 20 * time.Second
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "apod-mirror/1.0"
	}
	if cfg.MaxFiles <= 0 {
		cfg.MaxFiles = 1000
	}
	if cfg.MaxAgeHours <= 0 {
		cfg.MaxAgeHours = 720
	}
	if cfg.HotDays <= 0 {
		cfg.HotDays = 7
	}
	transport := cfg.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Service{
		dir:      cfg.Dir,
		client:   &http.Client{Timeout: cfg.DownloadTimeout, Transport: transport},
		maxFiles: cfg.MaxFiles,
		maxAge:   time.Duration(cfg.MaxAgeHours) * time.Hour,
		hotDays:  cfg.HotDays,
		timeout:  cfg.DownloadTimeout,
		ua:       cfg.UserAgent,
		sf:       cfg.SF,
		sem:      cfg.Sem,
		logger:   cfg.Logger,
	}
}

// Dir returns the cache directory path.
func (s *Service) Dir() string { return s.dir }

func (s *Service) localPath(date string) string {
	files, err := filepath.Glob(filepath.Join(s.dir, date+".*"))
	if err != nil || len(files) == 0 {
		return ""
	}
	return files[0]
}

// Ensure downloads the image if not already cached locally.
func (s *Service) Ensure(ctx context.Context, date, originURL string) {
	if originURL == "" {
		return
	}
	l := applog.LoggerFromCtx(ctx)
	dlCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	_, _, _ = s.sf.Do("apod:image:"+date, func() (interface{}, error) {
		if s.localPath(date) != "" {
			return nil, nil
		}

		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-dlCtx.Done():
			imageDownloadTotal.WithLabelValues("skipped").Inc()
			return nil, dlCtx.Err()
		}

		start := time.Now()
		req, err := http.NewRequest(http.MethodGet, originURL, nil)
		if err != nil {
			l.Warn("build image request failed", zap.Error(err), zap.String("url", originURL))
			imageDownloadTotal.WithLabelValues("error").Inc()
			return nil, err
		}
		req = req.WithContext(dlCtx)
		req.Header.Set("User-Agent", s.ua)

		resp, err := s.client.Do(req)
		if err != nil {
			l.Warn("download image failed", zap.Error(err), zap.String("url", originURL))
			imageDownloadTotal.WithLabelValues("error").Inc()
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			err := fmt.Errorf("download image bad status: %d", resp.StatusCode)
			l.Warn("download image bad status", zap.Int("status", resp.StatusCode), zap.String("url", originURL))
			imageDownloadTotal.WithLabelValues("error").Inc()
			return nil, err
		}

		ext := ".jpg"
		if exts, _ := mime.ExtensionsByType(resp.Header.Get("Content-Type")); len(exts) > 0 {
			ext = exts[0]
		}
		if len(ext) > 8 || !strings.HasPrefix(ext, ".") {
			ext = ".jpg"
		}

		tmpFile, err := os.CreateTemp(s.dir, date+"-*.tmp")
		if err != nil {
			l.Warn("create temp image failed", zap.Error(err), zap.String("dir", s.dir))
			return nil, err
		}
		tmp := tmpFile.Name()
		finalPath := filepath.Join(s.dir, date+ext)

		if _, err := io.Copy(tmpFile, resp.Body); err != nil {
			_ = tmpFile.Close()
			_ = os.Remove(tmp)
			l.Warn("write image failed", zap.Error(err), zap.String("path", tmp))
			return nil, err
		}
		if err := tmpFile.Close(); err != nil {
			_ = os.Remove(tmp)
			l.Warn("close image file failed", zap.Error(err), zap.String("path", tmp))
			return nil, err
		}

		if err := os.Rename(tmp, finalPath); err != nil {
			_ = os.Remove(tmp)
			l.Warn("rename image failed", zap.Error(err), zap.String("path", finalPath))
			imageDownloadTotal.WithLabelValues("error").Inc()
			return nil, err
		}
		imageDownloadTotal.WithLabelValues("success").Inc()
		imageDownloadDuration.WithLabelValues("success").Observe(time.Since(start).Seconds())
		l.Info("image cached", zap.String("date", date), zap.String("path", finalPath))
		return nil, nil
	})
}

// Cleanup removes old/excess cached image files.
func (s *Service) Cleanup() {
	files, err := s.listFiles()
	if err != nil {
		s.logger.Warn("list image cache files failed", zap.Error(err), zap.String("dir", s.dir))
		return
	}

	removedByAge := 0
	if s.maxAge > 0 {
		deadline := time.Now().Add(-s.maxAge)
		for _, f := range files {
			if s.isHotFile(f.name) {
				continue
			}
			if f.modTime.Before(deadline) {
				if err := os.Remove(f.path); err == nil {
					removedByAge++
				}
			}
		}
		files, err = s.listFiles()
		if err != nil {
			s.logger.Warn("relist image cache files failed", zap.Error(err), zap.String("dir", s.dir))
			return
		}
	}

	removedByCount := 0
	if s.maxFiles > 0 && len(files) > s.maxFiles {
		cold := make([]cachedFile, 0, len(files))
		hot := 0
		for _, f := range files {
			if s.isHotFile(f.name) {
				hot++
				continue
			}
			cold = append(cold, f)
		}
		targetCold := s.maxFiles - hot
		if targetCold < 0 {
			targetCold = 0
		}
		if len(cold) > targetCold {
			sort.Slice(cold, func(i, j int) bool { return cold[i].modTime.Before(cold[j].modTime) })
			for i := 0; i < len(cold)-targetCold; i++ {
				if err := os.Remove(cold[i].path); err == nil {
					removedByCount++
				}
			}
		}
	}

	if removedByAge > 0 || removedByCount > 0 {
		s.logger.Info("image cache cleanup done", zap.Int("removed_by_age", removedByAge), zap.Int("removed_by_count", removedByCount), zap.Int("max_files", s.maxFiles), zap.Duration("max_age", s.maxAge))
	}
}

// Serve serves a cached image or downloads on-the-fly and serves it.
func (s *Service) Serve(c *gin.Context, date, fallbackURL string) {
	if p := s.localPath(date); p != "" {
		imageCacheHitTotal.Inc()
		c.File(p)
		return
	}
	imageCacheMissTotal.Inc()
	if fallbackURL != "" {
		s.Ensure(c.Request.Context(), date, fallbackURL)
		if p := s.localPath(date); p != "" {
			c.File(p)
			return
		}
		c.Redirect(http.StatusFound, fallbackURL)
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"code": 404, "msg": "image not found"})
}

func (s *Service) isHotFile(name string) bool {
	if len(name) < 10 {
		return false
	}
	t, err := time.Parse("2006-01-02", name[:10])
	if err != nil {
		return false
	}
	cutoff := httputil.GetNasaTime().AddDate(0, 0, -s.hotDays)
	cutoffDay := time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, cutoff.Location())
	return !t.Before(cutoffDay)
}

type cachedFile struct {
	path    string
	modTime time.Time
	name    string
}

func (s *Service) listFiles() ([]cachedFile, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	files := make([]cachedFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, cachedFile{path: filepath.Join(s.dir, entry.Name()), modTime: info.ModTime(), name: entry.Name()})
	}
	return files, nil
}
