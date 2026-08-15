package state

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreResume(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "snap", "/src", "/apps/test/backup")
	if err != nil {
		t.Fatal(err)
	}
	v := Volume{
		Index:       0,
		Name:        "v0.tar",
		LocalPath:   filepath.Join(dir, "v0.tar"),
		RemotePath:  "/apps/test/backup/v0.tar",
		Size:        10,
		MD5:         "abc",
		BlockMD5s:   []string{"abc"},
		CursorAfter: Cursor{ManifestOffset: 42, NextVolume: 1, Done: true},
		SealedAt:    time.Now().UTC(),
	}
	if err := s.AddSealed(v); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUploaded(0); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkComplete(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, "snap", "/src", "/apps/test/backup")
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.Snapshot()
	if !got.Completed || len(got.Volumes) != 1 || !got.Volumes[0].Uploaded {
		t.Fatalf("unexpected resumed state: %+v", got)
	}
	if len(got.Volumes[0].BlockMD5s) != 0 {
		t.Fatalf("uploaded block hashes should be compacted")
	}
}
