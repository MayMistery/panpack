package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	packarchive "github.com/MayMistery/panpack/internal/archive"
	"github.com/MayMistery/panpack/internal/baidu"
	"github.com/MayMistery/panpack/internal/credentials"
	"github.com/MayMistery/panpack/internal/manifest"
	"github.com/MayMistery/panpack/internal/resource"
	"github.com/MayMistery/panpack/internal/state"
)

type Config struct {
	Source       string
	StateDir     string
	StagingDir   string
	RemoteDir    string
	TokenFile    string
	ExcludeNames []string
	Limits       resource.Limits
	DryRun       bool
	Logger       *log.Logger
}

type Result struct {
	Snapshot *manifest.Snapshot
	State    state.Backup
	Policy   resource.Policy
}

type VolumeIndex struct {
	FormatVersion int           `json:"format_version"`
	SnapshotID    string        `json:"snapshot_id"`
	CreatedAt     time.Time     `json:"created_at"`
	Volumes       []IndexedFile `json:"volumes"`
	Manifest      IndexedFile   `json:"manifest"`
	Snapshot      IndexedFile   `json:"snapshot"`
}

type IndexedFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	MD5  string `json:"md5"`
}

func Run(ctx context.Context, cfg Config) (*Result, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	resolved, err := resolveConfig(cfg)
	if err != nil {
		return nil, err
	}
	cfg = resolved

	snapshot, err := loadOrBuildSnapshot(ctx, cfg, logger)
	if err != nil {
		return nil, err
	}
	if snapshot.Source != cfg.Source {
		return nil, fmt.Errorf("snapshot source is %q, requested %q", snapshot.Source, cfg.Source)
	}
	resources, err := resource.Detect(cfg.StagingDir)
	if err != nil {
		return nil, err
	}
	policy, err := resource.Plan(resources, cfg.Limits)
	if err != nil {
		return nil, err
	}
	logger.Printf("policy: volume=%d bytes, upload concurrency=%d..%d, disk reserve=%d bytes", policy.VolumeBytes, policy.InitialConcurrency, policy.MaxConcurrency, policy.ReserveBytes)
	if cfg.DryRun {
		return &Result{Snapshot: snapshot, Policy: policy}, nil
	}

	store, err := state.Open(cfg.StateDir, snapshot.ID, cfg.Source, cfg.RemoteDir)
	if err != nil {
		return nil, err
	}
	creds, source, err := credentials.Discover(cfg.TokenFile)
	if err != nil {
		return nil, err
	}
	logger.Printf("credentials loaded from %s", source)
	uploader, err := baidu.New(creds.AccessToken, cfg.Limits.SliceSize, policy.InitialConcurrency, policy.MaxConcurrency, logger)
	if err != nil {
		return nil, err
	}
	if err := uploader.EnsureDir(ctx, cfg.RemoteDir); err != nil {
		return nil, err
	}

	if err := uploadPending(ctx, store, uploader, logger); err != nil {
		return nil, err
	}
	packer := &packarchive.Packer{
		Source:       cfg.Source,
		ManifestPath: filepath.Join(cfg.StateDir, snapshot.ManifestFile),
		ManifestSize: snapshot.ManifestSize,
		StagingDir:   cfg.StagingDir,
		SnapshotID:   snapshot.ID,
		BlockSize:    cfg.Limits.SliceSize,
	}

	for {
		current := store.Snapshot()
		if current.Cursor.Done {
			break
		}
		resources, err = resource.Detect(cfg.StagingDir)
		if err != nil {
			return nil, err
		}
		policy, err = resource.Plan(resources, cfg.Limits)
		if err != nil {
			return nil, fmt.Errorf("disk backpressure stopped packing: %w", err)
		}
		logger.Printf("packing volume %d with adaptive cap %d bytes", current.Cursor.NextVolume, policy.VolumeBytes)
		volume, err := packer.PackNext(ctx, current.Cursor, policy.VolumeBytes, cfg.RemoteDir)
		if errors.Is(err, io.EOF) {
			if err := store.MarkArchiveDone(); err != nil {
				return nil, err
			}
			break
		}
		if err != nil {
			return nil, err
		}
		if err := store.AddSealed(*volume); err != nil {
			return nil, err
		}
		logger.Printf("sealed %s (%d bytes, %d API blocks)", volume.Name, volume.Size, len(volume.BlockMD5s))
		if err := uploadOne(ctx, store, uploader, *volume, logger); err != nil {
			return nil, err
		}
	}

	if err := uploadMetadata(ctx, cfg, snapshot, store, uploader, logger); err != nil {
		return nil, err
	}
	if err := store.MarkComplete(); err != nil {
		return nil, err
	}
	final := store.Snapshot()
	return &Result{Snapshot: snapshot, State: final, Policy: policy}, nil
}

