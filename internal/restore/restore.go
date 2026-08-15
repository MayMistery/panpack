package restore

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	packarchive "github.com/MayMistery/panpack/internal/archive"
	"github.com/MayMistery/panpack/internal/manifest"
)

type Options struct {
	SnapshotPath string
	ManifestPath string
	VolumesDir   string
	Destination  string
	Force        bool
}

type dirMetadata struct {
	path    string
	mode    os.FileMode
	modTime time.Time
}

func Run(ctx context.Context, opts Options) error {
	snapshotData, err := os.ReadFile(opts.SnapshotPath)
	if err != nil {
		return fmt.Errorf("read snapshot: %w", err)
	}
	var snapshot manifest.Snapshot
	if err := json.Unmarshal(snapshotData, &snapshot); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}
	if snapshot.FormatVersion != manifest.FormatVersion {
		return fmt.Errorf("unsupported snapshot format %d", snapshot.FormatVersion)
	}
	if err := os.MkdirAll(opts.Destination, 0o700); err != nil {
		return fmt.Errorf("create destination: %w", err)
	}

	pattern := filepath.Join(opts.VolumesDir, snapshot.ID+".volume-*.tar")
	volumes, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	sort.Strings(volumes)
	if len(volumes) == 0 {
		return fmt.Errorf("no volumes match %s", pattern)
	}

	lookup, err := newEntryLookup(opts.ManifestPath)
	if err != nil {
		return err
	}
	defer lookup.Close()

	var dirs []dirMetadata
	var activeFragment *fragmentProgress
	for index, volumePath := range volumes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := extractVolume(ctx, volumePath, index, snapshot.ID, opts, lookup, &dirs, &activeFragment); err != nil {
			return err
		}
	}
	if activeFragment != nil && activeFragment.written != activeFragment.entry.Size {
		return fmt.Errorf("fragmented file %s is incomplete: %d/%d bytes", activeFragment.entry.Path, activeFragment.written, activeFragment.entry.Size)
	}

	// Directory mtimes are applied after children, in reverse order.
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Chmod(dirs[i].path, dirs[i].mode.Perm())
		_ = os.Chtimes(dirs[i].path, dirs[i].modTime, dirs[i].modTime)
	}
	return nil
}

func extractVolume(ctx context.Context, volumePath string, expectedIndex int, snapshotID string, opts Options, lookup *entryLookup, dirs *[]dirMetadata, activeFragment **fragmentProgress) error {
	f, err := os.Open(volumePath)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	markerSeen := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", filepath.Base(volumePath), err)
		}
		if hdr.Name == ".panpack/volume.json" {
			var marker packarchive.VolumeMarker
			if err := json.NewDecoder(io.LimitReader(tr, hdr.Size)).Decode(&marker); err != nil {
				return fmt.Errorf("decode volume marker: %w", err)
			}
			if marker.FormatVersion != packarchive.FormatVersion || marker.SnapshotID != snapshotID || marker.VolumeIndex != expectedIndex {
				return fmt.Errorf("unexpected volume marker in %s: %+v", filepath.Base(volumePath), marker)
			}
			markerSeen = true
			continue
		}
		if strings.HasPrefix(hdr.Name, ".panpack/fragments/") {
			if err := restoreFragment(tr, hdr, opts, lookup, activeFragment); err != nil {
				return fmt.Errorf("restore fragment from %s: %w", filepath.Base(volumePath), err)
			}
			continue
		}
		if err := restoreEntry(tr, hdr, opts, dirs); err != nil {
			return fmt.Errorf("restore %s from %s: %w", hdr.Name, filepath.Base(volumePath), err)
		}
	}
	if !markerSeen {
		return fmt.Errorf("volume %s has no panpack marker", filepath.Base(volumePath))
	}
	return nil
}

