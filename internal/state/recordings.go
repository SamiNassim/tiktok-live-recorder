package state

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// RemoveRecording removes one finalized history entry, never its media files.
// Active entries are retained so finalization can still update their metadata.
func (s *Store) RemoveRecording(id string) error {
	return s.Update(func(d *Data) error {
		i := slices.IndexFunc(d.Recordings, func(r Recording) bool { return r.ID == id })
		if i < 0 {
			return errors.New("recording is no longer in history")
		}
		if d.Recordings[i].EndedAt.IsZero() {
			return errors.New("active recordings remain in history until finalized")
		}
		d.Recordings = slices.Delete(d.Recordings, i, i+1)
		return nil
	})
}

// ClearRecordingHistory forgets finalized recordings without deleting media.
// The update is serialized with recording starts and finalization. Background
// processing may finish normally, but does not restore a forgotten entry.
func (s *Store) ClearRecordingHistory() (int, error) {
	removed := 0
	err := s.Update(func(d *Data) error {
		before := len(d.Recordings)
		d.Recordings = slices.DeleteFunc(d.Recordings, func(r Recording) bool { return !r.EndedAt.IsZero() })
		removed = before - len(d.Recordings)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// PruneMissingRecordings reconciles finalized history with external deletions.
// It never deletes media. Active files, permission errors and unavailable
// recording folders are preserved, including recordings on disconnected disks.
func (s *Store) PruneMissingRecordings() (int, error) {
	data, root := s.Data(), s.Config().RecordingPath
	available := make(map[string]bool)
	directoryAvailable := func(path string) bool {
		if ok, seen := available[path]; seen {
			return ok
		}
		info, err := os.Stat(path)
		ok := err == nil && info.IsDir()
		available[path] = ok
		return ok
	}
	type recordingPaths struct{ raw, processed string }
	missing := make(map[string]recordingPaths)
	for _, r := range data.Recordings {
		if r.EndedAt.IsZero() || r.Path == "" {
			continue
		}
		allMissing := true
		for _, path := range []string{r.Path, r.ProcessedPath} {
			if path == "" {
				continue
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				allMissing = false
				break
			}
			// Require an available parent or recording root before treating a
			// missing path as deleted, rather than on a disconnected drive.
			rel, err := filepath.Rel(root, path)
			underRoot := err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
			if !directoryAvailable(filepath.Dir(path)) && !(underRoot && directoryAvailable(root)) {
				allMissing = false
				break
			}
		}
		if allMissing {
			missing[r.ID] = recordingPaths{r.Path, r.ProcessedPath}
		}
	}
	if len(missing) == 0 {
		return 0, nil
	}
	removed := 0
	err := s.Update(func(d *Data) error {
		before := len(d.Recordings)
		d.Recordings = slices.DeleteFunc(d.Recordings, func(r Recording) bool {
			paths, ok := missing[r.ID]
			return ok && !r.EndedAt.IsZero() && paths == (recordingPaths{r.Path, r.ProcessedPath})
		})
		removed = before - len(d.Recordings)
		return nil
	})
	return removed, err
}
