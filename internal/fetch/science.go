package fetch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"

	applog "apod-server/internal/log"
	"apod-server/internal/model"
)

const scienceEndpoint = "https://science.nasa.gov/wp-json/wp/v2/image-article"

type scienceArticle struct {
	Link  string `json:"link"`
	Title struct {
		Rendered string `json:"rendered"`
	} `json:"title"`
	Content struct {
		Rendered string `json:"rendered"`
	} `json:"content"`
}

// fetchFromWeb uses NASA Science's rendered article content. All searches share
// one deadline; a failed search never falls back to the retired APOD website.
func (s *Service) fetchFromWeb(ctx context.Context, date time.Time) (*model.APOD, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	l := applog.LoggerFromCtx(ctx)
	dateStr := date.Format("2006-01-02")
	queries := []string{date.Format("2006 January 02")}
	if unpadded := date.Format("2006 January 2"); unpadded != queries[0] {
		queries = append(queries, unpadded)
	}
	for _, query := range queries {
		articles, err := s.searchScience(ctx, query)
		if err != nil {
			apodFetchFailTotal.WithLabelValues("web").Inc()
			l.Warn("science request failed", zap.String("date", dateStr), zap.Error(err))
			return nil, err
		}
		for _, article := range articles {
			apod, err := parseScienceArticle(article, dateStr)
			if err != nil {
				apodParseFailTotal.WithLabelValues("web").Inc()
				l.Warn("science parse failed", zap.String("date", dateStr), zap.String("article", article.Link), zap.Error(err))
				continue
			}
			return apod, nil
		}
	}
	return nil, fmt.Errorf("science: no valid APOD for %s", dateStr)
}

func (s *Service) searchScience(ctx context.Context, dateTitle string) ([]scienceArticle, error) {
	query := url.Values{
		"search":   {`"APOD: ` + dateTitle + `"`},
		"per_page": {"100"},
		"_fields":  {"link,title.rendered,content.rendered"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scienceEndpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.UserAgent)
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("science HTTP status %d", resp.StatusCode)
	}
	// Rendered articles can be large, but a date search should remain bounded.
	const maxResponseBytes = 8 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("science response exceeds %d bytes", maxResponseBytes)
	}
	var articles []scienceArticle
	if err := json.Unmarshal(body, &articles); err != nil {
		return nil, fmt.Errorf("science invalid JSON: %w", err)
	}
	return articles, nil
}
