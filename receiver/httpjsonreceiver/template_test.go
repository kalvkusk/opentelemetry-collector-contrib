package httpjsonreceiver

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderTimeTemplates(t *testing.T) {
	now := time.Date(2026, 9, 7, 18, 30, 0, 0, time.UTC)

	for _, tc := range []struct{ in, want string }{
		{"{{now}}", "2026-09-07T18:30:00Z"},
		{"{{ now }}", "2026-09-07T18:30:00Z"},
		{"{{now-5m}}", "2026-09-07T18:25:00Z"},
		{"{{now - 5m}}", "2026-09-07T18:25:00Z"},
		{"{{now+1h}}", "2026-09-07T19:30:00Z"},
		{"{{now-1h30m}}", "2026-09-07T17:00:00Z"},
		{"{{now|unix}}", "1788805800"},
		{"{{now-5m | unix}}", "1788805500"},
		{"{{now|date}}", "2026-09-07"},
		{"{{now|minute}}", "2026-09-07T18:30:00Z"},
		{"no templates here", "no templates here"},
		{`{"min":"{{now-6m}}","max":"{{now-5m}}"}`, `{"min":"2026-09-07T18:24:00Z","max":"2026-09-07T18:25:00Z"}`},
	} {
		got, err := renderTimeTemplates(tc.in, now)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
}

func TestRenderTimeTemplatesErrors(t *testing.T) {
	now := time.Now()
	// An unknown format must fail loudly rather than send the API a literal
	// "{{now|nonsense}}" and leave someone decoding the rejection.
	_, err := renderTimeTemplates("{{now|nonsense}}", now)
	require.ErrorContains(t, err, "unknown time format")
}

// TestMinuteFormatAlignsWindows is the property the Cloudflare queries rely on:
// two scrapes a minute apart must produce exactly adjacent, non-overlapping
// windows, even when the scrape itself lands at an arbitrary point in the minute.
func TestMinuteFormatAlignsWindows(t *testing.T) {
	first := time.Date(2026, 9, 7, 18, 30, 17, 400_000_000, time.UTC)
	second := first.Add(time.Minute).Add(230 * time.Millisecond)

	tmpl := `{"min":"{{now-6m|minute}}","max":"{{now-5m|minute}}"}`

	a, err := renderTimeTemplates(tmpl, first)
	require.NoError(t, err)
	b, err := renderTimeTemplates(tmpl, second)
	require.NoError(t, err)

	assert.Equal(t, `{"min":"2026-09-07T18:24:00Z","max":"2026-09-07T18:25:00Z"}`, a)
	assert.Equal(t, `{"min":"2026-09-07T18:25:00Z","max":"2026-09-07T18:26:00Z"}`, b)
	// The first window's max is the second window's min: adjacent, no overlap, no gap.
}
