// Package messaging_test pins the pure scheduling rules of the workers.
package messaging_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/chalkan3/backend-challenge-go/internal/messaging"
)

func TestExpBackoff(t *testing.T) {
	hour := time.Hour
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: 0, want: 2 * time.Second}, // clamped to the first real attempt
		{attempts: 1, want: 2 * time.Second},
		{attempts: 2, want: 4 * time.Second},
		{attempts: 3, want: 8 * time.Second},
		{attempts: 10, want: 1 << 10 * time.Second},
		{attempts: 21, want: hour}, // shift saturates at 20 and hits the cap
		{attempts: 999, want: hour},
	}
	for _, tc := range tests {
		got := messaging.ExpBackoff(tc.attempts, hour)
		assert.Equal(t, tc.want, got, "attempts=%d", tc.attempts)
	}
}

func TestExpBackoffRespectsCustomCap(t *testing.T) {
	cap30s := 30 * time.Second
	assert.Equal(t, 4*time.Second, messaging.ExpBackoff(2, cap30s))
	assert.Equal(t, cap30s, messaging.ExpBackoff(10, cap30s), "grows are capped by the limit")
}
