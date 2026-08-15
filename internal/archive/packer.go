package archive

import (
	"archive/tar"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MayMistery/panpack/internal/manifest"
	"github.com/MayMistery/panpack/internal/state"
)

const (
	FormatVersion  = 1
	endBlockBudget = int64(1024)
	headerBudget   = int64(4096)
)

type Packer struct {
	Source       string
	ManifestPath string
	ManifestSize int64
	StagingDir   string
	SnapshotID   string
	BlockSize    int64
}

type VolumeMarker struct {
	FormatVersion int    `json:"format_version"`
	SnapshotID    string `json:"snapshot_id"`
	VolumeIndex   int    `json:"volume_index"`
}

func (p *Packer) PackNext(ctx context.Context, cursor state.Cursor, targetBytes int64, remoteDir string) (*state.Volume, error) {
	if cursor.Done {
		return nil, io.EOF
	}
	if targetBytes < 64*1024 {
		return nil, fmt.Errorf("target volume size %d is too small", targetBytes)
	}
	if p.BlockSize <= 0 {
		return nil, fmt.Errorf("block size must be positive")
	}
	if err := os.MkdirAll(p.StagingDir, 0o700); err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}

	name := fmt.Sprintf("%s.volume-%06d.tar", p.SnapshotID, cursor.NextVolume)
	finalPath := filepath.Join(p.StagingDir, name)
	tmpPath := finalPath + ".tmp"
	if _, err := os.Stat(finalPath); err == nil {
		return nil, fmt.Errorf("sealed volume already exists but is absent from state: %s", finalPath)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale temporary volume: %w", err)
	}

	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create temporary volume: %w", err)
	}
	committed := false
	defer func() {
		_ = out.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	hw := newHashingWriter(out, p.BlockSize)
	tw := tar.NewWriter(hw)
	r, err := manifest.OpenReader(p.ManifestPath, cursor.ManifestOffset)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer r.Close()

	markerWritten := false
	sourceItems := 0
	writeMarker := func() error {
		if markerWritten {
			return nil
		}
		data, err := json.Marshal(VolumeMarker{FormatVersion: FormatVersion, SnapshotID: p.SnapshotID, VolumeIndex: cursor.NextVolume})
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:     ".panpack/volume.json",
			Mode:     0o600,
			Size:     int64(len(data)),
			ModTime:  time.Unix(0, 0).UTC(),
			Typeflag: tar.TypeReg,
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
		markerWritten = true
		return nil
	}

	nextCursor := cursor
packLoop:
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry, startOffset, nextOffset, readErr := r.Next()
		if errors.Is(readErr, io.EOF) {
			nextCursor.Done = true
			break packLoop
		}
		if readErr != nil {
			return nil, readErr
		}
		if startOffset != nextCursor.ManifestOffset {
			return nil, fmt.Errorf("manifest cursor drift: reader=%d state=%d", startOffset, nextCursor.ManifestOffset)
		}

		absPath, err := safeSourcePath(p.Source, entry.Path)
		if err != nil {
			return nil, err
		}
		if err := verifyEntry(absPath, entry); err != nil {
			return nil, err
		}

		switch entry.Kind {
		case manifest.KindFile:
			remaining := entry.Size - nextCursor.FileOffset
			if remaining < 0 {
				return nil, fmt.Errorf("invalid file cursor for %s: %d > %d", entry.Path, nextCursor.FileOffset, entry.Size)
			}
			available := targetBytes - hw.Size() - endBlockBudget
			fullEstimate := tarEntryBudget(entry.Path, remaining)
			if nextCursor.FileOffset == 0 && fullEstimate <= available {
				if err := writeMarker(); err != nil {
					return nil, err
				}
				if err := writeWholeFile(tw, absPath, entry); err != nil {
					return nil, err
				}
				sourceItems++
				nextCursor.ManifestOffset = nextOffset
				nextCursor.FileOffset = 0
				continue packLoop
			}

			// If this file would fit in a fresh volume, seal the current one first.
			if nextCursor.FileOffset == 0 && sourceItems > 0 && fullEstimate <= targetBytes-endBlockBudget-headerBudget {
				break packLoop
			}
			if err := writeMarker(); err != nil {
				return nil, err
			}
			available = targetBytes - hw.Size() - endBlockBudget - headerBudget
			if available < 512 {
				if sourceItems > 0 {
					break packLoop
				}
				return nil, fmt.Errorf("volume target %d leaves no room for file data", targetBytes)
			}
			partSize := remaining
			if partSize > available {
				partSize = available
			}
			// Keep room for tar padding. Non-final fragments are 512-byte aligned.
			if partSize < remaining {
				partSize -= partSize % 512
			}
			if partSize <= 0 {
				return nil, fmt.Errorf("unable to fit fragment for %s", entry.Path)
			}
			if err := writeFragment(tw, absPath, entry, nextCursor.FileOffset, partSize); err != nil {
				return nil, err
			}
			sourceItems++
			nextCursor.FileOffset += partSize
			if nextCursor.FileOffset == entry.Size {
				nextCursor.ManifestOffset = nextOffset
				nextCursor.FileOffset = 0
			}
			break packLoop // one fragment closes the volume, keeping the cap deterministic

		case manifest.KindDir, manifest.KindSymlink:
			if sourceItems > 0 && tarEntryBudget(entry.Path, 0) > targetBytes-hw.Size()-endBlockBudget {
				break packLoop
			}
			if err := writeMarker(); err != nil {
				return nil, err
			}
			if err := writeMetadataEntry(tw, entry); err != nil {
				return nil, err
			}
			sourceItems++
			nextCursor.ManifestOffset = nextOffset
			nextCursor.FileOffset = 0
		default:
			return nil, fmt.Errorf("unsupported manifest entry kind %q", entry.Kind)
		}
	}

	if sourceItems == 0 {
		_ = tw.Close()
		return nil, io.EOF
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close tar volume: %w", err)
	}
	if err := out.Sync(); err != nil {
		return nil, fmt.Errorf("sync tar volume: %w", err)
	}
	if err := out.Close(); err != nil {
		return nil, fmt.Errorf("close tar volume file: %w", err)
	}
	if hw.Size() > targetBytes {
		return nil, fmt.Errorf("internal error: volume %s is %d bytes, above target %d", name, hw.Size(), targetBytes)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return nil, fmt.Errorf("seal tar volume: %w", err)
	}
	committed = true
	nextCursor.NextVolume++
	if nextCursor.FileOffset == 0 && nextCursor.ManifestOffset >= p.ManifestSize {
		nextCursor.Done = true
	}

	fullMD5, blocks := hw.Sums()
	return &state.Volume{
		Index:       cursor.NextVolume,
		Name:        name,
		LocalPath:   finalPath,
		RemotePath:  strings.TrimRight(remoteDir, "/") + "/" + name,
		Size:        hw.Size(),
		MD5:         fullMD5,
		BlockMD5s:   blocks,
		CursorAfter: nextCursor,
		SealedAt:    time.Now().UTC(),
	}, nil
}

