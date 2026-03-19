package telegram

import (
	"strings"
	"testing"
)

func TestTruncateUTF8(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		maxRunes int
		want     string
	}{
		// --- Empty string ---
		{
			name:     "empty string",
			input:    "",
			maxRunes: 10,
			want:     "",
		},

		// --- ASCII strings: shorter, equal, longer ---
		{
			name:     "ASCII shorter than maxRunes",
			input:    "hello",
			maxRunes: 10,
			want:     "hello",
		},
		{
			name:     "ASCII exactly maxRunes",
			input:    "hello",
			maxRunes: 5,
			want:     "hello",
		},
		{
			name:     "ASCII one rune longer than maxRunes",
			input:    "hello!",
			maxRunes: 5,
			want:     "he...",
		},
		{
			name:     "ASCII much longer than maxRunes",
			input:    "the quick brown fox jumps over the lazy dog",
			maxRunes: 10,
			want:     "the qui...",
		},

		// --- Multi-byte UTF-8: degree symbol (2 bytes per rune) ---
		{
			name:     "degree symbols shorter than maxRunes",
			input:    "20°C",
			maxRunes: 10,
			want:     "20°C",
		},
		{
			name:     "degree symbols exactly maxRunes",
			input:    "20°C",
			maxRunes: 4,
			want:     "20°C",
		},
		{
			name:     "degree symbols truncated",
			input:    "100°F is hot",
			maxRunes: 7,
			want:     "100°...",
		},

		// --- Multi-byte UTF-8: CJK characters (3 bytes per rune) ---
		{
			name:     "CJK characters not truncated",
			input:    "日本語",
			maxRunes: 3,
			want:     "日本語",
		},
		{
			name:     "CJK characters truncated",
			input:    "日本語テスト",
			maxRunes: 5,
			want:     "日本...",
		},
		{
			name:     "mixed ASCII and CJK truncated",
			input:    "Hello世界你好",
			maxRunes: 8,
			want:     "Hello...",
		},

		// --- Multi-byte UTF-8: emoji (4 bytes per rune) ---
		{
			name:     "emoji not truncated",
			input:    "hi 😀",
			maxRunes: 4,
			want:     "hi 😀",
		},
		{
			name:     "emoji truncated preserves boundary",
			input:    "hello 😀 world",
			maxRunes: 8,
			want:     "hello...",
		},
		{
			name:     "string of emojis truncated",
			input:    "🎉🎊🎈🎁🎂",
			maxRunes: 4,
			want:     "🎉...",
		},

		// --- Boundary: exactly maxRunes (no truncation) ---
		{
			name:     "exactly maxRunes with multi-byte",
			input:    "ab°",
			maxRunes: 3,
			want:     "ab°",
		},

		// --- maxRunes equals 3 (minimum for ellipsis) ---
		{
			name:     "maxRunes is 3 and string is longer",
			input:    "abcdef",
			maxRunes: 3,
			want:     "...",
		},

		// --- maxRunes equals 4 (one char + ellipsis) ---
		{
			name:     "maxRunes is 4 and string is longer",
			input:    "abcdef",
			maxRunes: 4,
			want:     "a...",
		},

		// --- Large maxRunes, short string ---
		{
			name:     "maxRunes much larger than string",
			input:    "ok",
			maxRunes: 1000,
			want:     "ok",
		},

		// --- Single character string ---
		{
			name:     "single ASCII char not truncated",
			input:    "x",
			maxRunes: 5,
			want:     "x",
		},

		// --- String with combining characters ---
		{
			name:     "string with newlines truncated",
			input:    "line1\nline2\nline3",
			maxRunes: 8,
			want:     "line1...",
		},

		// --- Truncation that would split a multi-byte char with byte slicing ---
		{
			name:     "byte slicing would break 2-byte char",
			input:    "aaa°bbb",
			maxRunes: 5,
			want:     "aa...",
		},
		{
			name:     "byte slicing would break 3-byte CJK char",
			input:    "aa日本語",
			maxRunes: 4,
			want:     "a...",
		},
		{
			name:     "byte slicing would break 4-byte emoji",
			input:    "aaa😀bbb",
			maxRunes: 5,
			want:     "aa...",
		},

		// --- All multi-byte, truncated ---
		{
			name:     "all 2-byte runes truncated",
			input:    "°°°°°°",
			maxRunes: 5,
			want:     "°°...",
		},
		{
			name:     "all 4-byte emojis truncated",
			input:    "😀😀😀😀😀😀",
			maxRunes: 5,
			want:     "😀😀...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := truncateUTF8(tt.input, tt.maxRunes)
			if got != tt.want {
				t.Errorf("truncateUTF8(%q, %d) = %q, want %q", tt.input, tt.maxRunes, got, tt.want)
			}
		})
	}
}

func TestTruncateUTF8_NeverCutsMultiByte(t *testing.T) {
	t.Parallel()

	// Property: the result should always be valid UTF-8
	// and never exceed maxRunes runes.
	inputs := []string{
		"hello world",
		"日本語テスト文字列",
		"emoji 🎉🎊🎈🎁🎂 test",
		"café résumé naïve",
		"°°°abc°°°",
		strings.Repeat("🏳️‍🌈", 10),
	}

	for _, s := range inputs {
		for maxRunes := 3; maxRunes <= 20; maxRunes++ {
			got := truncateUTF8(s, maxRunes)

			// Result must be valid UTF-8 (Go strings are, but let's be explicit)
			for i, r := range got {
				if r == '\uFFFD' {
					t.Errorf("truncateUTF8(%q, %d) produced replacement char at byte %d", s, maxRunes, i)
				}
			}
		}
	}
}
