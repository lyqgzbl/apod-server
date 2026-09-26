package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	applog "apod-server/internal/log"
	"apod-server/internal/model"
	"go.uber.org/zap"
)

const nasaEndpoint = "https://science.nasa.gov/wp-json/wp/v2/apod-basic"

// --- NASA API response ---

type nasaAPIResponse struct {
	Date           string `json:"date"`
	Title          string `json:"title"`
	Copyright      string `json:"copyright"`
	Explanation    string `json:"explanation"`
	URL            string `json:"url"`
	HDURL          string `json:"hdurl"`
	Permalink      string `json:"permalink"`
	MediaType      string `json:"media_type"`
	ServiceVersion string `json:"service_version"`
}

// --- NASA API fetch ---

func (s *Service) fetchFromNASA(ctx context.Context, date string) (*model.APOD, error) {
	l := applog.LoggerFromCtx(ctx)
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := s.Limiter.Wait(fetchCtx); err != nil {
		apodFetchFailTotal.WithLabelValues("nasa_limiter").Inc()
		return nil, err
	}

	apiKey := s.NASAKey
	if apiKey == "" {
		apiKey = "DEMO_KEY"
	}
	query := neturl.Values{"api_key": {apiKey}, "date": {date}}
	url := nasaEndpoint + "?" + query.Encode()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(fetchCtx)
	req.Header.Set("User-Agent", s.UserAgent)

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		apodFetchFailTotal.WithLabelValues("nasa").Inc()
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		apodFetchFailTotal.WithLabelValues("nasa").Inc()
		return nil, fmt.Errorf("NASA API error: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 8<<20 {
		return nil, fmt.Errorf("NASA API response too large")
	}
	result, err := decodeNASAResponse(body, date)
	if err != nil {
		apodParseFailTotal.WithLabelValues("nasa").Inc()
		l.Warn("invalid nasa payload", zap.String("date", date), zap.Error(err))
		return nil, err
	}
	mediaType := strings.TrimSpace(result.MediaType)
	mediaURL := strings.TrimSpace(result.URL)
	// apod-basic's image URL is an article permalink; hdurl is the asset.
	if mediaType == "image" {
		mediaURL = strings.TrimSpace(result.HDURL)
	}
	parsedURL, err := neturl.Parse(mediaURL)
	if err != nil || parsedURL.Hostname() == "" || parsedURL.User != nil || (parsedURL.Scheme != "https" && parsedURL.Scheme != "http") ||
		mediaURL == strings.TrimSpace(result.Permalink) || (parsedURL.Hostname() == "science.nasa.gov" && strings.HasPrefix(parsedURL.Path, "/image-article/")) || (mediaType != "image" && mediaType != "video") {
		apodParseFailTotal.WithLabelValues("nasa").Inc()
		return nil, fmt.Errorf("NASA API returned no usable %s media URL", mediaType)
	}
	explanationDoc, err := goquery.NewDocumentFromReader(strings.NewReader(result.Explanation))
	if err != nil {
		return nil, err
	}
	apod := &model.APOD{
		Date:           strings.TrimSpace(result.Date),
		Title:          nasaPlainText(result.Title),
		Copyright:      nasaPlainText(result.Copyright),
		Explanation:    scienceExplanation(explanationDoc.Find("body")),
		ImageURL:       mediaURL,
		OriginImage:    mediaURL,
		MediaType:      mediaType,
		ServiceVersion: strings.TrimSpace(result.ServiceVersion),
	}
	if apod.ServiceVersion == "" {
		apod.ServiceVersion = "v1"
	}
	if apod.Date != date {
		apodParseFailTotal.WithLabelValues("nasa").Inc()
		l.Warn("nasa date mismatch", zap.String("requested_date", date), zap.String("actual_date", apod.Date))
		return nil, fmt.Errorf("NASA date mismatch: requested %s, got %s", date, apod.Date)
	}
	if apod.Title == "" || apod.Explanation == "" {
		apodParseFailTotal.WithLabelValues("nasa").Inc()
		l.Warn("invalid nasa payload", zap.String("date", date))
		return nil, fmt.Errorf("invalid NASA data")
	}
	return apod, nil
}

// NASA currently returns a collection even for some date queries. Never accept
// the first item without matching its date, or relabel it as the requested day.
func decodeNASAResponse(body []byte, date string) (nasaAPIResponse, error) {
	body = bytes.TrimSpace(body)
	var results []nasaAPIResponse
	if len(body) > 0 && body[0] == '[' {
		if err := json.Unmarshal(body, &results); err != nil {
			return nasaAPIResponse{}, err
		}
	} else {
		var result nasaAPIResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return nasaAPIResponse{}, err
		}
		results = append(results, result)
	}
	for _, result := range results {
		if strings.TrimSpace(result.Date) == date {
			return result, nil
		}
	}
	return nasaAPIResponse{}, fmt.Errorf("NASA date mismatch: no record for %s", date)
}

func nasaPlainText(value string) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(value))
	if err != nil {
		return ""
	}
	return normalizeText(doc.Find("body").Text())
}
