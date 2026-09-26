package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"apod-server/internal/store"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixture(t *testing.T, name string) ([]byte, scienceArticle) {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var articles []scienceArticle
	if err := json.Unmarshal(data, &articles); err != nil {
		t.Fatal(err)
	}
	return data, articles[0]
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestScienceRealFixtures(t *testing.T) {
	for _, tc := range []struct{ name, date, title, copyright, media string }{
		{"science_image", "2026-09-25", "Globular Cluster Omega Centauri", "Javier O. Cadenas Parra", "NGC5139CadenasParra.jpg"},
		{"science_rollover", "2026-09-24", "The Ghosts of Five Supernovas", "", "5SNR_Auriga_2000.jpg"},
		{"science_historical", "2000-01-01", "The Millennium that Defines Universe", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, article := fixture(t, tc.name)
			got, err := parseScienceArticle(article, tc.date)
			if err != nil {
				t.Fatal(err)
			}
			if got.Title != tc.title || got.Date != tc.date || got.Copyright != tc.copyright || got.MediaType != "image" || !strings.Contains(got.ImageURL, tc.media) {
				t.Fatalf("unexpected APOD: %+v", got)
			}
			for _, unwanted := range []string{"Explanation:", "Tomorrow's picture", "APOD's main NASA site", "APOD's email"} {
				if strings.Contains(got.Explanation, unwanted) {
					t.Fatalf("explanation contains %q", unwanted)
				}
			}
			if got.OriginImage != got.ImageURL || got.ServiceVersion != "v1" {
				t.Fatalf("incompatible fields: %+v", got)
			}
			presented := PresentAPOD(httptest.NewRequest("GET", "https://mirror.example/v1/apod", nil), got)
			if presented.URL != "https://mirror.example/static/apod/"+tc.date+".jpg" || presented.HDURL != got.ImageURL {
				t.Fatalf("incompatible response: %+v", presented)
			}
		})
	}
}

func syntheticArticle(media string) scienceArticle {
	var article scienceArticle
	article.Link = "https://science.nasa.gov/image-article/apod-2026-september-01-example/"
	article.Title.Rendered = "APOD: 2026 September 1 &#8211; Stars &amp; Galaxies"
	article.Content.Rendered = `<img src="https://example.org/navigation.png"><iframe src="https://www.googletagmanager.com/ns.html"></iframe>
 <div class="hds-media-detail-hero"><h1>Stars &amp; Galaxies</h1><div class="media-detail-hero__media">` + media + `</div>
 <div class="media-detail-hero__description"><p><strong>Explanation:</strong> Stars &amp; galaxies.</p><p>Second paragraph<br>continues.</p><p><strong>Tomorrow's picture:</strong> unrelated</p></div>
 <table class="media-detail-hero__meta-table"><tr><th>Date</th><td>September 1, 2026</td></tr><tr><th>Credit &amp; Copyright:</th><td>Alice &amp; Bob</td></tr></table></div>`
	return article
}

func TestScienceMediaAndText(t *testing.T) {
	for _, tc := range []struct{ name, html, url, kind string }{
		{"relative", `<img src="../photo.jpg?w=2048&amp;h=1200">`, "https://science.nasa.gov/image-article/photo.jpg?w=2048&h=1200", "image"},
		{"root", `<img src="/photo.jpg">`, "https://science.nasa.gov/photo.jpg", "image"},
		{"protocol relative", `<img src="//assets.science.nasa.gov/photo.jpg">`, "https://assets.science.nasa.gov/photo.jpg", "image"},
		{"iframe", `<img src="/poster.jpg"><iframe src="https://www.youtube.com/embed/example"></iframe>`, "https://www.youtube.com/embed/example", "video"},
		{"video", `<video src="/clip.mp4" poster="/poster.jpg"></video>`, "https://science.nasa.gov/clip.mp4", "video"},
		{"video source", `<video><source src="/clip.webm"></video>`, "https://science.nasa.gov/clip.webm", "video"},
		{"rollover", `<img class="smd-image-rollover__image--secondary" src="/annotated.jpg"><img class="smd-image-rollover__image--primary" src="/original.jpg">`, "https://science.nasa.gov/original.jpg", "image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseScienceArticle(syntheticArticle(tc.html), "2026-09-01")
			if err != nil {
				t.Fatal(err)
			}
			if got.ImageURL != tc.url || got.MediaType != tc.kind {
				t.Fatalf("unexpected media: %+v", got)
			}
			if got.Title != "Stars & Galaxies" || got.Explanation != "Stars & galaxies. Second paragraph continues." || got.Copyright != "Alice & Bob" {
				t.Fatalf("unexpected text: %+v", got)
			}
		})
	}
}

