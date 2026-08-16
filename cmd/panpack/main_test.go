package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MayMistery/panpack/internal/runreceipt"
)

func TestUploadBatchPersistsFailedRunReceipt(t *testing.T) {
	t.Setenv("PANPACK_ACCESS_TOKEN", "")
	t.Setenv("PANPACK_TOKEN_FILE", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "chunk_0000.tar"), []byte("sealed"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(dir, "state.json")
	receiptFile := filepath.Join(dir, "receipt.json")
	badToken := filepath.Join(dir, "invalid-credentials.json")
	if err := os.WriteFile(badToken, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := runUploadBatch(context.Background(), []string{
		"--source-dir", dir,
		"--pattern", "chunk_*.tar",
		"--remote-dir", "/apps/test/backup",
		"--state-file", stateFile,
		"--receipt-file", receiptFile,
		"--token-file", badToken,
		"--min-free", "1MiB",
		"--reserve-fraction", "0",
	}, &stdout, &stderr)
	if err == nil {
		t.Fatal("upload unexpectedly succeeded without credentials")
	}
	receipt, loadErr := runreceipt.Load(receiptFile)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if receipt.Status != runreceipt.StatusFailed || receipt.ExitCode == nil || *receipt.ExitCode != 1 || receipt.FinishedAt == nil || receipt.StateSHA256 == "" {
		t.Fatalf("unexpected failure receipt: %+v", receipt)
	}
}

func TestReceiptPathDashDisablesReceipt(t *testing.T) {
	got, err := receiptPath("-", filepath.Join(t.TempDir(), "default.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("disabled receipt resolved to %q", got)
	}
}

func TestPlanGeneratesStableSnapshotScopedRemoteDirectory(t *testing.T) {
	sourceDir := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), "state")
	args := []string{
		"--source", sourceDir,
		"--state-dir", stateDir,
		"--min-free", "1MiB",
		"--reserve-fraction", "0",
		"--min-volume-size", "1MiB",
		"--max-volume-size", "2MiB",
	}
	runPlan := func() string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if err := runBackup(context.Background(), args, &stdout, &stderr, true); err != nil {
			t.Fatalf("plan failed: %v\nstderr: %s", err, stderr.String())
		}
		marker := "remote="
		index := strings.LastIndex(stdout.String(), marker)
		if index < 0 {
			t.Fatalf("plan did not print a remote directory: %s", stdout.String())
		}
		return strings.TrimSpace(stdout.String()[index+len(marker):])
	}
	first := runPlan()
	second := runPlan()
	if !strings.HasPrefix(first, "/apps/bypy/panpack-") {
		t.Fatalf("unexpected generated remote directory %q", first)
	}
	if first != second {
		t.Fatalf("generated remote directory changed across resume: %q != %q", first, second)
	}
}
