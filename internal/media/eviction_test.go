package media

import (
	"os"
	"path/filepath"
	"testing"
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

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
