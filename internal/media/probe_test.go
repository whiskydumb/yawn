package media

import (
	"math"
	"testing"
)

// toleranceFor mirrors the rule inside Verify so it can be checked without
// running ffprobe.
func toleranceFor(expected float64) float64 {
	return math.Max(expected*DurationTolerance, DurationFloor)
}

func TestDurationToleranceAllowsRoundingOnShortVideos(t *testing.T) {
	cases := []struct {
		name     string
		expected float64
		actual   float64
		ok       bool
	}{
		{"seven seconds with rounding", 7, 7.4, true},
		{"seven seconds, half of it lost", 7, 3.5, false},
		{"a minute, a fraction over", 60, 60.8, true},
		{"an hour, two seconds over", 3600, 3602, true},
		{"an hour cut to a third", 3600, 1200, false},
		{"an hour missing five minutes", 3600, 3300, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			drift := math.Abs(c.actual - c.expected)
			ok := drift <= toleranceFor(c.expected)
			if ok != c.ok {
				t.Errorf("duration %.1f against %.1f: drift %.1f, allowed %.1f, got ok=%v want %v",
					c.actual, c.expected, drift, toleranceFor(c.expected), ok, c.ok)
			}
		})
	}
}

func TestDurationFloorDominatesOnShortVideos(t *testing.T) {
	// below forty seconds the relative tolerance is smaller than the floor,
	// and the floor has to win or short clips get failed for nothing
	if got := toleranceFor(10); got != DurationFloor {
		t.Errorf("tolerance for 10s = %.1f, want the floor %.1f", got, DurationFloor)
	}
	if got := toleranceFor(3600); got != 3600*DurationTolerance {
		t.Errorf("tolerance for an hour = %.1f, want the relative %.1f", got, 3600*DurationTolerance)
	}
}
