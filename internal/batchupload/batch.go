package batchupload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/MayMistery/panpack/internal/baidu"
	"github.com/MayMistery/panpack/internal/credentials"
	"github.com/MayMistery/panpack/internal/resource"
)

const FormatVersion = 1

type Config struct {
	SourceDir         string
	Pattern           string
	RemoteDir         string
	StateFile         string
	ReceiptFile       string
	TokenFile         string
	DeleteAfterVerify bool
	MaxFileAttempts   int
	RetryDelay        time.Duration
	Limits            resource.Limits
	Logger            *log.Logger
}

type FileRecord struct {
	Name            string    `json:"name"`
	Size            int64     `json:"size"`
	MD5             string    `json:"md5,omitempty"`
	RemoteMD5       string    `json:"remote_md5,omitempty"`
	Attempts        int       `json:"attempts"`
	Uploaded        bool      `json:"uploaded"`
	UploadedAt      time.Time `json:"uploaded_at,omitempty"`
	DurationSeconds float64   `json:"duration_seconds,omitempty"`
	Retries         int       `json:"request_retries,omitempty"`
	RateLimits      int       `json:"rate_limits,omitempty"`
}

type State struct {
	FormatVersion int          `json:"format_version"`
	SourceDir     string       `json:"source_dir"`
	Pattern       string       `json:"pattern"`
	RemoteDir     string       `json:"remote_dir"`
	Files         []FileRecord `json:"files"`
	TotalBytes    int64        `json:"total_bytes"`
	Completed     bool         `json:"completed"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
}

type Result struct {
	TotalFiles     int
	CompletedFiles int
	TotalBytes     int64
	CompletedBytes int64
	StateFile      string
}

type uploadClient interface {
	EnsureDir(context.Context, string) error
	RemoteInfo(context.Context, string) (baidu.RemoteInfo, error)
	UploadHashedFile(context.Context, string, string, int64, string, []string) (baidu.UploadStats, error)
	CurrentConcurrency() int
}

func Run(ctx context.Context, cfg Config) (Result, error) {
	resolved, err := resolveConfig(cfg)
	if err != nil {
		return Result{}, err
	}
	cfg = resolved
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}

	lock, err := lockState(cfg.StateFile)
	if err != nil {
		return Result{}, err
	}
	defer unlockState(lock)

	state, err := loadOrCreateState(cfg)
	if err != nil {
		return Result{}, err
	}
	if err := validateStateFiles(cfg, state); err != nil {
		return Result{}, err
	}

	resources, err := resource.Detect(cfg.SourceDir)
	if err != nil {
		return Result{}, err
	}
	policy, err := resource.Plan(resources, cfg.Limits)
	if err != nil {
		return Result{}, err
	}
	creds, credentialSource, err := credentials.Discover(cfg.TokenFile)
	if err != nil {
		return Result{}, err
	}
	logger.Printf("credentials loaded from %s", credentialSource)
	logger.Printf("batch policy: upload concurrency=%d..%d, disk reserve=%d bytes", policy.InitialConcurrency, policy.MaxConcurrency, policy.ReserveBytes)
	client, err := baidu.New(creds.AccessToken, cfg.Limits.SliceSize, policy.InitialConcurrency, policy.MaxConcurrency, logger)
	if err != nil {
		return Result{}, err
	}
	return runWithClient(ctx, cfg, state, client, logger)
}

func runWithClient(ctx context.Context, cfg Config, state *State, client uploadClient, logger *log.Logger) (Result, error) {
	if err := client.EnsureDir(ctx, cfg.RemoteDir); err != nil {
		return resultFromState(cfg.StateFile, state), err
	}
	initial := resultFromState(cfg.StateFile, state)
	logger.Printf("frozen batch: files=%d completed=%d bytes=%d completed_bytes=%d", initial.TotalFiles, initial.CompletedFiles, initial.TotalBytes, initial.CompletedBytes)
	for i := range state.Files {
		record := &state.Files[i]
		localPath := filepath.Join(cfg.SourceDir, record.Name)
		if record.Uploaded {
			if cfg.DeleteAfterVerify {
				if err := removeIfPresent(localPath); err != nil {
					return resultFromState(cfg.StateFile, state), err
				}
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			return resultFromState(cfg.StateFile, state), err
		}

		info, err := os.Lstat(localPath)
		if err != nil {
			return resultFromState(cfg.StateFile, state), fmt.Errorf("stat pending file %s: %w", record.Name, err)
		}
		if !info.Mode().IsRegular() || info.Size() != record.Size {
			return resultFromState(cfg.StateFile, state), fmt.Errorf("pending file %s changed: regular=%v size=%d expected=%d", record.Name, info.Mode().IsRegular(), info.Size(), record.Size)
		}
		logger.Printf("hashing %s (%d bytes)", record.Name, record.Size)
		size, fullMD5, blocks, err := baidu.HashFile(localPath, cfg.Limits.SliceSize)
		if err != nil {
			return resultFromState(cfg.StateFile, state), fmt.Errorf("hash %s: %w", record.Name, err)
		}
		if size != record.Size || (record.MD5 != "" && !strings.EqualFold(record.MD5, fullMD5)) {
			return resultFromState(cfg.StateFile, state), fmt.Errorf("pending file %s changed while hashing", record.Name)
		}
		record.MD5 = fullMD5
		remoteMD5, err := baidu.RemoteMD5(blocks)
		if err != nil {
			return resultFromState(cfg.StateFile, state), fmt.Errorf("calculate remote checksum for %s: %w", record.Name, err)
		}
		record.RemoteMD5 = remoteMD5
		state.UpdatedAt = time.Now().UTC()
		if err := saveState(cfg.StateFile, state); err != nil {
			return resultFromState(cfg.StateFile, state), err
		}

		remotePath := path.Join(cfg.RemoteDir, record.Name)
		completed := false
		var lastErr error
		for runAttempt := 1; runAttempt <= cfg.MaxFileAttempts; runAttempt++ {
			if err := ctx.Err(); err != nil {
				return resultFromState(cfg.StateFile, state), err
			}
			record.Attempts++
			state.UpdatedAt = time.Now().UTC()
			if err := saveState(cfg.StateFile, state); err != nil {
				return resultFromState(cfg.StateFile, state), err
			}

			remote, remoteErr := client.RemoteInfo(ctx, remotePath)
			if remoteErr == nil {
				if remote.Size != size || !baidu.RemoteMD5Matches(remote.MD5, fullMD5, blocks) {
					return resultFromState(cfg.StateFile, state), fmt.Errorf("refusing to overwrite remote collision %s: remote size/md5 differs", remotePath)
				}
				logger.Printf("recovered already-uploaded %s from matching remote metadata", record.Name)
				completeRecord(record, 0, baidu.UploadStats{Size: size, MD5: fullMD5, Rapid: true})
				completed = true
			} else if !errors.Is(remoteErr, baidu.ErrRemoteNotFound) {
				lastErr = fmt.Errorf("preflight remote metadata %s: %w", record.Name, remoteErr)
			} else {
				logger.Printf("uploading %s with concurrency %d (file attempt %d/%d)", record.Name, client.CurrentConcurrency(), runAttempt, cfg.MaxFileAttempts)
				started := time.Now()
				stats, uploadErr := client.UploadHashedFile(ctx, localPath, remotePath, size, fullMD5, blocks)
				if uploadErr == nil {
					completeRecord(record, time.Since(started), stats)
					completed = true
				} else {
					lastErr = uploadErr
				}
			}

			if completed {
				state.UpdatedAt = time.Now().UTC()
				if err := saveState(cfg.StateFile, state); err != nil {
					return resultFromState(cfg.StateFile, state), err
				}
				if cfg.DeleteAfterVerify {
					if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
						return resultFromState(cfg.StateFile, state), fmt.Errorf("remove verified file %s: %w", record.Name, err)
					}
				}
				logProgress(logger, state, record)
				break
			}
			if runAttempt < cfg.MaxFileAttempts {
				delay := retryDelay(cfg.RetryDelay, runAttempt)
				logger.Printf("file attempt failed for %s: %v; retrying in %s", record.Name, lastErr, delay)
				if err := waitContext(ctx, delay); err != nil {
					return resultFromState(cfg.StateFile, state), err
				}
			}
		}
		if !completed {
			return resultFromState(cfg.StateFile, state), fmt.Errorf("upload %s failed after %d file attempts: %w", record.Name, cfg.MaxFileAttempts, lastErr)
		}
	}

	state.Completed = true
	state.UpdatedAt = time.Now().UTC()
	if err := saveState(cfg.StateFile, state); err != nil {
		return resultFromState(cfg.StateFile, state), err
	}
	return resultFromState(cfg.StateFile, state), nil
}

func resolveConfig(cfg Config) (Config, error) {
	if cfg.SourceDir == "" {
		return cfg, errors.New("source directory is required")
	}
	sourceDir, err := filepath.Abs(cfg.SourceDir)
	if err != nil {
		return cfg, err
	}
	cfg.SourceDir = filepath.Clean(sourceDir)
	info, err := os.Stat(cfg.SourceDir)
	if err != nil {
		return cfg, err
	}
	if !info.IsDir() {
		return cfg, fmt.Errorf("source is not a directory: %s", cfg.SourceDir)
	}
	if cfg.Pattern == "" {
		cfg.Pattern = "*.tar"
	}
	if filepath.Base(cfg.Pattern) != cfg.Pattern || strings.Contains(cfg.Pattern, string(os.PathSeparator)) || strings.Contains(cfg.Pattern, "/") {
		return cfg, fmt.Errorf("pattern must match basenames only: %q", cfg.Pattern)
	}
	cleanRemote := path.Clean(cfg.RemoteDir)
	if cfg.RemoteDir == "" || !strings.HasPrefix(cleanRemote, "/apps/") {
		return cfg, fmt.Errorf("remote directory must be under /apps/<app>: %q", cfg.RemoteDir)
	}
	cfg.RemoteDir = cleanRemote
	if cfg.StateFile == "" {
		cfg.StateFile = filepath.Join(cfg.SourceDir, ".panpack-upload-state.json")
	}
	stateFile, err := filepath.Abs(cfg.StateFile)
	if err != nil {
		return cfg, err
	}
	cfg.StateFile = filepath.Clean(stateFile)
	if cfg.ReceiptFile != "" {
		receiptFile, err := filepath.Abs(cfg.ReceiptFile)
		if err != nil {
			return cfg, err
		}
		cfg.ReceiptFile = filepath.Clean(receiptFile)
	}
	if cfg.MaxFileAttempts <= 0 {
		cfg.MaxFileAttempts = 20
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = time.Minute
	}
	if cfg.Limits.SliceSize == 0 {
		cfg.Limits = resource.DefaultLimits()
	}
	if err := os.MkdirAll(filepath.Dir(cfg.StateFile), 0o700); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func loadOrCreateState(cfg Config) (*State, error) {
	state, err := LoadState(cfg.StateFile)
	if err == nil {
		if state.FormatVersion != FormatVersion || state.SourceDir != cfg.SourceDir || state.Pattern != cfg.Pattern || state.RemoteDir != cfg.RemoteDir {
			return nil, errors.New("upload state does not match the requested source, pattern, or remote directory")
		}
		return state, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(cfg.SourceDir, cfg.Pattern))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	matches = filterControlFiles(matches, cfg)
	if len(matches) == 0 {
		return nil, fmt.Errorf("no files match %s", filepath.Join(cfg.SourceDir, cfg.Pattern))
	}
	now := time.Now().UTC()
	state = &State{FormatVersion: FormatVersion, SourceDir: cfg.SourceDir, Pattern: cfg.Pattern, RemoteDir: cfg.RemoteDir, CreatedAt: now, UpdatedAt: now}
	for _, match := range matches {
		info, err := os.Lstat(match)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("matched path is not a regular file: %s", match)
		}
		name := filepath.Base(match)
		state.Files = append(state.Files, FileRecord{Name: name, Size: info.Size()})
		state.TotalBytes += info.Size()
	}
	if err := saveState(cfg.StateFile, state); err != nil {
		return nil, err
	}
	return state, nil
}

func LoadState(stateFile string) (*State, error) {
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return nil, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode upload state: %w", err)
	}
	if state.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("unsupported upload state format %d", state.FormatVersion)
	}
	return &state, nil
}

func validateStateFiles(cfg Config, state *State) error {
	known := make(map[string]*FileRecord, len(state.Files))
	var totalBytes int64
	allUploaded := true
	for i := range state.Files {
		record := &state.Files[i]
		if record.Name == "" || filepath.Base(record.Name) != record.Name {
			return fmt.Errorf("unsafe filename in upload state: %q", record.Name)
		}
		if _, exists := known[record.Name]; exists {
			return fmt.Errorf("duplicate filename in upload state: %s", record.Name)
		}
		known[record.Name] = record
		totalBytes += record.Size
		if record.Uploaded {
			if record.MD5 == "" {
				return fmt.Errorf("uploaded state file %s has no verified md5", record.Name)
			}
			continue
		}
		allUploaded = false
		info, err := os.Lstat(filepath.Join(cfg.SourceDir, record.Name))
		if err != nil {
			return fmt.Errorf("pending state file %s is unavailable: %w", record.Name, err)
		}
		if !info.Mode().IsRegular() || info.Size() != record.Size {
			return fmt.Errorf("pending state file %s changed", record.Name)
		}
	}
	if totalBytes != state.TotalBytes {
		return fmt.Errorf("upload state total bytes changed: records=%d state=%d", totalBytes, state.TotalBytes)
	}
	if state.Completed && !allUploaded {
		return errors.New("upload state is marked complete but still has pending files")
	}
	matches, err := filepath.Glob(filepath.Join(cfg.SourceDir, cfg.Pattern))
	if err != nil {
		return err
	}
	for _, match := range matches {
		if isControlFile(match, cfg) {
			continue
		}
		if _, ok := known[filepath.Base(match)]; !ok {
			return fmt.Errorf("new matching file is absent from immutable upload state: %s", match)
		}
	}
	return nil
}

func filterControlFiles(matches []string, cfg Config) []string {
	filtered := matches[:0]
	for _, match := range matches {
		if !isControlFile(match, cfg) {
			filtered = append(filtered, match)
		}
	}
	return filtered
}

func isControlFile(filePath string, cfg Config) bool {
	filePath = filepath.Clean(filePath)
	defaultReceipt := cfg.StateFile + ".receipt.json"
	control := []string{
		cfg.StateFile, cfg.StateFile + ".tmp", cfg.StateFile + ".lock",
		defaultReceipt, defaultReceipt + ".tmp", defaultReceipt + ".lock",
	}
	if cfg.ReceiptFile != "" {
		control = append(control, cfg.ReceiptFile, cfg.ReceiptFile+".tmp", cfg.ReceiptFile+".lock")
	}
	for _, candidate := range control {
		if filePath == filepath.Clean(candidate) {
			return true
		}
	}
	return false
}

func completeRecord(record *FileRecord, duration time.Duration, stats baidu.UploadStats) {
	record.MD5 = stats.MD5
	record.Uploaded = true
	record.UploadedAt = time.Now().UTC()
	record.DurationSeconds = duration.Seconds()
	record.Retries = stats.Retries
	record.RateLimits = stats.RateLimits
}

func logProgress(logger *log.Logger, state *State, latest *FileRecord) {
	result := resultFromState("", state)
	var measuredBytes int64
	var measuredSeconds float64
	for i := range state.Files {
		if state.Files[i].Uploaded && state.Files[i].DurationSeconds > 0 {
			measuredBytes += state.Files[i].Size
			measuredSeconds += state.Files[i].DurationSeconds
		}
	}
	remaining := state.TotalBytes - result.CompletedBytes
	eta := time.Duration(0)
	rateMiB := float64(0)
	if measuredBytes > 0 && measuredSeconds > 0 {
		rate := float64(measuredBytes) / measuredSeconds
		rateMiB = rate / float64(1<<20)
		eta = time.Duration(float64(remaining)/rate) * time.Second
	}
	logger.Printf("verified %s; progress=%d/%d bytes=%d/%d observed=%.2f MiB/s eta=%s", latest.Name, result.CompletedFiles, result.TotalFiles, result.CompletedBytes, result.TotalBytes, rateMiB, eta.Round(time.Minute))
}

func resultFromState(stateFile string, state *State) Result {
	result := Result{TotalFiles: len(state.Files), TotalBytes: state.TotalBytes, StateFile: stateFile}
	for i := range state.Files {
		if state.Files[i].Uploaded {
			result.CompletedFiles++
			result.CompletedBytes += state.Files[i].Size
		}
	}
	return result
}

func saveState(stateFile string, state *State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := stateFile + ".tmp"
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
	return os.Rename(tmp, stateFile)
}

func lockState(stateFile string) (*os.File, error) {
	lockPath := stateFile + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another upload process holds %s: %w", lockPath, err)
	}
	return lock, nil
}

func unlockState(lock *os.File) {
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
}

func removeIfPresent(filePath string) error {
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove verified file %s: %w", filePath, err)
	}
	return nil
}

func retryDelay(base time.Duration, attempt int) time.Duration {
	delay := base
	for i := 1; i < attempt && delay < 10*time.Minute; i++ {
		delay *= 2
	}
	if delay > 10*time.Minute {
		delay = 10 * time.Minute
	}
	return delay
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
