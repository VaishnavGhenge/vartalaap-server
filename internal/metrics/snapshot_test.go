package metrics

import "testing"

// The connection-success SLO. These pin the two judgement calls in it: a slow
// call is a connected call, and an abandoned one is not a call at all.
func TestSuccessRatePct(t *testing.T) {
	cases := []struct {
		name     string
		attempts CallAttemptsSummary
		want     float64
	}{
		{
			name: "no attempts reports zero rather than dividing by zero",
			want: 0,
		},
		{
			name:     "a slow call counts as connected",
			attempts: CallAttemptsSummary{Success: 8, Slow: 2},
			want:     100,
		},
		{
			name:     "abandoned is excluded from both halves",
			attempts: CallAttemptsSummary{Success: 10, Abandoned: 90},
			want:     100,
		},
		{
			name:     "failed counts against the rate",
			attempts: CallAttemptsSummary{Success: 9, Failed: 1},
			want:     90,
		},
		{
			name:     "error counts against the rate",
			attempts: CallAttemptsSummary{Success: 3, Error: 1},
			want:     75,
		},
		{
			name:     "legacy timeouts still count as failures",
			attempts: CallAttemptsSummary{Success: 1, Timeout: 1},
			want:     50,
		},
		{
			name:     "only abandoned means there is nothing to rate",
			attempts: CallAttemptsSummary{Abandoned: 5},
			want:     0,
		},
		{
			name:     "a room of slow calls is still fully connected",
			attempts: CallAttemptsSummary{Slow: 4, Failed: 1},
			want:     80,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := successRatePct(tc.attempts); got != tc.want {
				t.Fatalf("successRatePct = %.2f, want %.2f", got, tc.want)
			}
		})
	}
}
