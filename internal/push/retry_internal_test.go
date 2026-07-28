package push

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"absent", "", 0},
		{"delay seconds", "5", 5 * time.Second},
		{"zero seconds", "0", 0},
		{"negative seconds", "-3", 0},
		{"http date in the future", now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{"http date in the past", now.Add(-90 * time.Second).Format(http.TimeFormat), 0},
		{"garbage", "soon", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseRetryAfter(tc.header, now))
		})
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	assert.Equal(t, retryDelay, backoff(0))
	assert.Equal(t, 2*retryDelay, backoff(1))
	assert.Equal(t, 4*retryDelay, backoff(2))
	assert.Equal(t, maxBackoff, backoff(30), "backoff must cap, and must not overflow into a negative wait")
}
