package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/MayMistery/panpack/internal/audit"
	"github.com/MayMistery/panpack/internal/backup"
	"github.com/MayMistery/panpack/internal/batchupload"
	"github.com/MayMistery/panpack/internal/bytesize"
	"github.com/MayMistery/panpack/internal/credentials"
	"github.com/MayMistery/panpack/internal/resource"
	"github.com/MayMistery/panpack/internal/restore"
	"github.com/MayMistery/panpack/internal/runreceipt"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stderr)
		return errors.New("a command is required")
	}
	switch args[0] {
	case "backup":
		return runBackup(ctx, args[1:], stdout, stderr, false)
	case "upload-batch":
		return runUploadBatch(ctx, args[1:], stdout, stderr)
	case "audit-batch":
		return runAuditBatch(ctx, args[1:], stdout, stderr)
	case "plan":
		return runBackup(ctx, args[1:], stdout, stderr, true)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "restore":
		return runRestore(ctx, args[1:], stdout, stderr)
	case "auth":
		return runAuth(ctx, args[1:], stdout, stderr)
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, version)
		return nil
	case "help", "--help", "-h":
		printUsage(stdout)
		return nil
	default:
		printUsage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runUploadBatch(ctx context.Context, args []string, stdout, stderr io.Writer) (retErr error) {
	fs := flag.NewFlagSet("upload-batch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sourceDir := fs.String("source-dir", "", "directory containing sealed files (required)")
	pattern := fs.String("pattern", "*.tar", "basename glob frozen into the first-run state")
	remoteDir := fs.String("remote-dir", "", "absolute Baidu Netdisk destination (required)")
	stateFile := fs.String("state-file", "", "atomic resume state (default: <source-dir>/.panpack-upload-state.json)")
	receiptFile := fs.String("receipt-file", "", "atomic run receipt (default: <state-file>.receipt.json; '-' disables)")
	tokenFile := fs.String("token-file", "", "credentials JSON; supports bypy.json")
	deleteAfterVerify := fs.Bool("delete-after-verify", false, "delete each local file only after remote size/MD5 verification")
	maxFileAttempts := fs.Int("max-file-attempts", 20, "file-level attempts before stopping")
	retryDelay := fs.Duration("retry-delay", time.Minute, "initial delay between file-level attempts")
	maxConcurrency := fs.Int("max-upload-concurrency", 16, "hard cap for adaptive upload concurrency")
	sliceSize := fs.String("slice-size", "4MiB", "Baidu API block size")
	minFree := fs.String("min-free", "4GiB", "minimum disk space to reserve")
	reserveFraction := fs.Float64("reserve-fraction", 0.05, "fraction of filesystem capacity to reserve")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sourceDir == "" || *remoteDir == "" {
		return errors.New("--source-dir and --remote-dir are required")
	}
	resolvedState, err := uploadStatePath(*sourceDir, *stateFile)
	if err != nil {
		return err
	}
	resolvedReceipt, err := receiptPath(*receiptFile, resolvedState+".receipt.json")
	if err != nil {
		return err
	}
	var recorder *runreceipt.Recorder
	if resolvedReceipt != "" {
		recorder, err = runreceipt.Start(resolvedReceipt, "upload-batch", resolvedState)
		if err != nil {
			return err
		}
		defer finishReceipt(recorder, &retErr)
	}
	limits := resource.DefaultLimits()
	if limits.SliceSize, err = bytesize.Parse(*sliceSize); err != nil {
		return fmt.Errorf("--slice-size: %w", err)
	}
	if limits.MinFree, err = bytesize.Parse(*minFree); err != nil {
		return fmt.Errorf("--min-free: %w", err)
	}
	limits.ReserveFraction = *reserveFraction
	limits.MaxUploadConcurrency = *maxConcurrency
	logger := log.New(stderr, "", log.LstdFlags|log.Lmicroseconds)
	result, err := batchupload.Run(ctx, batchupload.Config{
		SourceDir: *sourceDir, Pattern: *pattern, RemoteDir: *remoteDir,
		StateFile: resolvedState, ReceiptFile: resolvedReceipt, TokenFile: *tokenFile, DeleteAfterVerify: *deleteAfterVerify,
		MaxFileAttempts: *maxFileAttempts, RetryDelay: *retryDelay, Limits: limits, Logger: logger,
	})
	if err != nil {
		return err
	}
	_, retErr = fmt.Fprintf(stdout, "batch upload complete: files=%d/%d bytes=%s state=%s receipt=%s\n", result.CompletedFiles, result.TotalFiles, bytesize.Format(result.CompletedBytes), result.StateFile, displayReceipt(resolvedReceipt))
	return retErr
}

func runBackup(ctx context.Context, args []string, stdout, stderr io.Writer, dryRun bool) (retErr error) {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	source := fs.String("source", "", "source directory (required)")
	stateDir := fs.String("state-dir", "", "state directory (default: <source>/.panpack)")
	stagingDir := fs.String("staging-dir", "", "volume staging directory (default: <state-dir>/staging)")
	receiptFile := fs.String("receipt-file", "", "atomic run receipt (default: <state-dir>/backup-run.receipt.json; '-' disables)")
	remoteDir := fs.String("remote-dir", "", "absolute Baidu Netdisk destination (default: unique /apps/bypy/panpack-<snapshot-id>)")
	tokenFile := fs.String("token-file", "", "credentials JSON; supports bypy.json")
	volumeSize := fs.String("volume-size", "auto", "fixed volume size or auto")
	minVolume := fs.String("min-volume-size", "64MiB", "smallest adaptive volume")
	maxVolume := fs.String("max-volume-size", "2GiB", "largest adaptive volume")
	minFree := fs.String("min-free", "4GiB", "minimum disk space to reserve")
	reserveFraction := fs.Float64("reserve-fraction", 0.05, "fraction of filesystem capacity to reserve")
	maxConcurrency := fs.Int("max-upload-concurrency", 16, "hard cap for adaptive upload concurrency")
	sliceSize := fs.String("slice-size", "4MiB", "Baidu API block size")
	var excludes stringList
	fs.Var(&excludes, "exclude-name", "exclude an exact basename; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *source == "" {
		return errors.New("--source is required")
	}
	limits := resource.DefaultLimits()
	var err error
	resolvedReceipt := ""
	if !dryRun {
		statePath, pathErr := backupStatePath(*source, *stateDir)
		if pathErr != nil {
			return pathErr
		}
		resolvedReceipt, err = receiptPath(*receiptFile, filepath.Join(filepath.Dir(statePath), "backup-run.receipt.json"))
		if err != nil {
			return err
		}
		if resolvedReceipt != "" {
			recorder, startErr := runreceipt.Start(resolvedReceipt, "backup", statePath)
			if startErr != nil {
				return startErr
			}
			defer finishReceipt(recorder, &retErr)
		}
	}
	if limits.MinFree, err = bytesize.Parse(*minFree); err != nil {
		return fmt.Errorf("--min-free: %w", err)
	}
	if limits.MinVolume, err = bytesize.Parse(*minVolume); err != nil {
		return fmt.Errorf("--min-volume-size: %w", err)
	}
	if limits.MaxVolume, err = bytesize.Parse(*maxVolume); err != nil {
		return fmt.Errorf("--max-volume-size: %w", err)
	}
	if *volumeSize != "auto" {
		if limits.RequestedVolume, err = bytesize.Parse(*volumeSize); err != nil {
			return fmt.Errorf("--volume-size: %w", err)
		}
	}
	if limits.SliceSize, err = bytesize.Parse(*sliceSize); err != nil {
		return fmt.Errorf("--slice-size: %w", err)
	}
	limits.ReserveFraction = *reserveFraction
	limits.MaxUploadConcurrency = *maxConcurrency
	logger := log.New(stderr, "", log.LstdFlags|log.Lmicroseconds)
	result, err := backup.Run(ctx, backup.Config{
		Source: *source, StateDir: *stateDir, StagingDir: *stagingDir,
		RemoteDir: *remoteDir, TokenFile: *tokenFile, ExcludeNames: excludes,
		Limits: limits, DryRun: dryRun, Logger: logger,
	})
	if err != nil {
		return err
	}
	if dryRun {
		_, retErr = fmt.Fprintf(stdout, "snapshot=%s entries=%d files=%d bytes=%d volume=%s concurrency=%d..%d reserve=%s remote=%s\n",
			result.Snapshot.ID, result.Snapshot.EntryCount, result.Snapshot.FileCount, result.Snapshot.TotalFileBytes,
			bytesize.Format(result.Policy.VolumeBytes), result.Policy.InitialConcurrency, result.Policy.MaxConcurrency,
			bytesize.Format(result.Policy.ReserveBytes), result.RemoteDir)
		return retErr
	}
	_, retErr = fmt.Fprintf(stdout, "backup complete: snapshot=%s volumes=%d remote=%s receipt=%s\n", result.Snapshot.ID, len(result.State.Volumes), result.State.RemoteDir, displayReceipt(resolvedReceipt))
	return retErr
}

func runAuditBatch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("audit-batch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateFile := fs.String("state-file", "", "upload-batch state file (required)")
	tokenFile := fs.String("token-file", "", "credentials JSON; supports bypy.json")
	remotePattern := fs.String("remote-pattern", "", "remote basename glob (default: pattern frozen in state)")
	expectedList := fs.String("expected-list", "", "file containing the exact expected remote basenames")
	expectedTemplate := fs.String("expected-template", "", "fmt-style integer template for an exact name sequence")
	expectedStart := fs.Int("expected-start", 0, "first expected sequence index")
	expectedEnd := fs.Int("expected-end", -1, "last expected sequence index, inclusive")
	requireLocalEmpty := fs.Bool("require-local-empty", false, "fail if the source directory still has files matching the frozen pattern")
	requireChecksum := fs.Bool("require-checksum", false, "require an independently auditable checksum for every frozen file")
	receiptFile := fs.String("receipt-file", "", "successful upload receipt (default: <state-file>.receipt.json; '-' skips)")
	jsonOutput := fs.Bool("json", false, "emit JSON evidence")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *stateFile == "" {
		return errors.New("--state-file is required")
	}
	if *expectedList != "" && *expectedTemplate != "" {
		return errors.New("--expected-list and --expected-template are mutually exclusive")
	}
	if *expectedTemplate == "" && (*expectedStart != 0 || *expectedEnd >= 0) {
		return errors.New("--expected-start and --expected-end require --expected-template")
	}
	if *expectedTemplate != "" && *expectedEnd < *expectedStart {
		return errors.New("--expected-template requires --expected-end >= --expected-start")
	}
	var expected []string
	var err error
	if *expectedList != "" {
		expected, err = audit.LoadExpectedList(*expectedList)
		if err != nil {
			return fmt.Errorf("load expected list: %w", err)
		}
		if len(expected) == 0 {
			return errors.New("--expected-list contains no names")
		}
	} else if *expectedTemplate != "" {
		expected, err = audit.GenerateExpected(*expectedTemplate, *expectedStart, *expectedEnd)
		if err != nil {
			return err
		}
	}
	resolvedState, err := filepath.Abs(*stateFile)
	if err != nil {
		return err
	}
	resolvedReceipt, err := receiptPath(*receiptFile, resolvedState+".receipt.json")
	if err != nil {
		return err
	}
	result, auditErr := audit.Run(ctx, audit.Config{
		StateFile: resolvedState, TokenFile: *tokenFile, RemotePattern: *remotePattern,
		ExpectedNames: expected, RequireLocalEmpty: *requireLocalEmpty,
		RequireChecksum: *requireChecksum, ReceiptFile: resolvedReceipt,
		Logger: log.New(stderr, "", log.LstdFlags|log.Lmicroseconds),
	})
	if result != nil {
		if *jsonOutput {
			if err := json.NewEncoder(stdout).Encode(result); err != nil {
				return err
			}
		} else if err := printAuditResult(stdout, result); err != nil {
			return err
		}
	}
	return auditErr
}

func printAuditResult(w io.Writer, result *audit.Result) error {
	receipt := "skipped"
	if result.Receipt.Checked {
		receipt = string(result.Receipt.Status)
		if result.Receipt.ExitCode != nil {
			receipt += fmt.Sprintf("/exit-%d", *result.Receipt.ExitCode)
		}
	}
	_, err := fmt.Fprintf(w,
		"audit passed=%t state=%d/%d bytes=%d/%d remote=%d/%d missing=%d extra=%d duplicates=%d sizes=%d/%d checksums=%d/%d unavailable=%d local=%d receipt=%s\n",
		result.Passed, result.StateUploaded, result.StateFiles, result.VerifiedBytes, result.TotalBytes,
		result.RemoteUnique, result.ExpectedEntries, len(result.Missing), len(result.Extra), len(result.Duplicates),
		result.FrozenSizeMatched, result.StateFiles, result.FrozenChecksumMatched, result.StateFiles,
		len(result.FrozenChecksumUnavailable), len(result.LocalMatching), receipt,
	)
	return err
}

func uploadStatePath(sourceDir, stateFile string) (string, error) {
	if stateFile != "" {
		return filepath.Abs(stateFile)
	}
	source, err := filepath.Abs(sourceDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(source, ".panpack-upload-state.json"), nil
}

func backupStatePath(source, stateDir string) (string, error) {
	if stateDir == "" {
		absSource, err := filepath.Abs(source)
		if err != nil {
			return "", err
		}
		stateDir = filepath.Join(absSource, ".panpack")
	}
	absStateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(absStateDir, "backup-state.json"), nil
}

func receiptPath(requested, defaultPath string) (string, error) {
	if requested == "-" {
		return "", nil
	}
	if requested == "" {
		requested = defaultPath
	}
	return filepath.Abs(requested)
}

func finishReceipt(recorder *runreceipt.Recorder, runErr *error) {
	exitCode := 0
	if *runErr != nil {
		exitCode = 1
	}
	if err := recorder.Finish(exitCode, *runErr); err != nil {
		*runErr = errors.Join(*runErr, fmt.Errorf("persist run receipt: %w", err))
	}
}

func displayReceipt(receipt string) string {
	if receipt == "" {
		return "disabled"
	}
	return receipt
}

func runDoctor(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("path", ".", "filesystem path to inspect")
	jsonOutput := fs.Bool("json", false, "emit JSON")
	minFree := fs.String("min-free", "4GiB", "minimum free-space reserve")
	maxVolume := fs.String("max-volume-size", "2GiB", "maximum adaptive volume")
	maxConcurrency := fs.Int("max-upload-concurrency", 16, "upload concurrency ceiling")
	if err := fs.Parse(args); err != nil {
		return err
	}
	limits := resource.DefaultLimits()
	var err error
	if limits.MinFree, err = bytesize.Parse(*minFree); err != nil {
		return err
	}
	if limits.MaxVolume, err = bytesize.Parse(*maxVolume); err != nil {
		return err
	}
	limits.MaxUploadConcurrency = *maxConcurrency
	snapshot, err := resource.Detect(*path)
	if err != nil {
		return err
	}
	policy, planErr := resource.Plan(snapshot, limits)
	if *jsonOutput {
		payload := struct {
			Resources resource.Snapshot `json:"resources"`
			Policy    *resource.Policy  `json:"policy,omitempty"`
			Error     string            `json:"error,omitempty"`
		}{Resources: snapshot}
		if planErr != nil {
			payload.Error = planErr.Error()
		} else {
			payload.Policy = &policy
		}
		return json.NewEncoder(stdout).Encode(payload)
	}
	fmt.Fprintf(stdout, "disk: free %s / total %s\n", bytesize.Format(snapshot.DiskFree), bytesize.Format(snapshot.DiskTotal))
	fmt.Fprintf(stdout, "memory: available %s / limit %s\n", bytesize.Format(snapshot.MemoryAvailable), bytesize.Format(snapshot.MemoryLimit))
	fmt.Fprintf(stdout, "cpu quota: %.2f\n", snapshot.CPUQuota)
	if planErr != nil {
		return planErr
	}
	fmt.Fprintf(stdout, "selected: volume %s, concurrency %d..%d, reserve %s\n",
		bytesize.Format(policy.VolumeBytes), policy.InitialConcurrency, policy.MaxConcurrency, bytesize.Format(policy.ReserveBytes))
	return nil
}

func runRestore(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	snapshot := fs.String("snapshot", "", "snapshot JSON path (required)")
	manifestPath := fs.String("manifest", "", "manifest JSONL or JSONL.gz path (required)")
	volumes := fs.String("volumes", "", "directory containing volume tar files (required)")
	destination := fs.String("destination", "", "restore destination (required)")
	force := fs.Bool("force", false, "overwrite existing files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *snapshot == "" || *manifestPath == "" || *volumes == "" || *destination == "" {
		return errors.New("--snapshot, --manifest, --volumes, and --destination are required")
	}
	if err := restore.Run(ctx, restore.Options{SnapshotPath: *snapshot, ManifestPath: *manifestPath, VolumesDir: *volumes, Destination: *destination, Force: *force}); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "restore complete")
	return nil
}

func runAuth(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("auth requires login, import-bypy, or refresh")
	}
	switch args[0] {
	case "login":
		fs := flag.NewFlagSet("auth login", flag.ContinueOnError)
		fs.SetOutput(stderr)
		appKey := fs.String("app-key", os.Getenv("BAIDU_APP_KEY"), "Baidu app key (or BAIDU_APP_KEY)")
		secretKey := fs.String("secret-key", os.Getenv("BAIDU_SECRET_KEY"), "Baidu secret key (or BAIDU_SECRET_KEY)")
		output := fs.String("output", "", "credential output path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *output == "" {
			var err error
			*output, err = credentials.DefaultPath()
			if err != nil {
				return err
			}
		}
		creds, err := credentials.DeviceLogin(ctx, *appKey, *secretKey, stdout)
		if err != nil {
			return err
		}
		if err := credentials.Save(*output, creds); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "credentials saved to %s (mode 0600)\n", *output)
		return nil

	case "import-bypy":
		fs := flag.NewFlagSet("auth import-bypy", flag.ContinueOnError)
		fs.SetOutput(stderr)
		from := fs.String("from", "", "path to bypy.json (default: ~/.bypy/bypy.json)")
		output := fs.String("output", "", "credential output path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		home, _ := os.UserHomeDir()
		if *from == "" {
			*from = filepath.Join(home, ".bypy", "bypy.json")
		}
		if *output == "" {
			var err error
			*output, err = credentials.DefaultPath()
			if err != nil {
				return err
			}
		}
		creds, err := credentials.Load(*from)
		if err != nil {
			return err
		}
		if err := credentials.Save(*output, creds); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "credentials imported to %s; no token value was printed\n", *output)
		return nil

	case "refresh":
		fs := flag.NewFlagSet("auth refresh", flag.ContinueOnError)
		fs.SetOutput(stderr)
		file := fs.String("file", "", "panpack credentials path")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *file == "" {
			var err error
			*file, err = credentials.DefaultPath()
			if err != nil {
				return err
			}
		}
		creds, err := credentials.LoadUnchecked(*file)
		if err != nil {
			return err
		}
		creds, err = credentials.Refresh(ctx, creds)
		if err != nil {
			return err
		}
		if err := credentials.Save(*file, creds); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "credentials refreshed")
		return nil
	default:
		return fmt.Errorf("unknown auth command %q", args[0])
	}
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	if value == "" || strings.Contains(value, string(os.PathSeparator)) || strings.Contains(value, "/") {
		return fmt.Errorf("exclude-name must be a basename, got %q", value)
	}
	*s = append(*s, value)
	return nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `panpack - low-disk, resumable Baidu Netdisk backup

Usage:
  panpack doctor [flags]
  panpack plan --source DIR [backup flags]
  panpack backup --source DIR --remote-dir /apps/APP/BACKUP [flags]
  panpack upload-batch --source-dir DIR --pattern '*.tar' --remote-dir /apps/APP/BACKUP [flags]
  panpack audit-batch --state-file FILE [audit flags]
  panpack restore --snapshot FILE --manifest FILE --volumes DIR --destination DIR
  panpack auth login|import-bypy|refresh [flags]
  panpack version`)
}
