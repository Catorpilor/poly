package polymarket

import (
	"net/url"
	"strings"
)

// ParseEventSlug extracts the event slug from a Polymarket URL.
// Supported URL patterns:
//   - https://polymarket.com/event/<slug>
//   - https://polymarket.com/sports/<sport>/<slug>
//   - https://polymarket.com/sports/<sport>/<subcategory>/<slug>
//
// The slug is always the last non-empty path segment.
// Returns the slug and true if parsed, or ("", false) if not a valid Polymarket event URL.
func ParseEventSlug(rawURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}

	host := strings.TrimPrefix(u.Host, "www.")
	if host != "polymarket.com" {
		return "", false
	}

	// Split path into non-empty segments
	segments := make([]string, 0)
	for _, s := range strings.Split(u.Path, "/") {
		if s != "" {
			segments = append(segments, s)
		}
	}

	// Need at least 2 segments: category + slug (e.g., "event/some-slug" or "sports/nba/slug")
	if len(segments) < 2 {
		return "", false
	}

	return segments[len(segments)-1], true
}
