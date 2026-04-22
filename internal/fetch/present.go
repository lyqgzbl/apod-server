package fetch

import (
	"fmt"
	"net/http"

	"apod-server/internal/httputil"
	"apod-server/internal/model"
)

// PresentAPOD converts internal APOD to the external API response format.
func PresentAPOD(r *http.Request, apod *model.APOD) *model.APODResponse {
	out := &model.APODResponse{
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
			out.URL = fmt.Sprintf("%s/static/apod/%s.jpg", httputil.BaseURL(r), out.Date)
		}
	}
	return out
}
