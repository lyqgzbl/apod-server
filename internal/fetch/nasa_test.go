package fetch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"apod-server/internal/store"
)

func TestNASABasicEndpointAndImageMapping(t *testing.T) {
	body, err := os.ReadFile("testdata/nasa_basic.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "key+with&special=value"} {
		t.Run(key, func(t *testing.T) {
			s := newFailingService(store.NewMemoryCache(180, 2000, 10))
			s.NASAKey = key
			s.HTTPClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Scheme != "https" || r.URL.Host != "science.nasa.gov" || r.URL.Path != "/wp-json/wp/v2/apod-basic" {
					t.Fatalf("unexpected endpoint %s", r.URL)
				}
				wantKey := key
				if wantKey == "" {
					wantKey = "DEMO_KEY"
				}
				if r.URL.Query().Get("api_key") != wantKey || r.URL.Query().Get("date") != "2026-09-11" || len(r.URL.Query()) != 2 {
					t.Fatal("unexpected query parameters")
				}
				return response(200, string(body)), nil
			})
			got, err := s.fetchFromNASA(context.Background(), "2026-09-11")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(got.ImageURL, "https://assets.science.nasa.gov/") || got.OriginImage != got.ImageURL {
				t.Fatalf("article used as image: %+v", got)
			}
			if got.Copyright != "Aldo Zanetti" || strings.ContainsAny(got.Explanation, "<>") || strings.HasPrefix(got.Explanation, "Explanation:") {
				t.Fatalf("HTML leaked: %+v", got)
			}
			out := PresentAPOD(httptest.NewRequest("GET", "https://mirror.example/v1/apod", nil), got)
			if out.URL != "https://mirror.example/static/apod/2026-09-11.jpg" || out.HDURL != got.ImageURL || out.ServiceVersion != "v1" {
				t.Fatalf("unexpected response: %+v", out)
			}
		})
	}
}

func TestNASABasicCollectionDateSelection(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`[{"date":"2026-09-25"},{"date":"2026-09-11","title":"match"}]`, true},
		{`{"date":"2026-09-11","title":"match"}`, true},
		{`[{"date":"2026-09-25"}]`, false},
		{`{"date":"2026-09-25"}`, false},
		{`[]`, false}, {`null`, false}, {`{"code":"rest_error"}`, false}, {`<html>error</html>`, false},
	} {
		result, err := decodeNASAResponse([]byte(tc.body), "2026-09-11")
		if (err == nil) != tc.valid {
			t.Fatalf("body=%s err=%v", tc.body, err)
		}
		if tc.valid && (result.Date != "2026-09-11" || result.Title != "match") {
			t.Fatalf("wrong record: %+v", result)
		}
	}
}

func TestNASABasicMediaValidation(t *testing.T) {
	for _, tc := range []struct {
		name, kind, url, hdurl string
		valid                  bool
	}{
		{"image needs hdurl", "image", "https://science.nasa.gov/image-article/example/", "", false},
		{"invalid asset", "image", "https://example.org/article", "javascript:bad", false},
		{"article hdurl", "image", "https://example.org/article", "https://science.nasa.gov/image-article/example/", false},
		{"video", "video", "https://www.youtube.com/embed/example", "", true},
		{"video article", "video", "https://science.nasa.gov/image-article/example/", "", false},
		{"unsupported", "other", "https://example.org/a", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newFailingService(store.NewMemoryCache(180, 2000, 10))
			s.HTTPClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body, _ := json.Marshal(nasaAPIResponse{Date: "2026-09-11", Title: "Example", Explanation: "A valid brief explanation.", MediaType: tc.kind, URL: tc.url, HDURL: tc.hdurl})
				return response(200, string(body)), nil
			})
			got, err := s.fetchFromNASA(context.Background(), "2026-09-11")
			if (err == nil) != tc.valid {
				t.Fatalf("got=%+v err=%v", got, err)
			}
		})
	}
}
