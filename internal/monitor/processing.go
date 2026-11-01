package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"tiktok-live-recorder/internal/fault"
	"tiktok-live-recorder/internal/state"
)

// processCopy runs independently of the discovery queue and the raw recorder.
// Limit concurrent converters; shutdown cancels both running and queued jobs.
// The caller holds e.mu, so the processor and recording settings are captured
// before another UI operation can change them.
func (e *Engine) processCopy(id, source string, cfg state.Config) {
	process := e.process
	e.wg.Go(func() {
		select {
		case e.processing <- struct{}{}:
			defer func() { <-e.processing }()
		case <-e.ctx.Done():
			return
		}
		deleteRaw := cfg.PostProcess && cfg.DeleteRawAfterProcessing
		var original os.FileInfo
		var originalErr error
		if deleteRaw {
			original, originalErr = os.Lstat(source)
		}
		e.log.Write("INFO", id, "Creating a processed copy; raw FLV is kept until processing completes")
		path, err := process(e.ctx, source, cfg)
		if e.ctx.Err() != nil {
			e.log.Write("INFO", id, "Processing interrupted on shutdown; raw FLV is preserved")
			return
		}
		var copyInfo os.FileInfo
		if err == nil {
			copyInfo, err = os.Lstat(path)
			if err != nil || !copyInfo.Mode().IsRegular() || copyInfo.Size() == 0 {
				err = fault.Action("the processed copy is missing or invalid; raw FLV kept", err)
			}
		}
		if err != nil {
			e.persist(func(d *state.Data) {
				for i := range d.Recordings {
					if d.Recordings[i].Path == source {
						d.Recordings[i].Error = fault.Reason(err)
					}
				}
			})
			level := "WARN"
			if fault.RequiresAction(err) {
				level = "ERROR"
			}
			e.log.WriteError(level, id, "Processed copy failed: "+fault.Reason(err), err)
			return
		}
		deleted, err := e.store.CompleteProcessing(source, path, deleteRaw, func() error {
			if originalErr != nil {
				return fault.Wrap("the original FLV could not be checked before cleanup", originalErr)
			}
			return removeProcessedRaw(e.ctx, source, path, original, copyInfo)
		})
		if deleted {
			e.log.Write("INFO", id, "Processed copy ready; original FLV deleted")
			if err != nil {
				e.log.WriteError("ERROR", id, "The raw FLV was deleted, but its cleanup status could not be saved: "+fault.Reason(err)+". The processed copy is retained in history.", err)
			}
		} else if err != nil {
			e.log.WriteError("WARN", id, "Processed copy ready; raw FLV kept because cleanup could not complete: "+fault.Reason(err), err)
		} else {
			e.log.Write("INFO", id, "Processed copy ready; raw FLV kept")
		}
	})
}

// removeProcessedRaw rechecks both file identities immediately before unlinking
// only the original FLV. Replacements, edits, links and cancellation retain it.
func removeProcessedRaw(ctx context.Context, source, processed string, original, copyInfo os.FileInfo) error {
	if original == nil || copyInfo == nil || !strings.EqualFold(filepath.Ext(source), ".flv") {
		return errors.New("the original FLV could not be identified safely")
	}
	currentRaw, rawErr := os.Lstat(source)
	currentCopy, copyErr := os.Lstat(processed)
	unchanged := func(before, after os.FileInfo) bool {
		return before.Mode().IsRegular() && after.Mode().IsRegular() && os.SameFile(before, after) &&
			before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
	}
	if rawErr != nil || copyErr != nil || !unchanged(original, currentRaw) || !unchanged(copyInfo, currentCopy) || currentCopy.Size() == 0 || os.SameFile(currentRaw, currentCopy) {
		return fault.Wrap("the original or processed file changed or could not be checked; cleanup was skipped", errors.Join(rawErr, copyErr))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(source); err != nil {
		return fault.Wrap("the original FLV could not be deleted; check file access and folder permissions", err)
	}
	return nil
}
