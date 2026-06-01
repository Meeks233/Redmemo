package media

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/redmemo/redmemo/internal/store"
)

// newDiskEvictor builds a bare Evictor with only rootPath set — enough to
// exercise DiskUsage without a database.
func newDiskEvictor(root string) *Evictor {
	return &Evictor{rootPath: root}
}

func TestDiskUsage_EmptyDir(t *testing.T) {
	e := newDiskEvictor(t.TempDir())
	total, err := e.DiskUsage()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 0 {
		t.Errorf("DiskUsage = %d, want 0 for an empty dir", total)
	}
}

func TestDiskUsage_FlatFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.bin"), 100)
	writeFile(t, filepath.Join(dir, "b.bin"), 250)
	writeFile(t, filepath.Join(dir, "c.bin"), 1)

	e := newDiskEvictor(dir)
	total, err := e.DiskUsage()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 351 {
		t.Errorf("DiskUsage = %d, want 351", total)
	}
}

func TestDiskUsage_NestedDirs(t *testing.T) {
	dir := t.TempDir()
	// Content-addressed layout uses sharded sub-directories.
	sub1 := filepath.Join(dir, "ab", "cd")
	sub2 := filepath.Join(dir, "ef", "gh")
	if err := os.MkdirAll(sub1, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub2, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "top.bin"), 10)
	writeFile(t, filepath.Join(sub1, "x.bin"), 2000)
	writeFile(t, filepath.Join(sub2, "y.bin"), 500)

	e := newDiskEvictor(dir)
	total, err := e.DiskUsage()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 2510 {
		t.Errorf("DiskUsage = %d, want 2510 (recursive sum)", total)
	}
}

func TestDiskUsage_MissingRoot(t *testing.T) {
	// A not-yet-created media root must not crash — Walk's error is swallowed
	// per-entry and the total is simply 0.
	e := newDiskEvictor(filepath.Join(t.TempDir(), "does-not-exist"))
	total, err := e.DiskUsage()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 0 {
		t.Errorf("DiskUsage = %d, want 0 for a missing root", total)
	}
}

// makeCandidate builds a *store.MediaMeta sized at sizeBytes and ranked at
// the given score (higher = evict sooner — same orientation as the SQL view).
// Hash and FilePath are synthesised from i so each row is identifiable.
func makeCandidate(i int, sizeBytes int64, score float64) *store.MediaMeta {
	fp := fmt.Sprintf("/tmp/media/%02x/%02x/%d", i&0xff, (i>>8)&0xff, i)
	return &store.MediaMeta{
		Hash:     fmt.Sprintf("hash-%06d", i),
		FilePath: &fp,
		FileSize: sizeBytes,
		Score:    score,
	}
}

// sortedDesc builds a candidate slice ordered by score DESC so it mirrors what
// the DB-side selector would emit before in-memory accumulation.
func sortedDesc(rows ...*store.MediaMeta) []*store.MediaMeta { return rows }

const (
	kib = 1024
	mib = 1024 * 1024
	gib = 1024 * 1024 * 1024
)

func sumSize(rows []*store.MediaMeta) int64 {
	var n int64
	for _, r := range rows {
		n += r.FileSize
	}
	return n
}

// TestSimulateEviction_BelowCap — usage under the cap is a no-op. The selector
// must not touch the candidate list.
func TestSimulateEviction_BelowCap(t *testing.T) {
	cands := sortedDesc(
		makeCandidate(1, 5*mib, 90),
		makeCandidate(2, 5*mib, 80),
	)
	sel, hashes, freed := simulateEviction(cands, 100*mib, 1*gib)
	if len(sel) != 0 || len(hashes) != 0 || freed != 0 {
		t.Fatalf("below-cap should be a no-op; got %d rows, %d hashes, %d freed", len(sel), len(hashes), freed)
	}
}

// TestSimulateEviction_SmallFiles — a fleet of tiny files (~64 KiB each) must
// accumulate many rows before crossing the 10% target of a 1 GiB cap (102.4
// MiB). The selector must keep walking until the cumulative size crosses the
// line, and the first row dropped must be the highest-score one.
func TestSimulateEviction_SmallFiles(t *testing.T) {
	var cap1G int64 = 1 * gib
	target := int64(float64(cap1G) * 0.10) // 107374182 bytes
	cands := make([]*store.MediaMeta, 0, 2000)
	// Scores 99.99, 99.98, ... descending so order is unambiguous.
	for i := 0; i < 2000; i++ {
		cands = append(cands, makeCandidate(i, 64*kib, 100.0-float64(i)*0.01))
	}
	sel, hashes, freed := simulateEviction(cands, cap1G, cap1G)
	if freed < target {
		t.Fatalf("freed %d < target %d", freed, target)
	}
	// Must have stopped at the first row that crosses the line — not one extra.
	if freed-int64(64*kib) >= target {
		t.Fatalf("over-selected: freed=%d, target=%d, last row=%d KiB", freed, target, 64)
	}
	if len(sel) != len(hashes) {
		t.Fatalf("hash count %d != selection count %d", len(hashes), len(sel))
	}
	// Highest-score row first.
	if sel[0].Hash != "hash-000000" {
		t.Fatalf("first selected = %s, want highest-score row hash-000000", sel[0].Hash)
	}
	// Approx 102.4 MiB / 64 KiB = 1638 rows expected.
	if len(sel) < 1600 || len(sel) > 1700 {
		t.Fatalf("small-file selection size %d outside expected 1600..1700", len(sel))
	}
}

