package model

// APOD represents internal APOD data used across the application.
type APOD struct {
	Date           string `json:"date"`
	Title          string `json:"title"`
	Copyright      string `json:"copyright"`
	ImageURL       string `json:"image_url"`
	OriginImage    string `json:"origin_image_url,omitempty"`
	Explanation    string `json:"explanation"`
	MediaType      string `json:"media_type"`
	ServiceVersion string `json:"service_version"`
	Cached         bool   `json:"cached"`
}

// APODResponse follows NASA APOD API response field naming for external output.
type APODResponse struct {
	Copyright      string `json:"copyright"`
	Date           string `json:"date"`
	Explanation    string `json:"explanation"`
	HDURL          string `json:"hdurl,omitempty"`
	MediaType      string `json:"media_type"`
	ServiceVersion string `json:"service_version"`
	Title          string `json:"title"`
	URL            string `json:"url"`
}
