package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const FormatVersion = 1

type Cursor struct {
	ManifestOffset int64 `json:"manifest_offset"`
	FileOffset     int64 `json:"file_offset"`
	NextVolume     int   `json:"next_volume"`
	Done           bool  `json:"done"`
}

type Volume struct {
	Index          int       `json:"index"`
	Name           string    `json:"name"`
	LocalPath      string    `json:"local_path"`
	RemotePath     string    `json:"remote_path"`
	Size           int64     `json:"size"`
	MD5            string    `json:"md5"`
	BlockMD5s      []string  `json:"block_md5s,omitempty"`
	CursorAfter    Cursor    `json:"cursor_after"`
	SealedAt       time.Time `json:"sealed_at"`
	UploadedAt     time.Time `json:"uploaded_at,omitempty"`
	Uploaded       bool      `json:"uploaded"`
	UploadAttempts int       `json:"upload_attempts"`
}

type Backup struct {
	FormatVersion int       `json:"format_version"`
	SnapshotID    string    `json:"snapshot_id"`
	Source        string    `json:"source"`
	RemoteDir     string    `json:"remote_dir"`
	Cursor        Cursor    `json:"cursor"`
	Volumes       []Volume  `json:"volumes"`
	Completed     bool      `json:"completed"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Store struct {
	path string
	mu   sync.Mutex
	data Backup
}

func Open(stateDir, snapshotID, source, remoteDir string) (*Store, error) {
	path := filepath.Join(stateDir, "backup-state.json")
	store := &Store{path: path}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &store.data); err != nil {
			return nil, fmt.Errorf("decode backup state: %w", err)
		}
		if store.data.FormatVersion != FormatVersion {
			return nil, fmt.Errorf("unsupported backup state format %d", store.data.FormatVersion)
		}
		if store.data.SnapshotID != snapshotID {
			return nil, fmt.Errorf("backup state belongs to snapshot %s, current snapshot is %s", store.data.SnapshotID, snapshotID)
		}
		if store.data.RemoteDir != remoteDir {
			return nil, fmt.Errorf("backup state remote directory is %q, requested %q", store.data.RemoteDir, remoteDir)
		}
		return store, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	now := time.Now().UTC()
	store.data = Backup{
		FormatVersion: FormatVersion,
		SnapshotID:    snapshotID,
		Source:        source,
		RemoteDir:     remoteDir,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := store.saveLocked(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Snapshot() Backup {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneBackup(s.data)
}

func (s *Store) AddSealed(v Volume) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.Index != s.data.Cursor.NextVolume {
		return fmt.Errorf("volume index %d does not match cursor %d", v.Index, s.data.Cursor.NextVolume)
	}
	for _, existing := range s.data.Volumes {
		if existing.Index == v.Index {
			return fmt.Errorf("volume %d already exists in state", v.Index)
		}
	}
	s.data.Volumes = append(s.data.Volumes, v)
	s.data.Cursor = v.CursorAfter
	s.data.UpdatedAt = time.Now().UTC()
	return s.saveLocked()
}

func (s *Store) IncrementAttempt(index int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Volumes {
		if s.data.Volumes[i].Index == index {
			s.data.Volumes[i].UploadAttempts++
			s.data.UpdatedAt = time.Now().UTC()
			return s.saveLocked()
		}
	}
	return fmt.Errorf("unknown volume %d", index)
}

func (s *Store) MarkUploaded(index int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Volumes {
		if s.data.Volumes[i].Index == index {
			s.data.Volumes[i].Uploaded = true
			s.data.Volumes[i].UploadedAt = time.Now().UTC()
			s.data.Volumes[i].BlockMD5s = nil
			s.data.UpdatedAt = time.Now().UTC()
			return s.saveLocked()
		}
	}
	return fmt.Errorf("unknown volume %d", index)
}

func (s *Store) MarkComplete() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.data.Volumes {
		if !v.Uploaded {
			return fmt.Errorf("cannot complete: volume %d is not uploaded", v.Index)
		}
	}
	if !s.data.Cursor.Done {
		return fmt.Errorf("cannot complete: archive cursor has not reached end of manifest")
	}
	s.data.Completed = true
	s.data.UpdatedAt = time.Now().UTC()
	return s.saveLocked()
}

func (s *Store) MarkArchiveDone() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Cursor.Done = true
	s.data.UpdatedAt = time.Now().UTC()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := s.path + ".tmp"
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
	return os.Rename(tmp, s.path)
}

func cloneBackup(in Backup) Backup {
	out := in
	out.Volumes = append([]Volume(nil), in.Volumes...)
	for i := range out.Volumes {
		out.Volumes[i].BlockMD5s = append([]string(nil), in.Volumes[i].BlockMD5s...)
	}
	return out
}
