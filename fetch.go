package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

type fetchResult struct {
	apod   *APOD
	source string
}

func fetchFromNASA(ctx context.Context, date string) (*APOD, error) {
	l := loggerFromCtx(ctx)
	fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := nasaLimiter.Wait(fetchCtx); err != nil {
		apodFetchFailTotal.WithLabelValues("nasa_limiter").Inc()
		return nil, err
	}

	apiKey := getenv("NASA_API_KEY", "DEMO_KEY")
	url := fmt.Sprintf("https://api.nasa.gov/planetary/apod?api_key=%s&date=%s", neturl.QueryEscape(apiKey), date)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(fetchCtx)
	req.Header.Set("User-Agent", defaultUA)

	resp, err := apiHTTPClient.Do(req)
	if err != nil {
		apodFetchFailTotal.WithLabelValues("nasa").Inc()
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		apodFetchFailTotal.WithLabelValues("nasa").Inc()
		return nil, fmt.Errorf("NASA API error")
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	imgURL := getString(result["url"])
	apod := &APOD{
		Date:           getString(result["date"]),
		Title:          getString(result["title"]),
		Copyright:      getString(result["copyright"]),
		Explanation:    getString(result["explanation"]),
		ImageURL:       imgURL,
		OriginImage:    imgURL,
		MediaType:      getString(result["media_type"]),
		ServiceVersion: getString(result["service_version"]),
	}
	if apod.ServiceVersion == "" {
		apod.ServiceVersion = "v1"
	}
	if len(apod.Explanation) < 50 {
		apodParseFailTotal.WithLabelValues("nasa").Inc()
		l.Error("invalid nasa payload", zap.String("date", date))
		return nil, fmt.Errorf("invalid NASA data")
	}
	return apod, nil
}

func fetchFromWeb(ctx context.Context, date time.Time) (*APOD, error) {
	l := loggerFromCtx(ctx)
	fetchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	url := fmt.Sprintf("https://apod.nasa.gov/apod/ap%s.html", date.Format("060102"))
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(fetchCtx)
	req.Header.Set("User-Agent", defaultUA)

	resp, err := apiHTTPClient.Do(req)
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

	apod := &APOD{Date: date.Format("2006-01-02")}
	apod.Title = strings.TrimSpace(doc.Find("center b").First().Text())
	apod.ImageURL, apod.MediaType = extractMedia(doc)
	apod.OriginImage = apod.ImageURL
	apod.Copyright = extractCopyright(doc)
	apod.Explanation = extractExplanation(doc)
	apod.ServiceVersion = "v1"

	if len(apod.Explanation) < 80 {
		apodParseFailTotal.WithLabelValues("web").Inc()
		l.Error("parse apod page failed", zap.String("date", apod.Date))
		return nil, fmt.Errorf("parse failed")
	}
	return apod, nil
}

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

func realFetchLogic(ctx context.Context, dateStr string, date time.Time) (*APOD, string, error) {
	l := loggerFromCtx(ctx)
	if err := ctx.Err(); err != nil {
		return nil, "canceled", err
	}

	if apod, err := fetchFromNASA(ctx, dateStr); err == nil {
		cache.Set(dateStr, apod)
		redisStore.Set(dateStr, apod)
		if apod.MediaType == "image" {
			go imageStore.Ensure(ctx, dateStr, apod.OriginImage)
		}
		return apod, "nasa", nil
	} else {
		l.Warn("fetch nasa failed", zap.String("date", dateStr), zap.Error(err))
	}

	if err := ctx.Err(); err != nil {
		return nil, "canceled", err
	}

	if apod, err := fetchFromWeb(ctx, date); err == nil {
		cache.Set(dateStr, apod)
		redisStore.Set(dateStr, apod)
		if apod.MediaType == "image" {
			go imageStore.Ensure(ctx, dateStr, apod.OriginImage)
		}
		return apod, "web", nil
	} else {
		l.Warn("fetch web failed", zap.String("date", dateStr), zap.Error(err))
	}

	if last := cache.GetLast(); last != nil {
		return last, "memory-fallback", nil
	}
	if last := redisStore.GetLast(); last != nil {
		cache.Set(last.Date, last)
		return last, "redis-fallback", nil
	}
	apodFetchFailTotal.WithLabelValues("all").Inc()
	l.Error("all apod sources failed", zap.String("date", dateStr))
	return nil, "failed", fmt.Errorf("all sources failed")
}

func getAPOD(ctx context.Context, dateStr string) (*APOD, string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var date time.Time
	var err error
	if dateStr == "" {
		date = getNasaTime()
		dateStr = date.Format("2006-01-02")
	} else {
		date, err = time.Parse("2006-01-02", dateStr)
		if err != nil {
			return nil, "invalid", err
		}
	}

	if data := cache.Get(dateStr); data != nil {
		cacheHits.Add(1)
		apodCacheHitTotal.WithLabelValues("memory").Inc()
		return data, "memory", nil
	}
	if data := redisStore.Get(dateStr); data != nil {
		cache.Set(dateStr, data)
		cacheHits.Add(1)
		apodCacheHitTotal.WithLabelValues("redis").Inc()
		return data, "redis", nil
	}

	cacheMisses.Add(1)
	apodCacheMissTotal.Inc()

	ch := fetchSF.DoChan("apod:data:"+dateStr, func() (interface{}, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if data := cache.Get(dateStr); data != nil {
			return fetchResult{apod: data, source: "memory"}, nil
		}
		if data := redisStore.Get(dateStr); data != nil {
			cache.Set(dateStr, data)
			return fetchResult{apod: data, source: "redis"}, nil
		}
		apod, source, fetchErr := realFetchLogic(ctx, dateStr, date)
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

func presentAPOD(c *gin.Context, apod *APOD) *APODResponse {
	out := &APODResponse{
		Copyright:      apod.Copyright,
		Date:           apod.Date,
		Explanation:    apod.Explanation,
		HDURL:          apod.OriginImage,
		MediaType:      apod.MediaType,
		ServiceVersion: apod.ServiceVersion,
		Title:          apod.Title,
		URL:            apod.ImageURL,
	}
	if out.ServiceVersion == "" {
		out.ServiceVersion = "v1"
	}
	if out.MediaType == "image" {
		if out.HDURL == "" {
			out.HDURL = out.URL
		}
		if out.HDURL != "" {
			out.URL = fmt.Sprintf("%s/static/apod/%s.jpg", baseURL(c.Request), out.Date)
		}
	}
	return out
}