// TestSimulateEviction_LargeFiles — a single ~120 MiB file already covers the
// 10% target of a 1 GiB cap on its own. The selector must stop right after
// the first row even though more candidates remain.
func TestSimulateEviction_LargeFiles(t *testing.T) {
	const cap1G = 1 * gib
	cands := sortedDesc(
		makeCandidate(1, 120*mib, 95),
		makeCandidate(2, 200*mib, 90),
		makeCandidate(3, 80*mib, 85),
	)
	sel, hashes, freed := simulateEviction(cands, cap1G+1, cap1G)
	if len(sel) != 1 {
		t.Fatalf("large-file pass should pick exactly 1 row, got %d", len(sel))
	}
	if sel[0].Hash != "hash-000001" || freed != 120*mib {
		t.Fatalf("wrong row selected: hash=%s freed=%d", sel[0].Hash, freed)
	}
	if hashes[0] != "hash-000001" {
		t.Fatalf("batch hash list mismatch: %v", hashes)
	}
}

// TestSimulateEviction_MixedFiles — large + medium + small interleaved.
// Cap = 1 GiB → target ≈ 102.4 MiB. With the highest-score rows being:
//
//	#0  90 MiB (score 99)   ← cumulative 90 MiB, still short of target
//	#1   8 MiB (score 98)   ← cumulative 98 MiB, still short
//	#2   5 MiB (score 97)   ← cumulative 103 MiB, crosses the line → stop
//	#3 200 MiB (score 50)   ← never reached, lower score
//
// The selector must include #0..#2 and exclude the giant #3 because its
// score is lower despite its size.
func TestSimulateEviction_MixedFiles(t *testing.T) {
	const cap1G = 1 * gib
	cands := sortedDesc(
		makeCandidate(0, 90*mib, 99),
		makeCandidate(1, 8*mib, 98),
		makeCandidate(2, 5*mib, 97),
		makeCandidate(3, 200*mib, 50),
		makeCandidate(4, 1*mib, 25),
	)
	sel, hashes, freed := simulateEviction(cands, cap1G, cap1G)
	if len(sel) != 3 {
		t.Fatalf("mixed pass should pick 3 rows, got %d (%v)", len(sel), hashes)
	}
	want := []string{"hash-000000", "hash-000001", "hash-000002"}
	for i, h := range want {
		if hashes[i] != h {
			t.Fatalf("hashes[%d]=%s want %s", i, hashes[i], h)
		}
	}
	if got := sumSize(sel); got != freed {
		t.Fatalf("freed=%d != sumSize=%d", freed, got)
	}
	// Score-order respected: the giant 200 MiB low-score row must NOT appear.
	for _, m := range sel {
		if m.Hash == "hash-000003" {
			t.Fatalf("low-score giant row was selected — selector ignored cache_score order")
		}
	}
}

// TestSimulateEviction_NilAndAbsentRowsSkipped — rows whose file_path is
// already nil (carrying the -1 absence sentinel) must not contribute. They
// represent already-evicted entries and would otherwise inflate the freed
// total without actually reclaiming disk.
func TestSimulateEviction_NilAndAbsentRowsSkipped(t *testing.T) {
	const cap1G = 1 * gib
	absent := makeCandidate(7, 500*mib, 100)
	absent.FilePath = nil
	cands := sortedDesc(
		absent,
		makeCandidate(8, 60*mib, 99),
		makeCandidate(9, 60*mib, 98),
	)
	sel, _, freed := simulateEviction(cands, cap1G, cap1G)
	if len(sel) != 2 {
		t.Fatalf("absent row must be skipped; selected %d rows", len(sel))
	}
	if freed != 120*mib {
		t.Fatalf("freed=%d want %d (skipping the absent giant)", freed, 120*mib)
	}
}

// TestSimulateEviction_ExactlyAtTarget — a single file sized exactly equal to
// the 10% target must be enough to stop the walk (the >= comparison).
func TestSimulateEviction_ExactlyAtTarget(t *testing.T) {
	var cap1G int64 = 1 * gib
	target := int64(float64(cap1G) * 0.10)
	cands := sortedDesc(
		makeCandidate(1, target, 80),
		makeCandidate(2, target, 70),
	)
	sel, _, freed := simulateEviction(cands, cap1G, cap1G)
	if len(sel) != 1 || freed != target {
		t.Fatalf("exact-target pass: got %d rows / %d freed; want 1 / %d", len(sel), freed, target)
	}
}

// TestSelectByCumulativeSize_EmptyTarget — a zero/negative target is a no-op.
func TestSelectByCumulativeSize_EmptyTarget(t *testing.T) {
	cands := sortedDesc(makeCandidate(1, mib, 50))
	if got := selectByCumulativeSize(cands, 0); got != nil {
		t.Fatalf("target=0 should select nothing, got %d rows", len(got))
	}
	if got := selectByCumulativeSize(cands, -1); got != nil {
		t.Fatalf("target<0 should select nothing, got %d rows", len(got))
	}
}

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
