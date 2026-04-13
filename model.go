package main

type APOD struct {
	Date        string `json:"date"`
	Title       string `json:"title"`
	ImageURL    string `json:"image_url"`
	OriginImage string `json:"origin_image_url,omitempty"`
	Explanation string `json:"explanation"`
	MediaType   string `json:"media_type"`
	Cached      bool   `json:"cached"`
}
