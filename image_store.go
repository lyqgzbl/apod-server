package main

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
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type ImageStore struct {
	dir      string
	client   *http.Client
	maxFiles int
	maxAge   time.Duration
	hotDays  int
}

type cachedFile struct {
	path    string
	modTime time.Time
	name    string
}

func NewImageStore(dir string) *ImageStore {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger.Fatal("create image cache dir failed", zap.Error(err), zap.String("dir", dir))
	}
	maxFiles := getenvInt("IMAGE_CACHE_MAX_FILES", 1000)
	maxAgeHours := getenvInt("IMAGE_CACHE_MAX_AGE_HOURS", 24*30)
	return &ImageStore{
		dir:      dir,
		client:   &http.Client{Timeout: 15 * time.Second, Transport: sharedTransport},
		maxFiles: maxFiles,
		maxAge:   time.Duration(maxAgeHours) * time.Hour,
		hotDays:  imageHotDays,
	}
}

func (s *ImageStore) localPath(date string) string {
	files, err := filepath.Glob(filepath.Join(s.dir, date+".*"))
	if err != nil || len(files) == 0 {
		return ""
	}
	return files[0]
}

func (s *ImageStore) Ensure(ctx context.Context, date, originURL string) {
	if originURL == "" {
		return
	}
	l := loggerFromCtx(ctx)
	dlCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	_, _, _ = fetchSF.Do("apod:image:"+date, func() (interface{}, error) {
		if s.localPath(date) != "" {
			return nil, nil
		}

		select {
		case imgSem <- struct{}{}:
			defer func() { <-imgSem }()
		case <-dlCtx.Done():
			return nil, dlCtx.Err()
		}

		req, err := http.NewRequest(http.MethodGet, originURL, nil)
		if err != nil {
			l.Warn("build image request failed", zap.Error(err), zap.String("url", originURL))
			return nil, err
		}
		req = req.WithContext(dlCtx)
		req.Header.Set("User-Agent", defaultUA)

		resp, err := s.client.Do(req)
		if err != nil {
			l.Warn("download image failed", zap.Error(err), zap.String("url", originURL))
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			err := fmt.Errorf("download image bad status: %d", resp.StatusCode)
			l.Warn("download image bad status", zap.Int("status", resp.StatusCode), zap.String("url", originURL))
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
			return nil, err
		}
		l.Info("image cached", zap.String("date", date), zap.String("path", finalPath))
		return nil, nil
	})
}

func (s *ImageStore) isHotFile(name string) bool {
	if len(name) < 10 {
		return false
	}
	t, err := time.Parse("2006-01-02", name[:10])
	if err != nil {
		return false
	}
	cutoff := getNasaTime().AddDate(0, 0, -s.hotDays)
	cutoffDay := time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, cutoff.Location())
	return !t.Before(cutoffDay)
}

func (s *ImageStore) listFiles() ([]cachedFile, error) {
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

func (s *ImageStore) Cleanup() {
	files, err := s.listFiles()
	if err != nil {
		logger.Warn("list image cache files failed", zap.Error(err), zap.String("dir", s.dir))
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
			logger.Warn("relist image cache files failed", zap.Error(err), zap.String("dir", s.dir))
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
		logger.Info("image cache cleanup done", zap.Int("removed_by_age", removedByAge), zap.Int("removed_by_count", removedByCount), zap.Int("max_files", s.maxFiles), zap.Duration("max_age", s.maxAge))
	}
}

func (s *ImageStore) Serve(c *gin.Context, date, fallbackURL string) {
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
