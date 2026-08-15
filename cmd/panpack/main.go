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

	"github.com/MayMistery/panpack/internal/backup"
	"github.com/MayMistery/panpack/internal/bytesize"
	"github.com/MayMistery/panpack/internal/credentials"
	"github.com/MayMistery/panpack/internal/resource"
	"github.com/MayMistery/panpack/internal/restore"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
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

func runBackup(ctx context.Context, args []string, stdout, stderr io.Writer, dryRun bool) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	source := fs.String("source", "", "source directory (required)")
	stateDir := fs.String("state-dir", "", "state directory (default: <source>/.panpack)")
	stagingDir := fs.String("staging-dir", "", "volume staging directory (default: <state-dir>/staging)")
	remoteDir := fs.String("remote-dir", "/apps/bypy/autodl-tmp-backup-panpack", "absolute Baidu Netdisk destination")
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
		fmt.Fprintf(stdout, "snapshot=%s entries=%d files=%d bytes=%d volume=%s concurrency=%d..%d reserve=%s\n",
			result.Snapshot.ID, result.Snapshot.EntryCount, result.Snapshot.FileCount, result.Snapshot.TotalFileBytes,
			bytesize.Format(result.Policy.VolumeBytes), result.Policy.InitialConcurrency, result.Policy.MaxConcurrency,
			bytesize.Format(result.Policy.ReserveBytes))
		return nil
	}
	fmt.Fprintf(stdout, "backup complete: snapshot=%s volumes=%d remote=%s\n", result.Snapshot.ID, len(result.State.Volumes), result.State.RemoteDir)
	return nil
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
  panpack restore --snapshot FILE --manifest FILE --volumes DIR --destination DIR
  panpack auth login|import-bypy|refresh [flags]
  panpack version`)
}
