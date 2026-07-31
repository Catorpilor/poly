package polymarket

import (
	"testing"
	"time"
)

func TestParseGameStartTime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want time.Time
	}{
		{
			name: "gamma space format with +00 offset",
			in:   "2026-01-18 03:00:00+00",
			want: time.Date(2026, 1, 18, 3, 0, 0, 0, time.UTC),
		},
		{
			name: "rfc3339",
			in:   "2026-01-18T03:00:00Z",
			want: time.Date(2026, 1, 18, 3, 0, 0, 0, time.UTC),
		},
		{
			name: "empty is zero",
			in:   "",
			want: time.Time{},
		},
		{
			name: "garbage is zero",
			in:   "not-a-time",
			want: time.Time{},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ParseGameStartTime(tt.in)
			if !got.Equal(tt.want) {
				t.Errorf("ParseGameStartTime(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestGammaMarketGameStartAndTokenIDs(t *testing.T) {
	t.Parallel()
	m := &GammaMarket{
		GameStartTimeRaw: "2026-01-18 03:00:00+00",
		ClobTokenIdsRaw:  `["111","222"]`,
	}
	want := time.Date(2026, 1, 18, 3, 0, 0, 0, time.UTC)
	if got := m.GetGameStartTime(); !got.Equal(want) {
		t.Errorf("GetGameStartTime() = %v, want %v", got, want)
	}
	ids := m.GetClobTokenIds()
	if len(ids) != 2 || ids[0] != "111" || ids[1] != "222" {
		t.Errorf("GetClobTokenIds() = %v, want [111 222]", ids)
	}

	empty := &GammaMarket{}
	if got := empty.GetGameStartTime(); !got.IsZero() {
		t.Errorf("empty GetGameStartTime() = %v, want zero", got)
	}
	if got := empty.GetClobTokenIds(); got != nil {
		t.Errorf("empty GetClobTokenIds() = %v, want nil", got)
	}
}
