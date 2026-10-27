package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
)

// CompleteProcessing saves the playback copy before optional raw cleanup.
// The lock serializes cleanup with settings changes and history removal. The
// caller supplies a short cleanup operation that rechecks the files and context.
// A true result means removal succeeded, even if saving its status then failed.
func (s *Store) CompleteProcessing(source, processed string, deleteRaw bool, remove func() error) (bool, error) {
	if source == "" || processed == "" || filepath.Clean(source) == filepath.Clean(processed) {
		return false, errors.New("the processed copy must be a separate file")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.data.Recordings, func(r Recording) bool { return r.Path == source })
	if i < 0 {
		return false, nil // Clearing history must not recreate entries or delete files.
	}
	if s.data.Recordings[i].EndedAt.IsZero() {
		return false, errors.New("the recording has not been finalized")
	}
	next := clone(s.data)
	next.Recordings[i].ProcessedPath = processed
	if err := writeJSON(filepath.Join(s.dir, "data.json"), next); err != nil {
		return false, fmt.Errorf("save processed recording: %w", err)
	}
	s.data = next
	s.dataRevision++
	if !deleteRaw || !s.config.PostProcess || !s.config.DeleteRawAfterProcessing || next.Recordings[i].RawDeleted {
		return false, nil
	}
	if remove == nil {
		return false, errors.New("raw cleanup is unavailable")
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(s.dir)
		if err != nil {
			return false, fmt.Errorf("flush recording history: %w", err)
		}
		if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
			return false, fmt.Errorf("flush recording history: %w", err)
		}
	}
	if err := remove(); err != nil {
		return false, err
	}
	// The copy is already persisted. Reflect the actual filesystem state even if
	// this second write fails; reopening can still find the surviving copy.
	next = clone(s.data)
	next.Recordings[i].RawDeleted = true
	s.data = next
	s.dataRevision++
	if err := writeJSON(filepath.Join(s.dir, "data.json"), next); err != nil {
		return true, fmt.Errorf("save raw cleanup status: %w", err)
	}
	return true, nil
}