func TestScienceRejectsInvalidContent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*scienceArticle)
	}{
		{"wrong title date", func(a *scienceArticle) {
			a.Title.Rendered = strings.ReplaceAll(a.Title.Rendered, "September 1", "September 2")
		}},
		{"wrong body date", func(a *scienceArticle) {
			a.Content.Rendered = strings.ReplaceAll(a.Content.Rendered, "September 1,", "September 2,")
		}},
		{"missing title date", func(a *scienceArticle) { a.Title.Rendered = "Something else" }},
		{"missing title", func(a *scienceArticle) {
			a.Content.Rendered = strings.ReplaceAll(a.Content.Rendered, "<h1>Stars &amp; Galaxies</h1>", "")
		}},
		{"missing explanation", func(a *scienceArticle) {
			a.Content.Rendered = strings.ReplaceAll(a.Content.Rendered, "media-detail-hero__description", "unrelated")
		}},
		{"navigation only", func(a *scienceArticle) {
			a.Content.Rendered = strings.ReplaceAll(a.Content.Rendered, `<img src="/photo.jpg">`, "")
		}},
		{"invalid media", func(a *scienceArticle) {
			a.Content.Rendered = strings.ReplaceAll(a.Content.Rendered, `src="/photo.jpg"`, `src="javascript:alert(1)"`)
		}},
		{"invalid base", func(a *scienceArticle) { a.Link = "https://other.example/image-article/example" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := syntheticArticle(`<img src="/photo.jpg">`)
			tc.change(&a)
			if _, err := parseScienceArticle(a, "2026-09-01"); err == nil {
				t.Fatal("expected validation failure")
			}
		})
	}
}

func TestScienceSearchUnpaddedAndSkipsWrongResult(t *testing.T) {
	s := newFailingService(store.NewMemoryCache(180, 2000, 10))
	calls := 0
	s.HTTPClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "science.nasa.gov" || r.URL.Path != "/wp-json/wp/v2/image-article" {
			t.Errorf("unexpected URL %s", r.URL)
		}
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 8*time.Second {
			t.Error("missing overall deadline")
		}
		if calls == 1 {
			if r.URL.Query().Get("search") != `"APOD: 2026 September 01"` {
				t.Errorf("unexpected search %s", r.URL)
			}
			return response(200, "[]"), nil
		}
		if r.URL.Query().Get("search") != `"APOD: 2026 September 1"` {
			t.Errorf("unexpected search %s", r.URL)
		}
		valid := syntheticArticle(`<video src="/clip.mp4"></video>`)
		wrong := valid
		wrong.Title.Rendered = "APOD: 2026 September 10 – Wrong day"
		body, _ := json.Marshal([]scienceArticle{wrong, valid})
		return response(200, string(body)), nil
	})
	got, err := s.fetchFromWeb(context.Background(), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || got.Date != "2026-09-01" || calls != 2 {
		t.Fatalf("got=%+v err=%v calls=%d", got, err, calls)
	}
}

