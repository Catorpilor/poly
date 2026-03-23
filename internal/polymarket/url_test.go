package polymarket

import "testing"

func TestParseEventSlug(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		wantSlug string
		wantOK   bool
	}{
		{
			name:     "sports URL with sport category",
			input:    "https://polymarket.com/sports/nba/nba-min-bos-2026-03-22",
			wantSlug: "nba-min-bos-2026-03-22",
			wantOK:   true,
		},
		{
			name:     "event URL",
			input:    "https://polymarket.com/event/lighter-market-cap-fdv-one-day-after-launch",
			wantSlug: "lighter-market-cap-fdv-one-day-after-launch",
			wantOK:   true,
		},
		{
			name:     "sports URL with subcategory",
			input:    "https://polymarket.com/sports/soccer/epl/epl-match-slug",
			wantSlug: "epl-match-slug",
			wantOK:   true,
		},
		{
			name:     "www prefix",
			input:    "https://www.polymarket.com/event/some-slug",
			wantSlug: "some-slug",
			wantOK:   true,
		},
		{
			name:     "query params stripped",
			input:    "https://polymarket.com/event/some-slug?tab=markets&ref=abc",
			wantSlug: "some-slug",
			wantOK:   true,
		},
		{
			name:     "trailing slash",
			input:    "https://polymarket.com/event/some-slug/",
			wantSlug: "some-slug",
			wantOK:   true,
		},
		{
			name:   "homepage rejected",
			input:  "https://polymarket.com/",
			wantOK: false,
		},
		{
			name:   "sports root rejected",
			input:  "https://polymarket.com/sports",
			wantOK: false,
		},
		{
			name:   "wrong host",
			input:  "https://google.com/event/foo",
			wantOK: false,
		},
		{
			name:   "not a URL",
			input:  "not-a-url",
			wantOK: false,
		},
		{
			name:   "empty string",
			input:  "",
			wantOK: false,
		},
		{
			name:   "bare slug without URL",
			input:  "nba-min-bos-2026-03-22",
			wantOK: false,
		},
		{
			name:     "http scheme also works",
			input:    "http://polymarket.com/event/some-slug",
			wantSlug: "some-slug",
			wantOK:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			slug, ok := ParseEventSlug(tt.input)
			if ok != tt.wantOK {
				t.Errorf("ParseEventSlug(%q) ok = %v, want %v", tt.input, ok, tt.wantOK)
			}
			if slug != tt.wantSlug {
				t.Errorf("ParseEventSlug(%q) slug = %q, want %q", tt.input, slug, tt.wantSlug)
			}
		})
	}
}
