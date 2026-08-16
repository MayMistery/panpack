package audit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MayMistery/panpack/internal/baidu"
	"github.com/MayMistery/panpack/internal/batchupload"
	"github.com/MayMistery/panpack/internal/runreceipt"
)

type fakeRemoteClient struct {
	entries  []baidu.RemoteInfo
	metadata map[int64]baidu.RemoteInfo
	onList   func()
}

func (f *fakeRemoteClient) ListDir(context.Context, string) ([]baidu.RemoteInfo, error) {
	if f.onList != nil {
		f.onList()
	}
	return append([]baidu.RemoteInfo(nil), f.entries...), nil
}

func (f *fakeRemoteClient) Metadata(_ context.Context, ids []int64) (map[int64]baidu.RemoteInfo, error) {
	result := make(map[int64]baidu.RemoteInfo, len(ids))
	for _, id := range ids {
		if info, ok := f.metadata[id]; ok {
			result[id] = info
		}
	}
	return result, nil
}

func TestAuditProvesExactSetSizesChecksumsLocalCleanupAndReceipt(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	receiptFile := stateFile + ".receipt.json"
	state := batchupload.State{
		FormatVersion: batchupload.FormatVersion,
		SourceDir:     dir,
		Pattern:       "chunk_*.tar",
		RemoteDir:     "/apps/test/backup",
		Files: []batchupload.FileRecord{
			{Name: "chunk_0001.tar", Size: 10, MD5: "full-1", RemoteMD5: "remote-1", Uploaded: true},
			{Name: "chunk_0002.tar", Size: 20, MD5: "full-2", RemoteMD5: "remote-2", Uploaded: true},
		},
		TotalBytes: 30,
		Completed:  true,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}
	writeJSON(t, stateFile, state)
	recorder, err := runreceipt.Start(receiptFile, "upload-batch", stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Finish(0, nil); err != nil {
		t.Fatal(err)
	}
	client := &fakeRemoteClient{
		entries: []baidu.RemoteInfo{
			{FsID: 1, Name: "chunk_0000.tar", Path: "/apps/test/backup/chunk_0000.tar", Size: 5},
			{FsID: 2, Name: "chunk_0001.tar", Path: "/apps/test/backup/chunk_0001.tar", Size: 10},
			{FsID: 3, Name: "chunk_0002.tar", Path: "/apps/test/backup/chunk_0002.tar", Size: 20},
			{FsID: 4, Name: "notes.txt", Path: "/apps/test/backup/notes.txt", Size: 1},
		},
		metadata: map[int64]baidu.RemoteInfo{
			2: {FsID: 2, MD5: "remote-1", Size: 10},
			3: {FsID: 3, MD5: "remote-2", Size: 20},
		},
	}
	expected, err := GenerateExpected("chunk_%04d.tar", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	result, err := RunWithClient(context.Background(), Config{
		StateFile: stateFile, ExpectedNames: expected, RequireLocalEmpty: true,
		RequireChecksum: true, ReceiptFile: receiptFile,
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Passed || result.RemoteUnique != 3 || result.ExpectedEntries != 3 || result.FrozenSizeMatched != 2 || result.FrozenChecksumMatched != 2 {
		t.Fatalf("unexpected audit result: %+v", result)
	}
	if !result.Receipt.Checked || result.Receipt.ExitCode == nil || *result.Receipt.ExitCode != 0 {
		t.Fatalf("receipt was not verified: %+v", result.Receipt)
	}
}

func TestAuditRejectsMissingExtraSizeMismatchLocalFileAndStaleReceipt(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	receiptFile := stateFile + ".receipt.json"
	state := batchupload.State{
		FormatVersion: batchupload.FormatVersion,
		SourceDir:     dir,
		Pattern:       "chunk_*.tar",
		RemoteDir:     "/apps/test/backup",
		Files:         []batchupload.FileRecord{{Name: "chunk_0001.tar", Size: 10, MD5: "full", RemoteMD5: "remote", Uploaded: true}},
		TotalBytes:    10,
		Completed:     true,
	}
	writeJSON(t, stateFile, state)
	recorder, err := runreceipt.Start(receiptFile, "upload-batch", stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Finish(0, nil); err != nil {
		t.Fatal(err)
	}
	state.UpdatedAt = time.Now().UTC()
	writeJSON(t, stateFile, state)
	if err := os.WriteFile(filepath.Join(dir, "chunk_0001.tar"), []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &fakeRemoteClient{
		entries: []baidu.RemoteInfo{
			{FsID: 1, Name: "chunk_0001.tar", Size: 11},
			{FsID: 2, Name: "chunk_0003.tar", Size: 3},
		},
		metadata: map[int64]baidu.RemoteInfo{1: {FsID: 1, MD5: "wrong", Size: 11}},
	}
	expected, err := GenerateExpected("chunk_%04d.tar", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	result, err := RunWithClient(context.Background(), Config{
		StateFile: stateFile, ExpectedNames: expected, RequireLocalEmpty: true,
		RequireChecksum: true, ReceiptFile: receiptFile,
	}, client)
	if err == nil {
		t.Fatal("invalid audit unexpectedly passed")
	}
	if result.Passed || len(result.Missing) != 2 || len(result.Extra) != 1 || len(result.FrozenSizeMismatches) != 1 || len(result.FrozenChecksumMismatches) != 1 || len(result.LocalMatching) != 1 || result.Receipt.Error == "" {
		t.Fatalf("audit did not report all failures: %+v", result)
	}
}

func TestAuditSupportsLegacyStateWithoutStoredCompositeChecksum(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	state := batchupload.State{
		FormatVersion: batchupload.FormatVersion,
		SourceDir:     dir,
		Pattern:       "chunk_*.tar",
		RemoteDir:     "/apps/test/backup",
		Files:         []batchupload.FileRecord{{Name: "chunk_0000.tar", Size: 10, MD5: "whole-file", Uploaded: true}},
		TotalBytes:    10,
		Completed:     true,
	}
	writeJSON(t, stateFile, state)
	if err := os.WriteFile(stateFile+".receipt.json", []byte("legacy receipt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &fakeRemoteClient{
		entries:  []baidu.RemoteInfo{{FsID: 1, Name: "chunk_0000.tar", Size: 10}},
		metadata: map[int64]baidu.RemoteInfo{1: {FsID: 1, MD5: "legacy-composite", Size: 10}},
	}
	result, err := RunWithClient(context.Background(), Config{StateFile: stateFile, RequireLocalEmpty: true}, client)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Passed || len(result.FrozenChecksumUnavailable) != 1 {
		t.Fatalf("legacy checksum evidence was not reported: %+v", result)
	}
	if _, err := RunWithClient(context.Background(), Config{StateFile: stateFile, RequireChecksum: true}, client); err == nil {
		t.Fatal("strict checksum audit accepted legacy state without a stored composite checksum")
	}
}

func TestAuditRejectsStateChangedDuringRemoteVerification(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	state := batchupload.State{
		FormatVersion: batchupload.FormatVersion,
		SourceDir:     dir,
		Pattern:       "chunk_*.tar",
		RemoteDir:     "/apps/test/backup",
		Files:         []batchupload.FileRecord{{Name: "chunk_0000.tar", Size: 10, MD5: "whole", RemoteMD5: "remote", Uploaded: true}},
		TotalBytes:    10,
		Completed:     true,
	}
	writeJSON(t, stateFile, state)
	client := &fakeRemoteClient{
		entries:  []baidu.RemoteInfo{{FsID: 1, Name: "chunk_0000.tar", Size: 10}},
		metadata: map[int64]baidu.RemoteInfo{1: {FsID: 1, MD5: "remote", Size: 10}},
		onList: func() {
			state.UpdatedAt = time.Now().UTC()
			writeJSON(t, stateFile, state)
		},
	}
	result, err := RunWithClient(context.Background(), Config{StateFile: stateFile}, client)
	if err == nil {
		t.Fatal("audit accepted a state file modified during verification")
	}
	if result.Passed || len(result.Issues) == 0 || result.Issues[len(result.Issues)-1] != "upload state changed during audit" {
		t.Fatalf("state mutation was not reported: %+v", result)
	}
}

func TestAuditRequiresAuthoritativeMetadataForFrozenFiles(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	state := batchupload.State{
		FormatVersion: batchupload.FormatVersion,
		SourceDir:     dir,
		Pattern:       "chunk_*.tar",
		RemoteDir:     "/apps/test/backup",
		Files:         []batchupload.FileRecord{{Name: "chunk_0000.tar", Size: 10, MD5: "whole", RemoteMD5: "remote", Uploaded: true}},
		TotalBytes:    10,
		Completed:     true,
	}
	writeJSON(t, stateFile, state)
	client := &fakeRemoteClient{
		entries:  []baidu.RemoteInfo{{FsID: 1, Name: "chunk_0000.tar", Size: 10}},
		metadata: map[int64]baidu.RemoteInfo{},
	}
	result, err := RunWithClient(context.Background(), Config{StateFile: stateFile}, client)
	if err == nil {
		t.Fatal("audit accepted a frozen file without authoritative metadata")
	}
	if len(result.FrozenMetadataMissing) != 1 || result.FrozenMetadataMissing[0] != "chunk_0000.tar" {
		t.Fatalf("missing metadata was not reported: %+v", result)
	}
}

func TestGenerateExpectedRejectsUnsafeOrInvalidTemplates(t *testing.T) {
	if _, err := GenerateExpected("../chunk_%d.tar", 0, 1); err == nil {
		t.Fatal("unsafe template accepted")
	}
	if _, err := GenerateExpected("chunk_%s.tar", 0, 1); err == nil {
		t.Fatal("invalid format verb accepted")
	}
	if _, err := GenerateExpected("chunk.tar", 0, 1); err == nil {
		t.Fatal("duplicate-producing template accepted")
	}
	if _, err := GenerateExpected("chunk_%d.tar", -1, 1); err == nil {
		t.Fatal("negative sequence index accepted")
	}
}

func writeJSON(t *testing.T, filePath string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
