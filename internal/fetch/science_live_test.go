package fetch

import (
	"context"
	"golang.org/x/time/rate"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Explicitly opt in: normal tests and CI never depend on NASA availability.
func TestScienceLiveSmoke(t *testing.T) {
	if os.Getenv("APOD_LIVE_TEST") != "1" {
		t.Skip("set APOD_LIVE_TEST=1 for NASA Science smoke checks")
	}
	s := &Service{HTTPClient: &http.Client{Timeout: 10 * time.Second}, UserAgent: "apod-mirror/1.0"}
	for _, dateStr := range []string{"2026-09-25", "2000-01-01"} {
		t.Run(dateStr, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			date, _ := time.Parse("2006-01-02", dateStr)
			got, err := s.fetchFromWeb(ctx, date)
			if err != nil {
				t.Fatal(err)
			}
			if got.Date != dateStr || got.MediaType != "image" || got.Explanation == "" {
				t.Fatalf("unexpected APOD: %+v", got)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, got.ImageURL, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("User-Agent", s.UserAgent)
			res, err := s.HTTPClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			data, err := io.ReadAll(io.LimitReader(res.Body, 512))
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != http.StatusOK || !strings.HasPrefix(http.DetectContentType(data), "image/") {
				t.Fatalf("image status=%d detected=%s", res.StatusCode, http.DetectContentType(data))
			}
			t.Logf("date=%s title=%q media=%s image=%s", got.Date, got.Title, got.MediaType, got.ImageURL)
		})
	}
}

func TestNASALiveSmoke(t *testing.T) {
	if os.Getenv("APOD_LIVE_TEST") != "1" {
		t.Skip("set APOD_LIVE_TEST=1 for NASA API smoke checks")
	}
	s := &Service{HTTPClient: &http.Client{Timeout: 10 * time.Second}, UserAgent: "apod-mirror/1.0", Limiter: rate.NewLimiter(1, 2)}
	got, err := s.fetchFromNASA(context.Background(), "2026-09-11")
	if err != nil {
		t.Fatal(err)
	}
	if got.Date != "2026-09-11" || got.MediaType != "image" || !strings.HasPrefix(got.ImageURL, "https://assets.science.nasa.gov/") {
		t.Fatalf("unexpected result: %+v", got)
	}
	res, err := s.HTTPClient.Get(got.ImageURL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 512))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(http.DetectContentType(data), "image/") {
		t.Fatal("asset is not a downloadable image")
	}
	t.Logf("apod-basic date=%s title=%q image=%s", got.Date, got.Title, got.ImageURL)
}
