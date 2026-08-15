package batchupload

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/MayMistery/panpack/internal/baidu"
	"github.com/MayMistery/panpack/internal/resource"
)

type fakeUploadClient struct {
	remote      map[string]baidu.RemoteInfo
	failures    map[string]int
	uploadCalls map[string]int
}

func (f *fakeUploadClient) EnsureDir(context.Context, string) error { return nil }
func (f *fakeUploadClient) CurrentConcurrency() int                 { return 4 }
func (f *fakeUploadClient) RemoteInfo(_ context.Context, remotePath string) (baidu.RemoteInfo, error) {
	if info, ok := f.remote[remotePath]; ok {
		return info, nil
	}
	return baidu.RemoteInfo{}, baidu.ErrRemoteNotFound
}
func (f *fakeUploadClient) UploadHashedFile(_ context.Context, _, remotePath string, size int64, md5 string, _ []string) (baidu.UploadStats, error) {
	f.uploadCalls[remotePath]++
	if f.failures[remotePath] > 0 {
		f.failures[remotePath]--
		return baidu.UploadStats{}, errors.New("transient upload failure")
	}
	f.remote[remotePath] = baidu.RemoteInfo{Path: remotePath, Size: size, MD5: md5}
	return baidu.UploadStats{Concurrency: 4, Size: size, MD5: md5, Duration: 2 * time.Second}, nil
}

func TestBatchRecoversExactRemoteRetriesAndDeletesVerifiedFiles(t *testing.T) {
	sourceDir := t.TempDir()
	files := map[string][]byte{"a.tar": []byte("already remote"), "b.tar": []byte("retry me")}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(sourceDir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := resolveConfig(Config{
		SourceDir: sourceDir, Pattern: "*.tar", RemoteDir: "/apps/test/backup",
		StateFile: filepath.Join(sourceDir, "state.json"), DeleteAfterVerify: true,
		MaxFileAttempts: 3, RetryDelay: time.Millisecond, Limits: resource.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := loadOrCreateState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, _, aBlocks, err := baidu.HashFile(filepath.Join(sourceDir, "a.tar"), cfg.Limits.SliceSize)
	if err != nil {
		t.Fatal(err)
	}
	aRemoteMD5, err := baidu.RemoteMD5(aBlocks)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeUploadClient{
		remote: map[string]baidu.RemoteInfo{
			"/apps/test/backup/a.tar": {Path: "/apps/test/backup/a.tar", Size: int64(len(files["a.tar"])), MD5: aRemoteMD5},
		},
		failures: map[string]int{"/apps/test/backup/b.tar": 1}, uploadCalls: map[string]int{},
	}
	result, err := runWithClient(context.Background(), cfg, state, client, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if result.CompletedFiles != 2 || result.TotalFiles != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if client.uploadCalls["/apps/test/backup/a.tar"] != 0 || client.uploadCalls["/apps/test/backup/b.tar"] != 2 {
		t.Fatalf("unexpected upload calls: %+v", client.uploadCalls)
	}
	for name := range files {
		if _, err := os.Stat(filepath.Join(sourceDir, name)); !os.IsNotExist(err) {
			t.Fatalf("verified local file %s was not deleted", name)
		}
	}
	loaded, err := loadOrCreateState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Completed || !loaded.Files[0].Uploaded || !loaded.Files[1].Uploaded {
		t.Fatalf("state not completed: %+v", loaded)
	}
}

func TestBatchRefusesRemoteCollision(t *testing.T) {
	sourceDir := t.TempDir()
	localPath := filepath.Join(sourceDir, "chunk.tar")
	if err := os.WriteFile(localPath, []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := resolveConfig(Config{
		SourceDir: sourceDir, Pattern: "*.tar", RemoteDir: "/apps/test/backup",
		StateFile: filepath.Join(sourceDir, "state.json"), MaxFileAttempts: 1,
		RetryDelay: time.Millisecond, Limits: resource.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := loadOrCreateState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	remotePath := path.Join(cfg.RemoteDir, "chunk.tar")
	client := &fakeUploadClient{
		remote:   map[string]baidu.RemoteInfo{remotePath: {Path: remotePath, Size: 5, MD5: "wrong"}},
		failures: map[string]int{}, uploadCalls: map[string]int{},
	}
	if _, err := runWithClient(context.Background(), cfg, state, client, log.New(io.Discard, "", 0)); err == nil {
		t.Fatal("expected remote collision error")
	}
	if client.uploadCalls[remotePath] != 0 {
		t.Fatal("collision was overwritten")
	}
	if _, err := os.Stat(localPath); err != nil {
		t.Fatalf("local file was removed after collision: %v", err)
	}
}
