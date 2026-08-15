package manifest

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const FormatVersion = 1

type Kind string

const (
	KindFile    Kind = "file"
	KindDir     Kind = "dir"
	KindSymlink Kind = "symlink"
)

type Entry struct {
	ID              uint64 `json:"id"`
	Path            string `json:"path"`
	Kind            Kind   `json:"kind"`
	Size            int64  `json:"size,omitempty"`
	Mode            uint32 `json:"mode"`
	ModTimeUnixNano int64  `json:"mtime_unix_nano"`
	LinkTarget      string `json:"link_target,omitempty"`
}

type Snapshot struct {
	FormatVersion       int       `json:"format_version"`
	ID                  string    `json:"id"`
	Source              string    `json:"source"`
	CreatedAt           time.Time `json:"created_at"`
	EntryCount          uint64    `json:"entry_count"`
	FileCount           uint64    `json:"file_count"`
	TotalFileBytes      int64     `json:"total_file_bytes"`
	ManifestFile        string    `json:"manifest_file"`
	ManifestSize        int64     `json:"manifest_size"`
	ManifestSHA256      string    `json:"manifest_sha256"`
	CompressedManifest  string    `json:"compressed_manifest"`
	CompressedSize      int64     `json:"compressed_size"`
	CompressedSHA256    string    `json:"compressed_sha256"`
	SkippedSpecialFiles uint64    `json:"skipped_special_files"`
}

type ScanOptions struct {
	Source       string
	StateDir     string
	ExcludeNames map[string]struct{}
	SkipPaths    []string
}

