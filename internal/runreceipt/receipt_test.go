package runreceipt

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRecorderPersistsRunningAndSuccessfulTerminalState(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	receiptFile := filepath.Join(dir, "run.json")
	if err := os.WriteFile(stateFile, []byte("state-v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder, err := Start(receiptFile, "upload-batch", stateFile)
	if err != nil {
		t.Fatal(err)
	}
	running, err := Load(receiptFile)
	if err != nil {
		t.Fatal(err)
	}
	if running.Status != StatusRunning || running.ExitCode != nil || running.FinishedAt != nil {
		t.Fatalf("unexpected running receipt: %+v", running)
	}
	if err := recorder.Finish(0, nil); err != nil {
		t.Fatal(err)
	}
	finished, err := VerifySucceeded(receiptFile, stateFile, "upload-batch")
	if err != nil {
		t.Fatal(err)
	}
	if finished.ExitCode == nil || *finished.ExitCode != 0 || finished.StateSHA256 == "" {
		t.Fatalf("unexpected terminal receipt: %+v", finished)
	}
}

func TestRecorderPersistsFailureAndRejectsConcurrentWriter(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	receiptFile := filepath.Join(dir, "run.json")
	if err := os.WriteFile(stateFile, []byte("state\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder, err := Start(receiptFile, "upload-batch", stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Start(receiptFile, "upload-batch", stateFile); err == nil {
		t.Fatal("expected concurrent receipt writer to fail")
	}
	if err := recorder.Finish(1, errors.New("upload failed")); err != nil {
		t.Fatal(err)
	}
	receipt, err := Load(receiptFile)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != StatusFailed || receipt.ExitCode == nil || *receipt.ExitCode != 1 || receipt.Error != "command failed; see process log" {
		t.Fatalf("unexpected failed receipt: %+v", receipt)
	}
	if _, err := VerifySucceeded(receiptFile, stateFile, "upload-batch"); err == nil {
		t.Fatal("failed receipt passed verification")
	}
}

func TestVerifySucceededBindsReceiptToStateContents(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	receiptFile := filepath.Join(dir, "run.json")
	if err := os.WriteFile(stateFile, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder, err := Start(receiptFile, "upload-batch", stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Finish(0, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySucceeded(receiptFile, stateFile, "upload-batch"); err == nil {
		t.Fatal("receipt accepted a modified state file")
	}
}
