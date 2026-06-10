package prefetch

import (
	"testing"
	"time"
)

func TestSplitNonUniform_SumAndFloor(t *testing.T) {
	cases := []struct {
		postCount int
		waves     int
	}{
		{0, 5},
		{1, 5},
		{4, 5},
		{5, 5},
		{6, 5},
		{25, 5},
		{76, 5},
		{100, 5},
	}
	for _, c := range cases {
		got := splitNonUniform(c.postCount, c.waves)
		if len(got) != c.waves {
			t.Errorf("splitNonUniform(%d,%d): len=%d want %d", c.postCount, c.waves, len(got), c.waves)
			continue
		}
		sum := 0
		for _, v := range got {
			if v < 0 {
				t.Errorf("splitNonUniform(%d,%d): negative bin %v", c.postCount, c.waves, got)
			}
			sum += v
		}
		if sum != c.postCount {
			t.Errorf("splitNonUniform(%d,%d): sum=%d want %d (bins=%v)", c.postCount, c.waves, sum, c.postCount, got)
		}
		if c.postCount >= c.waves {
			for i, v := range got {
				if v < 1 {
					t.Errorf("splitNonUniform(%d,%d): bin %d is 0 but postCount>=waves (bins=%v)", c.postCount, c.waves, i, got)
				}
			}
		}
	}
}

func TestSplitNonUniform_DistributionIsNonUniform(t *testing.T) {
	const trials = 200
	uniformRuns := 0
	for i := 0; i < trials; i++ {
		bins := splitNonUniform(50, 5)
		first := bins[0]
		same := true
		for _, v := range bins[1:] {
			if v != first {
				same = false
				break
			}
		}
		if same {
			uniformRuns++
		}
	}
	// With genuinely random non-uniform partitioning over 200 trials of 50/5
	// posts, almost every run should have some variance. Allow ≤5% of runs
	// to be coincidentally uniform.
	if uniformRuns > trials/20 {
		t.Errorf("expected non-uniform distribution; %d/%d trials produced all-equal bins", uniformRuns, trials)
	}
}

func TestPlanWaves_MinGapAtLeastTenPercent(t *testing.T) {
	const trials = 500
	periods := []time.Duration{
		time.Hour,
		6 * time.Hour,
		12 * time.Hour,
		48 * time.Hour,
	}
	for _, period := range periods {
		minGap := time.Duration(float64(period) * waveMinGapFrac)
		// computeCyclePeriod jitters but planWaves takes the resolved period
		// directly, so the floor is exact.
		for i := 0; i < trials; i++ {
			_, offsets := planWaves(50, period)
			if len(offsets) != l2WavesPerCycle {
				t.Fatalf("planWaves returned %d offsets, want %d", len(offsets), l2WavesPerCycle)
			}
			for j := 1; j < len(offsets); j++ {
				gap := offsets[j] - offsets[j-1]
				// Allow a 1ns rounding tolerance for the int64 conversion.
				if gap+time.Nanosecond < minGap {
					t.Errorf("period=%s trial=%d: gap %s between wave %d→%d below floor %s (offsets=%v)",
						period, i, gap, j-1, j, minGap, offsets)
					break
				}
			}
			if offsets[len(offsets)-1] > period {
				t.Errorf("period=%s trial=%d: last offset %s exceeds period (offsets=%v)",
					period, i, offsets[len(offsets)-1], offsets)
			}
			if offsets[0] < 0 {
				t.Errorf("period=%s trial=%d: first offset %s negative", period, i, offsets[0])
			}
		}
	}
}

func TestPlanWaves_OffsetsSorted(t *testing.T) {
	for i := 0; i < 50; i++ {
		_, offsets := planWaves(40, 12*time.Hour)
		for j := 1; j < len(offsets); j++ {
			if offsets[j] < offsets[j-1] {
				t.Fatalf("offsets not sorted: %v", offsets)
			}
		}
	}
}

func TestPlanWaves_GapsAreNonUniform(t *testing.T) {
	const trials = 200
	const period = 12 * time.Hour
	uniformRuns := 0
	for i := 0; i < trials; i++ {
		_, offsets := planWaves(50, period)
		gaps := make([]time.Duration, 0, len(offsets))
		var prev time.Duration
		for _, o := range offsets {
			gaps = append(gaps, o-prev)
			prev = o
		}
		// Compute range of gaps; if min==max the gaps are uniform.
		minG, maxG := gaps[0], gaps[0]
		for _, g := range gaps[1:] {
			if g < minG {
				minG = g
			}
			if g > maxG {
				maxG = g
			}
		}
		// Tolerance: gaps within 1s of each other count as uniform.
		if maxG-minG < time.Second {
			uniformRuns++
		}
	}
	if uniformRuns > trials/20 {
		t.Errorf("expected non-uniform gaps; %d/%d trials had near-uniform gaps", uniformRuns, trials)
	}
}

func TestPlanWaves_ChunksSumToPostCount(t *testing.T) {
	for postCount := 0; postCount <= 100; postCount += 13 {
		chunks, _ := planWaves(postCount, 12*time.Hour)
		sum := 0
		for _, c := range chunks {
			sum += c
		}
		if sum != postCount {
			t.Errorf("planWaves(postCount=%d): chunks sum to %d, want %d (chunks=%v)", postCount, sum, postCount, chunks)
		}
	}
}

func TestPlanWaves_ZeroPeriod(t *testing.T) {
	chunks, offsets := planWaves(10, 0)
	if len(chunks) != l2WavesPerCycle || len(offsets) != l2WavesPerCycle {
		t.Fatalf("zero period: lengths %d/%d, want %d", len(chunks), len(offsets), l2WavesPerCycle)
	}
	for _, o := range offsets {
		if o != 0 {
			t.Errorf("zero period: expected all-zero offsets, got %v", offsets)
			break
		}
	}
}
