package image

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"go.uber.org/zap"
)

type staticRoundTripper struct {
	body string
}

func (s staticRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"image/jpeg"}},
		Body:          io.NopCloser(bytes.NewBufferString(s.body)),
		ContentLength: int64(len(s.body)),
	}, nil
}

func TestEnsureRejectsImagesOverMaxBytes(t *testing.T) {
	svc := NewService(Config{
		Dir:       t.TempDir(),
		MaxBytes:  4,
		Transport: staticRoundTripper{body: "12345"},
		Logger:    zap.NewNop(),
	})

	svc.Ensure(context.Background(), "2026-01-01", "https://example.test/image.jpg")

	if p := svc.localPath("2026-01-01"); p != "" {
		t.Fatalf("expected no cached file for oversized image, got %q", p)
	}
}
