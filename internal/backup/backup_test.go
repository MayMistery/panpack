package backup

import (
	"path/filepath"
	"testing"

	"github.com/MayMistery/panpack/internal/state"
)

func TestResolveRemoteDirGeneratesSnapshotScopedDefault(t *testing.T) {
	dir := t.TempDir()
	got, err := resolveRemoteDir(Config{StateDir: dir}, "20260816T120000Z-a1b2c3d4e5f6")
	if err != nil {
		t.Fatal(err)
	}
	want := "/apps/bypy/panpack-20260816T120000Z-a1b2c3d4e5f6"
	if got != want {
		t.Fatalf("default remote directory = %q, want %q", got, want)
	}
}

func TestResolveRemoteDirReusesStoredDirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := state.Open(dir, "snapshot", "/source", "/apps/bypy/legacy-backup"); err != nil {
		t.Fatal(err)
	}
	got, err := resolveRemoteDir(Config{StateDir: dir}, "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/apps/bypy/legacy-backup" {
		t.Fatalf("resumed remote directory = %q", got)
	}
}

func TestResolveRemoteDirKeepsExplicitDirectory(t *testing.T) {
	got, err := resolveRemoteDir(Config{StateDir: filepath.Join(t.TempDir(), "missing"), RemoteDir: "/apps/custom/backup"}, "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/apps/custom/backup" {
		t.Fatalf("explicit remote directory = %q", got)
	}
}

func TestResolveRemoteDirRejectsUnsafeSnapshotID(t *testing.T) {
	if _, err := resolveRemoteDir(Config{StateDir: t.TempDir()}, "../escape"); err == nil {
		t.Fatal("unsafe snapshot ID was accepted")
	}
}
