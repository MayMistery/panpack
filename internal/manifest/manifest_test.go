package manifest

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildStreamsManifestAndExcludesState(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "dir", "b.txt"), []byte("beta"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(source, ".panpack")

	snapshot, err := Build(context.Background(), ScanOptions{Source: source, StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.FileCount != 2 || snapshot.TotalFileBytes != 9 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if snapshot.EntryCount != 3 {
		t.Fatalf("entry_count=%d, want 3", snapshot.EntryCount)
	}

	r, err := OpenReader(filepath.Join(stateDir, snapshot.ManifestFile), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var paths []string
	for {
		entry, _, _, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, entry.Path)
	}
	for _, p := range paths {
		if p == ".panpack" || filepath.Dir(p) == ".panpack" {
			t.Fatalf("state directory leaked into manifest: %q", p)
		}
	}
}