func TestGetAPODScienceFallbackAndCache(t *testing.T) {
	cache := store.NewMemoryCache(180, 2000, 10)
	s := newFailingService(cache)
	calls := 0
	s.HTTPClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		switch r.URL.Path {
		case "/wp-json/wp/v2/apod-basic":
			return response(503, ""), nil
		case "/wp-json/wp/v2/image-article":
			body, _ := json.Marshal([]scienceArticle{syntheticArticle(`<video src="/clip.mp4"></video>`)})
			return response(200, string(body)), nil
		default:
			t.Errorf("unexpected upstream: %s", r.URL)
			return nil, errors.New("unexpected upstream")
		}
	})
	got, source, err := s.GetAPOD(context.Background(), "2026-09-01")
	if err != nil || source != "web" || got.Date != "2026-09-01" {
		t.Fatalf("got=%+v source=%s err=%v", got, source, err)
	}
	before := calls
	_, source, err = s.GetAPOD(context.Background(), "2026-09-01")
	if err != nil || source != "memory" || calls != before {
		t.Fatalf("cache miss: source=%s calls=%d err=%v", source, calls, err)
	}
}

func TestGetAPODScienceFailureDoesNotCache(t *testing.T) {
	wrong := syntheticArticle(`<video src="/clip.mp4"></video>`)
	wrong.Content.Rendered = strings.ReplaceAll(wrong.Content.Rendered, "September 1,", "September 2,")
	wrongBody, _ := json.Marshal([]scienceArticle{wrong})
	noMedia, _ := json.Marshal([]scienceArticle{syntheticArticle("")})
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"empty", 200, "[]"}, {"wrong date", 200, string(wrongBody)}, {"no media", 200, string(noMedia)}, {"http", 503, "unavailable"}, {"malformed", 200, "<html>error</html>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := store.NewMemoryCache(180, 2000, 10)
			s := newFailingService(cache)
			s.HTTPClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/wp-json/wp/v2/apod-basic" {
					return response(503, ""), nil
				}
				if r.URL.Host != "science.nasa.gov" {
					t.Errorf("unexpected upstream %s", r.URL)
				}
				return response(tc.status, tc.body), nil
			})
			if _, _, err := s.GetAPOD(context.Background(), "2026-09-01"); err == nil {
				t.Fatal("expected error")
			}
			if cache.Get("2026-09-01") != nil {
				t.Fatal("invalid content cached")
			}
			if s.KV.(*fakeKVStore).writes.Load() != 0 {
				t.Fatal("invalid content written to Redis")
			}
		})
	}
}

func TestScienceContextCancellation(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[timeout], func(t *testing.T) {
			s := newFailingService(store.NewMemoryCache(180, 2000, 10))
			ctx, cancel := context.WithCancel(context.Background())
			if timeout {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
			}
			defer cancel()
			s.HTTPClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if !timeout {
					cancel()
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			_, err := s.fetchFromWeb(ctx, time.Now())
			want := context.Canceled
			if timeout {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) {
				t.Fatalf("err=%v want=%v", err, want)
			}
		})
	}
}

func TestNASADateValidationAndPriority(t *testing.T) {
	for _, date := range []string{"2026-09-01", "2026-09-02"} {
		t.Run(date, func(t *testing.T) {
			s := newFailingService(store.NewMemoryCache(180, 2000, 10))
			scienceCalls := 0
			s.HTTPClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/wp-json/wp/v2/apod-basic" {
					body, _ := json.Marshal(nasaAPIResponse{Date: date, Title: "NASA", Explanation: strings.Repeat("Explanation ", 10), MediaType: "video", URL: "https://example.org/clip.mp4"})
					return response(200, string(body)), nil
				}
				if r.URL.Host != "science.nasa.gov" {
					t.Errorf("unexpected upstream %s", r.URL)
				}
				scienceCalls++
				body, _ := json.Marshal([]scienceArticle{syntheticArticle(`<video src="/clip.mp4"></video>`)})
				return response(200, string(body)), nil
			})
			got, source, err := s.GetAPOD(context.Background(), "2026-09-01")
			if err != nil {
				t.Fatal(err)
			}
			if date == "2026-09-01" && (source != "nasa" || scienceCalls != 0) {
				t.Fatal("NASA did not take priority")
			}
			if date != "2026-09-01" && (source != "web" || got.Date != "2026-09-01" || scienceCalls == 0) {
				t.Fatal("wrong API date accepted")
			}
		})
	}
}
