package archive_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	packarchive "github.com/MayMistery/panpack/internal/archive"
	"github.com/MayMistery/panpack/internal/manifest"
	"github.com/MayMistery/panpack/internal/restore"
	"github.com/MayMistery/panpack/internal/state"
)

func TestPackAndRestoreWithOversizedFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	stateDir := filepath.Join(source, ".panpack")
	staging := filepath.Join(stateDir, "staging")
	destination := filepath.Join(root, "restored")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	small := []byte("small file\n")
	large := bytes.Repeat([]byte("0123456789abcdef"), 30_000) // 480 KiB
	if err := os.WriteFile(filepath.Join(source, "small.txt"), small, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "large.bin"), large, 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot, err := manifest.Build(ctx, manifest.ScanOptions{Source: source, StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	p := &packarchive.Packer{
		Source:       source,
		ManifestPath: filepath.Join(stateDir, snapshot.ManifestFile),
		ManifestSize: snapshot.ManifestSize,
		StagingDir:   staging,
		SnapshotID:   snapshot.ID,
		BlockSize:    16 * 1024,
	}
	cursor := state.Cursor{}
	volumeCount := 0
	for {
		v, err := p.PackNext(ctx, cursor, 128*1024, "/apps/test/backup")
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if v.Size > 128*1024 {
			t.Fatalf("volume %s exceeds cap: %d", v.Name, v.Size)
		}
		cursor = v.CursorAfter
		volumeCount++
	}
	if volumeCount < 3 {
		t.Fatalf("expected large file to span volumes, got %d", volumeCount)
	}

	err = restore.Run(ctx, restore.Options{
		SnapshotPath: filepath.Join(stateDir, "snapshot.json"),
		ManifestPath: filepath.Join(stateDir, snapshot.CompressedManifest),
		VolumesDir:   staging,
		Destination:  destination,
	})
	if err != nil {
		t.Fatal(err)
	}
	gotSmall, err := os.ReadFile(filepath.Join(destination, "small.txt"))
	if err != nil {
		t.Fatal(err)
	}
	gotLarge, err := os.ReadFile(filepath.Join(destination, "nested", "large.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotSmall, small) || !bytes.Equal(gotLarge, large) {
		t.Fatal("restored content differs")
	}
}
