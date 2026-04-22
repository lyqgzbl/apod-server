package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"apod-server/internal/fetch"
	"apod-server/internal/httputil"
	"apod-server/internal/image"
	"apod-server/internal/store"
)

const invalidDateErrorMessage = "Invalid date format, expected YYYY-MM-DD"

func badDateRequest(c *gin.Context) {
	c.JSON(http.StatusBadRequest, gin.H{"error": invalidDateErrorMessage})
}

func healthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func readinessHandler(kv store.KVStore, img *image.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()

		redisErr := kv.Ready(ctx)
		fsErr := checkFSWritable(img.Dir())
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

func apodHandler(fetchSvc *fetch.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		l := RequestLogger(c)
		started := time.Now()
		date := strings.TrimSpace(c.Query("date"))
		if date != "" && !httputil.IsValidISODate(date) {
			badDateRequest(c)
			return
		}
		c.Header("Cache-Control", "public, max-age=3600")

		apod, source, err := fetchSvc.GetAPOD(c.Request.Context(), date)
		if err != nil {
			if source == "invalid" {
				badDateRequest(c)
				return
			}
			fetch.RequestTotal().WithLabelValues("error", source).Inc()
			fetch.RequestDuration().WithLabelValues(source).Observe(time.Since(started).Seconds())
			if source == "failed" {
				l.Error("get apod failed", zap.String("date", date), zap.String("source", source), zap.Error(err))
			} else {
				l.Warn("get apod failed", zap.String("date", date), zap.String("source", source), zap.Error(err))
			}
			c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "msg": err.Error()})
			return
		}

		out := fetch.PresentAPOD(c.Request, apod)
		tag := httputil.BuildETag(out.Date, out.URL)
		c.Header("ETag", tag)
		if c.GetHeader("If-None-Match") == tag {
			c.Status(http.StatusNotModified)
			return
		}

		fetch.RequestTotal().WithLabelValues("ok", source).Inc()
		fetch.RequestDuration().WithLabelValues(source).Observe(time.Since(started).Seconds())
		fetch.SourceTotal().WithLabelValues(source).Inc()
		c.JSON(http.StatusOK, out)
	}
}

func imageRedirectHandler(fetchSvc *fetch.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		l := RequestLogger(c)
		date := strings.TrimSpace(c.Query("date"))
		if date == "" {
			date = httputil.GetNasaTime().Format("2006-01-02")
		} else if !httputil.IsValidISODate(date) {
			badDateRequest(c)
			return
		}
		apod, source, err := fetchSvc.GetAPOD(c.Request.Context(), date)
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
	}
}

func staticImageHandler(fetchSvc *fetch.Service, img *image.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		filename := strings.TrimSpace(c.Param("filename"))
		lowerFilename := strings.ToLower(filename)
		if !strings.HasSuffix(lowerFilename, ".jpg") {
			badDateRequest(c)
			return
		}
		date := strings.TrimSpace(filename[:len(filename)-4])
		if !httputil.IsValidISODate(date) {
			badDateRequest(c)
			return
		}

		apod, source, err := fetchSvc.GetAPOD(c.Request.Context(), date)
		if err != nil {
			if source == "invalid" {
				badDateRequest(c)
				return
			}
			RequestLogger(c).Warn("get apod for static image failed", zap.String("date", date), zap.String("source", source), zap.Error(err))
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
		img.Serve(c, apod.Date, origin)
	}
}