func writeWholeFile(tw *tar.Writer, absPath string, entry manifest.Entry) error {
	hdr := headerFor(entry, entry.Path, entry.Size, tar.TypeReg)
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write header %s: %w", entry.Path, err)
	}
	f, err := os.Open(absPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", entry.Path, err)
	}
	defer f.Close()
	if _, err := io.CopyN(tw, f, entry.Size); err != nil {
		return fmt.Errorf("archive %s: %w", entry.Path, err)
	}
	return nil
}

func writeFragment(tw *tar.Writer, absPath string, entry manifest.Entry, offset, size int64) error {
	name := fmt.Sprintf(".panpack/fragments/%020d/%020d.part", entry.ID, offset)
	hdr := headerFor(entry, name, size, tar.TypeReg)
	hdr.PAXRecords = map[string]string{
		"PANPACK.entry_id":      fmt.Sprintf("%d", entry.ID),
		"PANPACK.file_offset":   fmt.Sprintf("%d", offset),
		"PANPACK.original_path": entry.Path,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write fragment header %s: %w", entry.Path, err)
	}
	f, err := os.Open(absPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", entry.Path, err)
	}
	defer f.Close()
	section := io.NewSectionReader(f, offset, size)
	if _, err := io.CopyN(tw, section, size); err != nil {
		return fmt.Errorf("archive fragment %s@%d: %w", entry.Path, offset, err)
	}
	return nil
}