func Build(ctx context.Context, opts ScanOptions) (*Snapshot, error) {
	source, err := filepath.Abs(opts.Source)
	if err != nil {
		return nil, fmt.Errorf("absolute source path: %w", err)
	}
	info, err := os.Stat(source)
	if err != nil {
		return nil, fmt.Errorf("stat source: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("source is not a directory: %s", source)
	}
	stateDir, err := filepath.Abs(opts.StateDir)
	if err != nil {
		return nil, fmt.Errorf("absolute state path: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}

	id, err := newSnapshotID()
	if err != nil {
		return nil, err
	}
	manifestName := "manifest-" + id + ".jsonl"
	gzipName := manifestName + ".gz"
	manifestTmp := filepath.Join(stateDir, manifestName+".tmp")
	gzipTmp := filepath.Join(stateDir, gzipName+".tmp")
	manifestFinal := filepath.Join(stateDir, manifestName)
	gzipFinal := filepath.Join(stateDir, gzipName)

	mf, err := os.OpenFile(manifestTmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create manifest: %w", err)
	}
	cleanup := func() {
		_ = mf.Close()
		_ = os.Remove(manifestTmp)
		_ = os.Remove(gzipTmp)
	}
	defer cleanup()

	gzf, err := os.OpenFile(gzipTmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create compressed manifest: %w", err)
	}
	defer gzf.Close()

	manifestHash := sha256.New()
	compressedHash := sha256.New()
	plainWriter := bufio.NewWriterSize(io.MultiWriter(mf, manifestHash), 1<<20)
	compressedCounter := &countingWriter{w: io.MultiWriter(gzf, compressedHash)}
	gzw, err := gzip.NewWriterLevel(compressedCounter, gzip.BestSpeed)
	if err != nil {
		return nil, fmt.Errorf("create gzip writer: %w", err)
	}

	snapshot := &Snapshot{
		FormatVersion:      FormatVersion,
		ID:                 id,
		Source:             source,
		CreatedAt:          time.Now().UTC(),
		ManifestFile:       manifestName,
		CompressedManifest: gzipName,
	}

	skipPaths := make([]string, 0, len(opts.SkipPaths)+1)
	skipPaths = append(skipPaths, filepath.Clean(stateDir))
	for _, p := range opts.SkipPaths {
		abs, absErr := filepath.Abs(p)
		if absErr != nil {
			return nil, fmt.Errorf("absolute skip path %q: %w", p, absErr)
		}
		skipPaths = append(skipPaths, filepath.Clean(abs))
	}

	var nextID uint64
	writeEntry := func(entry Entry) error {
		data, marshalErr := json.Marshal(entry)
		if marshalErr != nil {
			return marshalErr
		}
		data = append(data, '\n')
		if _, writeErr := plainWriter.Write(data); writeErr != nil {
			return writeErr
		}
		if _, writeErr := gzw.Write(data); writeErr != nil {
			return writeErr
		}
		snapshot.EntryCount++
		if entry.Kind == KindFile {
			snapshot.FileCount++
			snapshot.TotalFileBytes += entry.Size
		}
		return nil
	}

	var walk func(string, string) error
	walk = func(absDir, relDir string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir, openErr := os.Open(absDir)
		if openErr != nil {
			return fmt.Errorf("open directory %q: %w", absDir, openErr)
		}
		defer dir.Close()

		for {
			batch, readErr := dir.ReadDir(1024)
			for _, de := range batch {
				if err := ctx.Err(); err != nil {
					return err
				}
				absPath := filepath.Join(absDir, de.Name())
				if shouldSkip(absPath, de.Name(), opts.ExcludeNames, skipPaths) {
					continue
				}
				relPath := filepath.Join(relDir, de.Name())
				if !utf8.ValidString(relPath) {
					return fmt.Errorf("non-UTF-8 path is not supported: %q", []byte(relPath))
				}
				fi, statErr := os.Lstat(absPath)
				if statErr != nil {
					return fmt.Errorf("lstat %q: %w", absPath, statErr)
				}
				entry := Entry{
					ID:              nextID,
					Path:            filepath.ToSlash(relPath),
					Mode:            uint32(fi.Mode()),
					ModTimeUnixNano: fi.ModTime().UnixNano(),
				}
				nextID++

				switch {
				case fi.Mode().IsRegular():
					entry.Kind = KindFile
					entry.Size = fi.Size()
				case fi.IsDir():
					entry.Kind = KindDir
				case fi.Mode()&os.ModeSymlink != 0:
					entry.Kind = KindSymlink
					target, linkErr := os.Readlink(absPath)
					if linkErr != nil {
						return fmt.Errorf("read symlink %q: %w", absPath, linkErr)
					}
					if !utf8.ValidString(target) {
						return fmt.Errorf("non-UTF-8 symlink target is not supported: %q", []byte(target))
					}
					entry.LinkTarget = target
				default:
					snapshot.SkippedSpecialFiles++
					continue
				}
				if err := writeEntry(entry); err != nil {
					return fmt.Errorf("write manifest entry %q: %w", entry.Path, err)
				}
				if entry.Kind == KindDir {
					if err := walk(absPath, relPath); err != nil {
						return err
					}
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				return fmt.Errorf("read directory %q: %w", absDir, readErr)
			}
		}
		return nil
	}

	if err := walk(source, ""); err != nil {
		return nil, err
	}
	if err := plainWriter.Flush(); err != nil {
		return nil, fmt.Errorf("flush manifest: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return nil, fmt.Errorf("close compressed manifest: %w", err)
	}
	if err := mf.Sync(); err != nil {
		return nil, fmt.Errorf("sync manifest: %w", err)
	}
	if err := gzf.Sync(); err != nil {
		return nil, fmt.Errorf("sync compressed manifest: %w", err)
	}
	if err := mf.Close(); err != nil {
		return nil, fmt.Errorf("close manifest: %w", err)
	}
	if err := gzf.Close(); err != nil {
		return nil, fmt.Errorf("close compressed manifest file: %w", err)
	}

	manifestInfo, err := os.Stat(manifestTmp)
	if err != nil {
		return nil, err
	}
	snapshot.ManifestSize = manifestInfo.Size()
	snapshot.ManifestSHA256 = hex.EncodeToString(manifestHash.Sum(nil))
	snapshot.CompressedSize = compressedCounter.n
	snapshot.CompressedSHA256 = hex.EncodeToString(compressedHash.Sum(nil))

	if err := os.Rename(manifestTmp, manifestFinal); err != nil {
		return nil, fmt.Errorf("commit manifest: %w", err)
	}
	if err := os.Rename(gzipTmp, gzipFinal); err != nil {
		return nil, fmt.Errorf("commit compressed manifest: %w", err)
	}
	if err := WriteSnapshot(stateDir, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func LoadSnapshot(stateDir string) (*Snapshot, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, "snapshot.json"))
	if err != nil {
		return nil, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	if snapshot.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("unsupported snapshot format %d", snapshot.FormatVersion)
	}
	return &snapshot, nil
}

func WriteSnapshot(stateDir string, snapshot *Snapshot) error {
	return writeJSONAtomic(filepath.Join(stateDir, "snapshot.json"), snapshot, 0o600)
}

type Reader struct {
	file   *os.File
	extra  io.Closer
	reader *bufio.Reader
	offset int64
}

func OpenReader(path string, offset int64) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	var source io.Reader = f
	var extra io.Closer
	if strings.HasSuffix(strings.ToLower(path), ".gz") {
		if offset != 0 {
			f.Close()
			return nil, errors.New("compressed manifests only support sequential reads from offset zero")
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		source = gz
		extra = gz
	} else if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return &Reader{file: f, extra: extra, reader: bufio.NewReaderSize(source, 1<<20), offset: offset}, nil
}

func (r *Reader) Next() (entry Entry, start, next int64, err error) {
	start = r.offset
	line, readErr := r.reader.ReadBytes('\n')
	r.offset += int64(len(line))
	if len(line) == 0 && readErr != nil {
		return Entry{}, start, r.offset, readErr
	}
	if err := json.Unmarshal(bytesTrimSpace(line), &entry); err != nil {
		return Entry{}, start, r.offset, fmt.Errorf("decode manifest at offset %d: %w", start, err)
	}
	if errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	return entry, start, r.offset, readErr
}

func (r *Reader) Close() error {
	if r.extra != nil {
		_ = r.extra.Close()
	}
	return r.file.Close()
}

func shouldSkip(absPath, name string, excludes map[string]struct{}, skipPaths []string) bool {
	if _, ok := excludes[name]; ok {
		return true
	}
	clean := filepath.Clean(absPath)
	for _, skip := range skipPaths {
		if clean == skip || strings.HasPrefix(clean, skip+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func newSnapshotID() (string, error) {
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate snapshot id: %w", err)
	}
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(random), nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	tmp := path + ".tmp"
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
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

func bytesTrimSpace(p []byte) []byte {
	return []byte(strings.TrimSpace(string(p)))
}
