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
		got := splitNonUniform(c.postCount, c.waves, l2WaveCap)
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
		bins := splitNonUniform(50, 5, l2WaveCap)
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

// TestSplitNonUniform_RespectsCap pins the per-wave ceiling: no bin may exceed
// the supplied cap regardless of postCount, and when postCount overflows
// waves*cap the bins saturate (sum below postCount is intentional — the L3
// overflow is fetched in a later cycle, not bursted now).
func TestSplitNonUniform_RespectsCap(t *testing.T) {
	const waves = 5
	for _, postCount := range []int{1, 5, 11, 25, 50, 76, 100, 500} {
		for trial := 0; trial < 50; trial++ {
			bins := splitNonUniform(postCount, waves, l3WaveCap)
			sum := 0
			for i, v := range bins {
				if v < 0 || v > l3WaveCap {
					t.Fatalf("splitNonUniform(%d,%d,cap=%d): bin %d = %d out of [0,%d] (bins=%v)",
						postCount, waves, l3WaveCap, i, v, l3WaveCap, bins)
				}
				sum += v
			}
			// Never fetch more than the cumulative cap this cycle.
			if max := waves * l3WaveCap; sum > max {
				t.Fatalf("splitNonUniform(%d,%d,cap=%d): sum %d exceeds waves*cap %d (bins=%v)",
					postCount, waves, l3WaveCap, sum, max, bins)
			}
			// Small workloads must still be fully covered.
			if postCount <= waves*l3WaveCap && postCount <= waves && sum != postCount {
				t.Fatalf("splitNonUniform(%d,%d,cap=%d): sum %d != postCount (bins=%v)",
					postCount, waves, l3WaveCap, sum, bins)
			}
		}
	}
}

// TestPlanL3Waves_ScalesWaveCount confirms L3's planner now sizes its wave count
// to the work — ceil(postCount/l3WaveTarget), clamped to [1,l3MaxWaves] — so the
// whole round's candidates are scheduled (no dropped overflow for realistic
// counts), every per-wave chunk stays within l3WaveCap, the average lands near
// l3WaveTarget, and all offsets fall inside the period (finish before next L1).
func TestPlanL3Waves_ScalesWaveCount(t *testing.T) {
	const period = 12 * time.Hour
	for postCount := 0; postCount <= 320; postCount += 7 {
		chunks, offsets := planL3Waves(postCount, period)
		if len(chunks) != len(offsets) {
			t.Fatalf("planL3Waves(%d): chunks/offsets length mismatch %d/%d", postCount, len(chunks), len(offsets))
		}
		if postCount == 0 {
			if len(chunks) != 0 {
				t.Fatalf("planL3Waves(0): want 0 waves, got %d", len(chunks))
			}
			continue
		}
		wantWaves := (postCount + l3WaveTarget - 1) / l3WaveTarget
		if wantWaves > l3MaxWaves {
			wantWaves = l3MaxWaves
		}
		if len(offsets) != wantWaves {
			t.Fatalf("planL3Waves(%d): %d waves, want %d", postCount, len(offsets), wantWaves)
		}
		sum := 0
		for i, c := range chunks {
			if c > l3WaveCap {
				t.Errorf("planL3Waves(%d): wave %d chunk %d exceeds cap %d (chunks=%v)",
					postCount, i, c, l3WaveCap, chunks)
			}
			sum += c
		}
		// No silent overflow drop within the unclamped range (waves*cap ≥ pc).
		if wantWaves < l3MaxWaves && sum != postCount {
			t.Errorf("planL3Waves(%d): chunks sum to %d, want full coverage %d (chunks=%v)",
				postCount, sum, postCount, chunks)
		}
		// Offsets sorted and strictly inside the period (the trailing tail keeps
		// the final wave clear of the next L1 cycle).
		for j := 1; j < len(offsets); j++ {
			if offsets[j] < offsets[j-1] {
				t.Fatalf("planL3Waves(%d): offsets not sorted: %v", postCount, offsets)
			}
		}
		if last := offsets[len(offsets)-1]; last >= period {
			t.Errorf("planL3Waves(%d): last offset %s overruns period %s", postCount, last, period)
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
