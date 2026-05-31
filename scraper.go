package lilith

import "context"

// ScrapeResult is the extracted content of a scraped web page.
type ScrapeResult struct {
	// URL is the final URL after following all redirects.
	URL string
	// StatusCode is the HTTP status code of the final response.
	StatusCode int
	// Title is the page title (<title> or og:title).
	Title string
	// Description is the page description (meta description or og:description).
	Description string
	// Text is the extracted, whitespace-normalized readable text of the page.
	Text string
}

// Scraper fetches and extracts readable content from web pages. Implementations
// follow redirects and present a browser-like identity to the remote server.
type Scraper interface {
	// Scrape fetches the page at url, following redirects, and returns its
	// extracted content.
	Scrape(ctx context.Context, url string) (*ScrapeResult, error)
}