func restoreEntry(tr *tar.Reader, hdr *tar.Header, opts Options, dirs *[]dirMetadata) error {
	dst, err := safeDestination(opts.Destination, hdr.Name)
	if err != nil {
		return err
	}
	if err := ensureNoSymlinkParents(opts.Destination, dst); err != nil {
		return err
	}
	modTime := hdr.ModTime
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(dst, os.FileMode(hdr.Mode).Perm()); err != nil {
			return err
		}
		*dirs = append(*dirs, dirMetadata{path: dst, mode: os.FileMode(hdr.Mode), modTime: modTime})
		return nil
	case tar.TypeSymlink:
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if _, err := os.Lstat(dst); err == nil {
			if !opts.Force {
				return fmt.Errorf("destination exists: %s", dst)
			}
			if err := os.Remove(dst); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		return os.Symlink(hdr.Linkname, dst)
	case tar.TypeReg, tar.TypeRegA:
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		if !opts.Force {
			flags |= os.O_EXCL
		}
		out, err := os.OpenFile(dst, flags, os.FileMode(hdr.Mode).Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(out, tr, hdr.Size)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if err := os.Chmod(dst, os.FileMode(hdr.Mode).Perm()); err != nil {
			return err
		}
		return os.Chtimes(dst, modTime, modTime)
	default:
		return fmt.Errorf("unsupported tar entry type %d", hdr.Typeflag)
	}
}

type fragmentProgress struct {
	entry   manifest.Entry
	written int64
}

func restoreFragment(tr *tar.Reader, hdr *tar.Header, opts Options, lookup *entryLookup, active **fragmentProgress) error {
	idText := hdr.PAXRecords["PANPACK.entry_id"]
	offsetText := hdr.PAXRecords["PANPACK.file_offset"]
	if idText == "" || offsetText == "" {
		return errors.New("fragment lacks PANPACK metadata")
	}
	id, err := strconv.ParseUint(idText, 10, 64)
	if err != nil {
		return err
	}
	offset, err := strconv.ParseInt(offsetText, 10, 64)
	if err != nil {
		return err
	}
	entry, err := lookup.Get(id)
	if err != nil {
		return err
	}
	if entry.Kind != manifest.KindFile || offset < 0 || offset+hdr.Size > entry.Size {
		return fmt.Errorf("invalid fragment range for manifest entry %d", id)
	}
	if *active == nil || (*active).entry.ID != id {
		if *active != nil && (*active).written != (*active).entry.Size {
			return fmt.Errorf("fragmented file %s ended early", (*active).entry.Path)
		}
		*active = &fragmentProgress{entry: entry}
	}
	if offset != (*active).written {
		return fmt.Errorf("fragment offset %d, expected %d", offset, (*active).written)
	}

	dst, err := safeDestination(opts.Destination, entry.Path)
	if err != nil {
		return err
	}
	if err := ensureNoSymlinkParents(opts.Destination, dst); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	flags := os.O_CREATE | os.O_WRONLY
	if offset == 0 {
		flags |= os.O_TRUNC
		if !opts.Force {
			flags |= os.O_EXCL
		}
	}
	out, err := os.OpenFile(dst, flags, os.FileMode(entry.Mode).Perm())
	if err != nil {
		return err
	}
	if _, err := out.Seek(offset, io.SeekStart); err != nil {
		out.Close()
		return err
	}
	_, copyErr := io.CopyN(out, tr, hdr.Size)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	(*active).written += hdr.Size
	if (*active).written == entry.Size {
		_ = os.Chmod(dst, os.FileMode(entry.Mode).Perm())
		modTime := time.Unix(0, entry.ModTimeUnixNano)
		_ = os.Chtimes(dst, modTime, modTime)
	}
	return nil
}

type entryLookup struct {
	reader  *manifest.Reader
	current manifest.Entry
	have    bool
}

func newEntryLookup(path string) (*entryLookup, error) {
	r, err := manifest.OpenReader(path, 0)
	if err != nil {
		return nil, err
	}
	return &entryLookup{reader: r}, nil
}

func (l *entryLookup) Get(id uint64) (manifest.Entry, error) {
	if l.have && l.current.ID == id {
		return l.current, nil
	}
	if l.have && l.current.ID > id {
		return manifest.Entry{}, fmt.Errorf("fragment entry id moved backward: %d", id)
	}
	for {
		entry, _, _, err := l.reader.Next()
		if err != nil {
			return manifest.Entry{}, err
		}
		l.current = entry
		l.have = true
		if entry.ID == id {
			return entry, nil
		}
		if entry.ID > id {
			return manifest.Entry{}, fmt.Errorf("manifest entry %d not found", id)
		}
	}
}

func (l *entryLookup) Close() error { return l.reader.Close() }

func safeDestination(root, slashPath string) (string, error) {
	if slashPath == "" || strings.HasPrefix(slashPath, "/") {
		return "", fmt.Errorf("unsafe archive path %q", slashPath)
	}
	rel := filepath.Clean(filepath.FromSlash(slashPath))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe archive path %q", slashPath)
	}
	return filepath.Join(root, rel), nil
}

func ensureNoSymlinkParents(root, destination string) error {
	rel, err := filepath.Rel(root, destination)
	if err != nil {
		return err
	}
	current := root
	parts := strings.Split(filepath.Dir(rel), string(os.PathSeparator))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		fi, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write through symlink parent: %s", current)
		}
	}
	return nil
}
