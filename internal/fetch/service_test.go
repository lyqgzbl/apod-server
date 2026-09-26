package fetch

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"

	"apod-server/internal/model"
	"apod-server/internal/store"
)

type fakeKVStore struct {
	last   *model.APOD
	writes atomic.Int64
}

func (f *fakeKVStore) Get(string) *model.APOD {
	return nil
}

func (f *fakeKVStore) Set(string, *model.APOD) { f.writes.Add(1) }

func (f *fakeKVStore) GetLast() *model.APOD {
	if f.last == nil {
		return nil
	}
	cp := *f.last
	return &cp
}

func (f *fakeKVStore) Ready(context.Context) error {
	return nil
}

type failingRoundTripper struct{}

func (f failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("upstream unavailable")
}

func newFailingService(cache store.Cache) *Service {
	return &Service{
		Cache:      cache,
		KV:         &fakeKVStore{},
		SF:         &singleflight.Group{},
		HTTPClient: &http.Client{Transport: failingRoundTripper{}},
		Limiter:    rate.NewLimiter(rate.Limit(1000), 1000),
		Logger:     zap.NewNop(),
	}
}

func TestGetAPODTodayCanUseLastFallback(t *testing.T) {
	cache := store.NewMemoryCache(180, 2000, 10)
	cache.Set("2000-01-01", &model.APOD{
		Date:           "2000-01-01",
		Title:          "Fallback",
		Explanation:    strings.Repeat("x", 100),
		MediaType:      "image",
		ServiceVersion: "v1",
	})

	apod, source, err := newFailingService(cache).GetAPOD(context.Background(), "")
	if err != nil {
		t.Fatalf("expected fallback APOD, got error: %v", err)
	}
	if apod.Date != "2000-01-01" || source != "memory-fallback" {
		t.Fatalf("expected memory fallback for 2000-01-01, got date=%q source=%q", apod.Date, source)
	}
}

func TestGetAPODExplicitDateDoesNotUseLastFallback(t *testing.T) {
	cache := store.NewMemoryCache(180, 2000, 10)
	cache.Set("2000-01-01", &model.APOD{
		Date:           "2000-01-01",
		Title:          "Fallback",
		Explanation:    strings.Repeat("x", 100),
		MediaType:      "image",
		ServiceVersion: "v1",
	})

	apod, source, err := newFailingService(cache).GetAPOD(context.Background(), "2000-01-02")
	if err == nil {
		t.Fatalf("expected explicit date error, got apod=%v source=%q", apod, source)
	}
	if source != "failed" {
		t.Fatalf("expected source failed, got %q", source)
	}
}

func TestGetAPODInvalidDate(t *testing.T) {
	_, source, err := newFailingService(store.NewMemoryCache(180, 2000, 10)).GetAPOD(context.Background(), "not-a-date")
	if err == nil {
		t.Fatal("expected invalid date error")
	}
	if source != "invalid" {
		t.Fatalf("expected source invalid, got %q", source)
	}
}