func resolveConfig(cfg Config) (Config, error) {
	source, err := filepath.Abs(cfg.Source)
	if err != nil {
		return cfg, err
	}
	cfg.Source = filepath.Clean(source)
	if cfg.StateDir == "" {
		cfg.StateDir = filepath.Join(cfg.Source, ".panpack")
	}
	stateDir, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return cfg, err
	}
	cfg.StateDir = filepath.Clean(stateDir)
	if cfg.StagingDir == "" {
		cfg.StagingDir = filepath.Join(cfg.StateDir, "staging")
	}
	staging, err := filepath.Abs(cfg.StagingDir)
	if err != nil {
		return cfg, err
	}
	cfg.StagingDir = filepath.Clean(staging)
	cfg.RemoteDir = strings.TrimRight(cfg.RemoteDir, "/")
	if !strings.HasPrefix(cfg.RemoteDir, "/apps/") {
		return cfg, fmt.Errorf("remote directory must be under /apps/<app>: %q", cfg.RemoteDir)
	}
	if cfg.Limits.MinVolume == 0 {
		cfg.Limits = resource.DefaultLimits()
	}
	if err := os.MkdirAll(cfg.StagingDir, 0o700); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func loadOrBuildSnapshot(ctx context.Context, cfg Config, logger *log.Logger) (*manifest.Snapshot, error) {
	snapshot, err := manifest.LoadSnapshot(cfg.StateDir)
	if err == nil {
		logger.Printf("resuming snapshot %s", snapshot.ID)
		return snapshot, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	excludes := make(map[string]struct{}, len(cfg.ExcludeNames))
	for _, name := range cfg.ExcludeNames {
		if name != "" {
			excludes[name] = struct{}{}
		}
	}
	logger.Printf("scanning source %s with bounded-memory directory batches", cfg.Source)
	snapshot, err = manifest.Build(ctx, manifest.ScanOptions{
		Source: cfg.Source, StateDir: cfg.StateDir, ExcludeNames: excludes, SkipPaths: []string{cfg.StagingDir},
	})
	if err != nil {
		return nil, err
	}
	logger.Printf("snapshot %s: %d files, %d bytes, %d entries", snapshot.ID, snapshot.FileCount, snapshot.TotalFileBytes, snapshot.EntryCount)
	return snapshot, nil
}

func uploadPending(ctx context.Context, store *state.Store, uploader *baidu.Client, logger *log.Logger) error {
	current := store.Snapshot()
	for _, volume := range current.Volumes {
		if volume.Uploaded {
			if _, err := os.Stat(volume.LocalPath); err == nil {
				if removeErr := os.Remove(volume.LocalPath); removeErr != nil {
					return fmt.Errorf("remove already-uploaded staging volume: %w", removeErr)
				}
			}
			continue
		}
		if len(volume.BlockMD5s) == 0 {
			return fmt.Errorf("pending volume %d has no block hashes", volume.Index)
		}
		logger.Printf("resuming sealed volume %s", volume.Name)
		if err := uploadOne(ctx, store, uploader, volume, logger); err != nil {
			return err
		}
	}
	return nil
}

func uploadOne(ctx context.Context, store *state.Store, uploader *baidu.Client, volume state.Volume, logger *log.Logger) error {
	if err := store.IncrementAttempt(volume.Index); err != nil {
		return err
	}
	logger.Printf("uploading %s with concurrency %d", volume.Name, uploader.CurrentConcurrency())
	stats, err := uploader.UploadVolume(ctx, volume)
	if err != nil {
		return fmt.Errorf("upload %s: %w", volume.Name, err)
	}
	logger.Printf("verified %s in %s (retries=%d, rate_limits=%d, rapid=%v)", volume.Name, stats.Duration.Round(time.Second), stats.Retries, stats.RateLimits, stats.Rapid)
	if err := store.MarkUploaded(volume.Index); err != nil {
		return err
	}
	if err := os.Remove(volume.LocalPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove uploaded staging volume %s: %w", volume.LocalPath, err)
	}
	return nil
}

func uploadMetadata(ctx context.Context, cfg Config, snapshot *manifest.Snapshot, store *state.Store, uploader *baidu.Client, logger *log.Logger) error {
	current := store.Snapshot()
	index := VolumeIndex{FormatVersion: 1, SnapshotID: snapshot.ID, CreatedAt: time.Now().UTC()}
	for _, volume := range current.Volumes {
		if !volume.Uploaded {
			return fmt.Errorf("volume %d is not uploaded", volume.Index)
		}
		index.Volumes = append(index.Volumes, IndexedFile{Name: volume.Name, Size: volume.Size, MD5: volume.MD5})
	}

	manifestPath := filepath.Join(cfg.StateDir, snapshot.CompressedManifest)
	snapshotPath := filepath.Join(cfg.StateDir, "snapshot.json")
	manifestMD5Size, manifestMD5, _, err := baidu.HashFile(manifestPath, cfg.Limits.SliceSize)
	if err != nil {
		return err
	}
	snapshotSize, snapshotMD5, _, err := baidu.HashFile(snapshotPath, cfg.Limits.SliceSize)
	if err != nil {
		return err
	}
	index.Manifest = IndexedFile{Name: snapshot.CompressedManifest, Size: manifestMD5Size, MD5: manifestMD5}
	index.Snapshot = IndexedFile{Name: "snapshot-" + snapshot.ID + ".json", Size: snapshotSize, MD5: snapshotMD5}

	indexName := "index-" + snapshot.ID + ".json"
	indexPath := filepath.Join(cfg.StateDir, indexName)
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := writeAtomic(indexPath, data); err != nil {
		return err
	}

	metadata := []struct{ local, remote string }{
		{manifestPath, cfg.RemoteDir + "/" + snapshot.CompressedManifest},
		{snapshotPath, cfg.RemoteDir + "/snapshot-" + snapshot.ID + ".json"},
		{indexPath, cfg.RemoteDir + "/" + indexName},
	}
	for _, file := range metadata {
		logger.Printf("uploading metadata %s", filepath.Base(file.local))
		if _, err := uploader.UploadFile(ctx, file.local, file.remote); err != nil {
			return fmt.Errorf("upload metadata %s: %w", filepath.Base(file.local), err)
		}
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
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
	return os.Rename(tmp, path)
}
