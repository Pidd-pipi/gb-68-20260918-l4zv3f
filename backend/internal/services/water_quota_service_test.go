package services

import "testing"

func TestEstimateWaterAmount(t *testing.T) {
	cases := []struct {
		duration int
		want     float64
	}{
		{duration: 0, want: 0},
		{duration: -5, want: 0},
		{duration: 60, want: 6.0},
		{duration: 300, want: 30.0},
	}

	for _, c := range cases {
		if got := EstimateWaterAmount(c.duration); got != c.want {
			t.Errorf("EstimateWaterAmount(%d) = %v, want %v", c.duration, got, c.want)
		}
	}
}
