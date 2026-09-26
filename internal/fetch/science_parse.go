package fetch

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	xhtml "golang.org/x/net/html"

	"apod-server/internal/model"
)

var scienceTitleDate = regexp.MustCompile(`^APOD:\s*(\d{4} [A-Za-z]+ \d{1,2})\s*[-–—]\s*\S`)

func parseScienceArticle(article scienceArticle, requestedDate string) (*model.APOD, error) {
	title := normalizeText(html.UnescapeString(article.Title.Rendered))
	match := scienceTitleDate.FindStringSubmatch(title)
	if len(match) != 2 {
		return nil, fmt.Errorf("science: missing APOD title date")
	}
	date, err := time.Parse("2006 January 2", match[1])
	if err != nil || date.Format("2006-01-02") != requestedDate {
		return nil, fmt.Errorf("science title date mismatch: requested %s, got %s", requestedDate, match[1])
	}
	base, err := url.Parse(article.Link)
	if err != nil || base.Scheme != "https" || base.Host != "science.nasa.gov" || !strings.HasPrefix(base.Path, "/image-article/") {
		return nil, fmt.Errorf("science: invalid article link")
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(article.Content.Rendered))
	if err != nil {
		return nil, err
	}
	hero := doc.Find(".hds-media-detail-hero").First()
	var actualDate, copyright string
	hero.Find(".media-detail-hero__meta-table tr").Each(func(_ int, row *goquery.Selection) {
		label := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(normalizeText(row.Find("th").Text()), ":")))
		value := normalizeText(row.Find("td").Text())
		if label == "date" {
			actualDate = value
		}
		if strings.Contains(label, "copyright") {
			copyright = value
		}
	})
	date, err = time.Parse("January 2, 2006", actualDate)
	if err != nil || date.Format("2006-01-02") != requestedDate {
		return nil, fmt.Errorf("science body date mismatch: requested %s, got %q", requestedDate, actualDate)
	}
	mediaURL, mediaType := extractScienceMedia(hero.Find(".media-detail-hero__media").First(), article.Link)
	apod := &model.APOD{
		Date:           requestedDate,
		Title:          normalizeText(hero.Find("h1, h2").First().Text()),
		Explanation:    scienceExplanation(hero.Find(".media-detail-hero__description")),
		Copyright:      copyright,
		ImageURL:       mediaURL,
		OriginImage:    mediaURL,
		MediaType:      mediaType,
		ServiceVersion: "v1",
	}
	if apod.Title == "" || apod.Explanation == "" || mediaURL == "" {
		return nil, fmt.Errorf("science: missing title, explanation or supported media")
	}
	return apod, nil
}

func extractScienceMedia(media *goquery.Selection, pageURL string) (string, string) {
	// A video may also contain a poster image. Prefer its actual playback URL.
	for _, selector := range []string{"video[src]", "video source[src]", "iframe[src]"} {
		var result string
		media.Find(selector).EachWithBreak(func(_ int, node *goquery.Selection) bool {
			if node.Is("[hidden], [aria-hidden='true']") || node.ParentsFiltered("noscript, [hidden], [aria-hidden='true']").Length() > 0 {
				return true
			}
			raw, _ := node.Attr("src")
			resolved := resolveMediaURL(pageURL, raw)
			if resolved == "" {
				return true
			}
			u, _ := url.Parse(resolved)
			if u.Hostname() == "www.googletagmanager.com" || u.Hostname() == "www.google-analytics.com" {
				return true
			}
			result = resolved
			return false
		})
		if result != "" {
			return result, "video"
		}
	}
	img := media.Find(".smd-image-rollover__image--primary").First()
	if img.Length() == 0 {
		img = media.Find("img").Not(".smd-image-rollover__image--secondary").First()
	}
	if raw, ok := img.Attr("src"); ok {
		if resolved := resolveMediaURL(pageURL, raw); resolved != "" {
			return resolved, "image"
		}
	}
	return "", ""
}

func resolveMediaURL(pageURL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return ""
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	resolved := base.ResolveReference(ref)
	if (resolved.Scheme != "https" && resolved.Scheme != "http") || resolved.Hostname() == "" || resolved.User != nil {
		return ""
	}
	return resolved.String()
}

func scienceExplanation(selection *goquery.Selection) string {
	var text strings.Builder
	var walk func(*xhtml.Node)
	walk = func(node *xhtml.Node) {
		if node.Type == xhtml.ElementNode {
			switch node.Data {
			case "script", "style", "noscript":
				return
			case "br", "p", "div":
				text.WriteByte(' ')
			}
		}
		if node.Type == xhtml.TextNode {
			text.WriteString(node.Data)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if node.Type == xhtml.ElementNode && (node.Data == "p" || node.Data == "div") {
			text.WriteByte(' ')
		}
	}
	for _, node := range selection.Nodes {
		walk(node)
	}
	result := normalizeText(text.String())
	if strings.HasPrefix(strings.ToLower(result), "explanation:") {
		result = strings.TrimSpace(result[len("explanation:"):])
	}
	for _, marker := range []string{"APOD's email for image submissions", "APOD's main NASA site", "Tomorrow's picture:", "Tomorrow’s picture:"} {
		if i := strings.Index(strings.ToLower(result), strings.ToLower(marker)); i >= 0 {
			result = strings.TrimSpace(result[:i])
		}
	}
	return result
}

func normalizeText(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