func writeMetadataEntry(tw *tar.Writer, entry manifest.Entry) error {
	typeFlag := byte(tar.TypeDir)
	name := entry.Path
	if entry.Kind == manifest.KindDir {
		name = strings.TrimRight(name, "/") + "/"
	} else {
		typeFlag = tar.TypeSymlink
	}
	hdr := headerFor(entry, name, 0, typeFlag)
	if entry.Kind == manifest.KindSymlink {
		hdr.Linkname = entry.LinkTarget
	}
	return tw.WriteHeader(hdr)
}

func headerFor(entry manifest.Entry, name string, size int64, typeFlag byte) *tar.Header {
	return &tar.Header{
		Name:       name,
		Linkname:   entry.LinkTarget,
		Size:       size,
		Mode:       int64(os.FileMode(entry.Mode).Perm()),
		ModTime:    time.Unix(0, entry.ModTimeUnixNano).UTC(),
		AccessTime: time.Unix(0, 0).UTC(),
		ChangeTime: time.Unix(0, 0).UTC(),
		Typeflag:   typeFlag,
		Format:     tar.FormatPAX,
	}
}

func verifyEntry(absPath string, entry manifest.Entry) error {
	fi, err := os.Lstat(absPath)
	if err != nil {
		return fmt.Errorf("source changed after scan; lstat %s: %w", entry.Path, err)
	}
	if uint32(fi.Mode()) != entry.Mode || fi.ModTime().UnixNano() != entry.ModTimeUnixNano {
		return fmt.Errorf("source changed after scan: %s (mode or mtime differs)", entry.Path)
	}
	if entry.Kind == manifest.KindFile && fi.Size() != entry.Size {
		return fmt.Errorf("source changed after scan: %s (size %d, expected %d)", entry.Path, fi.Size(), entry.Size)
	}
	if entry.Kind == manifest.KindSymlink {
		target, err := os.Readlink(absPath)
		if err != nil || target != entry.LinkTarget {
			return fmt.Errorf("source changed after scan: symlink %s", entry.Path)
		}
	}
	return nil
}

func safeSourcePath(source, slashPath string) (string, error) {
	if slashPath == "" || strings.HasPrefix(slashPath, "/") {
		return "", fmt.Errorf("unsafe manifest path %q", slashPath)
	}
	rel := filepath.Clean(filepath.FromSlash(slashPath))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe manifest path %q", slashPath)
	}
	return filepath.Join(source, rel), nil
}

func tarEntryBudget(name string, size int64) int64 {
	pathOverhead := int64(len(name)*2 + 512)
	pathOverhead = round512(pathOverhead)
	return headerBudget + pathOverhead + round512(size)
}

func round512(n int64) int64 {
	return (n + 511) &^ 511
}

type hashingWriter struct {
	w          io.Writer
	full       hash.Hash
	block      hash.Hash
	blockSize  int64
	blockBytes int64
	blocks     []string
	size       int64
	finalized  bool
}

func newHashingWriter(w io.Writer, blockSize int64) *hashingWriter {
	return &hashingWriter{w: w, full: md5.New(), block: md5.New(), blockSize: blockSize}
}

func (w *hashingWriter) Write(p []byte) (int, error) {
	if w.finalized {
		return 0, errors.New("write after hash finalization")
	}
	total := 0
	for len(p) > 0 {
		remaining := w.blockSize - w.blockBytes
		n := int64(len(p))
		if n > remaining {
			n = remaining
		}
		chunk := p[:int(n)]
		written, err := w.w.Write(chunk)
		if written > 0 {
			_, _ = w.full.Write(chunk[:written])
			_, _ = w.block.Write(chunk[:written])
			w.blockBytes += int64(written)
			w.size += int64(written)
			total += written
			p = p[written:]
		}
		if w.blockBytes == w.blockSize {
			w.blocks = append(w.blocks, hex.EncodeToString(w.block.Sum(nil)))
			w.block.Reset()
			w.blockBytes = 0
		}
		if err != nil {
			return total, err
		}
		if written == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func (w *hashingWriter) Size() int64 { return w.size }

func (w *hashingWriter) Sums() (string, []string) {
	if !w.finalized {
		if w.blockBytes > 0 {
			w.blocks = append(w.blocks, hex.EncodeToString(w.block.Sum(nil)))
		}
		w.finalized = true
	}
	return hex.EncodeToString(w.full.Sum(nil)), append([]string(nil), w.blocks...)
}
