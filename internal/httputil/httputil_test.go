package httputil

import (
	"net/http/httptest"
	"testing"
)

func TestBaseURLIgnoresForwardedHeadersFromUntrustedRemote(t *testing.T) {
	SetTrustedProxiesForRealIP([]string{"127.0.0.1"})

	req := httptest.NewRequest("GET", "http://service.local/v1/apod", nil)
	req.RemoteAddr = "198.51.100.10:12345"
	req.Host = "service.local"
	req.Header.Set("X-Forwarded-Host", "evil.example")
	req.Header.Set("X-Forwarded-Proto", "https")

	if got, want := BaseURL(req), "http://service.local"; got != want {
		t.Fatalf("BaseURL() = %q, want %q", got, want)
	}
}

func TestBaseURLTrustsForwardedHeadersFromTrustedRemote(t *testing.T) {
	SetTrustedProxiesForRealIP([]string{"127.0.0.1"})

	req := httptest.NewRequest("GET", "http://service.local/v1/apod", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Host = "service.local"
	req.Header.Set("X-Forwarded-Host", "apod.example")
	req.Header.Set("X-Forwarded-Proto", "https")

	if got, want := BaseURL(req), "https://apod.example"; got != want {
		t.Fatalf("BaseURL() = %q, want %q", got, want)
	}
}
