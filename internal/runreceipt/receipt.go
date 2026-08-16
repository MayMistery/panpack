package runreceipt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const FormatVersion = 1

type Status string

const (
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

type Receipt struct {
	FormatVersion int        `json:"format_version"`
	Command       string     `json:"command"`
	PID           int        `json:"pid"`
	Status        Status     `json:"status"`
	StateFile     string     `json:"state_file"`
	StateSHA256   string     `json:"state_sha256,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	ExitCode      *int       `json:"exit_code,omitempty"`
	Error         string     `json:"error,omitempty"`
}

type Recorder struct {
	path    string
	lock    *os.File
	receipt Receipt
}

func Start(receiptFile, command, stateFile string) (*Recorder, error) {
	if receiptFile == "" {
		return nil, errors.New("receipt file is required")
	}
	if command == "" {
		return nil, errors.New("receipt command is required")
	}
	absReceipt, err := filepath.Abs(receiptFile)
	if err != nil {
		return nil, fmt.Errorf("absolute receipt path: %w", err)
	}
	absState, err := filepath.Abs(stateFile)
	if err != nil {
		return nil, fmt.Errorf("absolute state path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absReceipt), 0o700); err != nil {
		return nil, fmt.Errorf("create receipt directory: %w", err)
	}
	lock, err := os.OpenFile(absReceipt+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open receipt lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("another run holds receipt %s: %w", absReceipt, err)
	}
	recorder := &Recorder{
		path: absReceipt,
		lock: lock,
		receipt: Receipt{
			FormatVersion: FormatVersion,
			Command:       command,
			PID:           os.Getpid(),
			Status:        StatusRunning,
			StateFile:     absState,
			StartedAt:     time.Now().UTC(),
		},
	}
	if err := recorder.save(); err != nil {
		recorder.unlock()
		return nil, err
	}
	return recorder, nil
}

func (r *Recorder) Path() string {
	if r == nil {
		return ""
	}
	return r.path
}

func (r *Recorder) Finish(exitCode int, runErr error) error {
	if r == nil {
		return nil
	}
	defer r.unlock()
	now := time.Now().UTC()
	r.receipt.FinishedAt = &now
	r.receipt.ExitCode = &exitCode
	if exitCode == 0 && runErr == nil {
		r.receipt.Status = StatusSucceeded
	} else {
		r.receipt.Status = StatusFailed
		if runErr != nil {
			// Keep credentials and signed API URLs out of durable status files.
			r.receipt.Error = "command failed; see process log"
		}
	}
	if digest, err := FileSHA256(r.receipt.StateFile); err == nil {
		r.receipt.StateSHA256 = digest
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("hash receipt state file: %w", err)
	}
	return r.save()
}

func Load(receiptFile string) (Receipt, error) {
	data, err := os.ReadFile(receiptFile)
	if err != nil {
		return Receipt{}, err
	}
	var receipt Receipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return Receipt{}, fmt.Errorf("decode run receipt: %w", err)
	}
	if receipt.FormatVersion != FormatVersion {
		return Receipt{}, fmt.Errorf("unsupported run receipt format %d", receipt.FormatVersion)
	}
	return receipt, nil
}

func VerifySucceeded(receiptFile, stateFile, command string) (Receipt, error) {
	receipt, err := Load(receiptFile)
	if err != nil {
		return Receipt{}, err
	}
	absState, err := filepath.Abs(stateFile)
	if err != nil {
		return Receipt{}, err
	}
	if receipt.Command != command {
		return receipt, fmt.Errorf("receipt command is %q, expected %q", receipt.Command, command)
	}
	if filepath.Clean(receipt.StateFile) != filepath.Clean(absState) {
		return receipt, fmt.Errorf("receipt state file is %q, expected %q", receipt.StateFile, absState)
	}
	if receipt.Status != StatusSucceeded || receipt.FinishedAt == nil || receipt.ExitCode == nil || *receipt.ExitCode != 0 {
		exitCode := "missing"
		if receipt.ExitCode != nil {
			exitCode = fmt.Sprintf("%d", *receipt.ExitCode)
		}
		return receipt, fmt.Errorf("receipt is not a successful terminal run: status=%s exit_code=%s", receipt.Status, exitCode)
	}
	digest, err := FileSHA256(absState)
	if err != nil {
		return receipt, err
	}
	if receipt.StateSHA256 == "" || receipt.StateSHA256 != digest {
		return receipt, errors.New("receipt state checksum does not match the current state file")
	}
	return receipt, nil
}

func FileSHA256(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (r *Recorder) save() error {
	data, err := json.MarshalIndent(r.receipt, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := r.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, r.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	dir, err := os.Open(filepath.Dir(r.path))
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr = dir.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (r *Recorder) unlock() {
	if r.lock == nil {
		return
	}
	_ = syscall.Flock(int(r.lock.Fd()), syscall.LOCK_UN)
	_ = r.lock.Close()
	r.lock = nil
}
