package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"tiktok-live-recorder/internal/fault"
	"tiktok-live-recorder/internal/recording"
	"tiktok-live-recorder/internal/state"
)

type ProcessingState struct {
	Queued     bool
	Repair     bool
	Cancelable bool
	StartedAt  time.Time
	Progress   recording.Progress
	Pass       int
	Passes     int
	Percent    int // Current pass; -1 when its duration is unknown.
	Overall    int // Estimated work across all planned passes; -1 if unknown.
}

type processingJob struct {
	ProcessingState
	recordingID string
	cancel      context.CancelFunc
	tracker     conversionProgress
}

// ProcessingFailure is a completed conversion failure awaiting UI feedback.
type ProcessingFailure struct {
	StreamerID, Path, Message string
	Repair                    bool
}

// TakeProcessingFailures delivers each failure once, including jobs that finish
// between UI snapshots or repeat the same error on a later retry. Keeping these
// separate from the recording-start channel avoids dropping errors when it fills.
func (e *Engine) TakeProcessingFailures() []ProcessingFailure {
	e.mu.Lock()
	defer e.mu.Unlock()
	failures := e.processingFailures
	e.processingFailures = nil
	return failures
}

// RetryProcessing uses the same bounded converter queue as newly completed
// recordings. An explicit retry also works when automatic processing is off.
func (e *Engine) RetryProcessing(recordingID string) error {
	return e.retryProcessing(recordingID, false)
}

// RepairRecording explicitly opts into tolerant conversion, using the same
// bounded queue and cancellation controls. It always retains the original FLV.
func (e *Engine) RepairRecording(recordingID string) error {
	return e.retryProcessing(recordingID, true)
}

func (e *Engine) retryProcessing(recordingID string, repair bool) (err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := "recordings"
	defer func() {
		if err != nil && e.log != nil && e.ctx.Err() == nil {
			operation := "Could not queue MP4 conversion: "
			if repair {
				operation = "Could not queue recording repair: "
			}
			e.log.WriteError("ERROR", id, operation+fault.Reason(err), err)
		}
	}()
	if err := e.ctx.Err(); err != nil {
		return err
	}
	rows := e.store.Data().Recordings
	i := slices.IndexFunc(rows, func(r state.Recording) bool { return r.ID == recordingID })
	if i < 0 {
		return errors.New("recording is no longer in history")
	}
	r := rows[i]
	id = r.StreamerID
	if r.EndedAt.IsZero() || r.RawDeleted || r.Path == "" {
		return errors.New("choose a completed recording whose original FLV is still available")
	}
	if e.processingJobs[r.Path] != nil {
		return errors.New("this recording is already queued for MP4 conversion")
	}
	info, err := os.Lstat(r.Path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 13 || !strings.EqualFold(filepath.Ext(r.Path), ".flv") {
		return fault.Action("the original FLV is missing or cannot be read for conversion", err)
	}
	cfg := e.store.Config()
	if repair {
		cfg.FFmpegArgs = ""
		cfg.DeleteRawAfterProcessing = false
	}
	e.processCopyMode(r.StreamerID, r.Path, cfg, repair)
	return nil
}

func (e *Engine) IsProcessing(source string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.processingJobs[source] != nil
}

// CancelProcessing interrupts one queued or running conversion independently
// of recording and monitoring. Its original and any previous copy are kept.
func (e *Engine) CancelProcessing(recordingID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, job := range e.processingJobs {
		if job.recordingID == recordingID {
			if !job.Cancelable {
				return errors.New("this conversion has already finished")
			}
			job.cancel()
			job.Queued, job.Cancelable = false, false
			job.Progress.Phase = "Canceling conversion"
			job.Percent = -1
			e.processingRevision++
			return nil
		}
	}
	return errors.New("this recording is no longer queued for conversion")
}

// processCopy runs independently of the discovery queue and the raw recorder.
// Limit concurrent converters; shutdown cancels both running and queued jobs.
// The caller holds e.mu, so the processor and recording settings are captured
// before another UI operation can change them.
func (e *Engine) processCopy(id, source string, cfg state.Config) {
	e.processCopyMode(id, source, cfg, false)
}

func (e *Engine) processCopyMode(id, source string, cfg state.Config, repair bool) {
	if e.processingJobs == nil {
		e.processingJobs = make(map[string]*processingJob)
	}
	if e.processingJobs[source] != nil || e.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(e.ctx)
	job := &processingJob{
		ProcessingState: ProcessingState{Queued: true, Repair: repair, Cancelable: true, Passes: 2, Percent: -1, Overall: -1},
		cancel:          cancel,
		tracker:         conversionProgress{custom: strings.TrimSpace(cfg.FFmpegArgs) != ""},
	}
	for _, r := range e.store.Data().Recordings {
		if r.Path == source {
			job.recordingID = r.ID
			if !r.StartedAt.IsZero() && r.EndedAt.After(r.StartedAt) {
				job.tracker.sourceDuration = r.EndedAt.Sub(r.StartedAt)
			}
			break
		}
	}
	e.processingJobs[source] = job
	e.processingRevision++
	process := e.process
	if repair {
		process = e.repair
	}
	e.wg.Go(func() {
		defer func() {
			cancel()
			e.mu.Lock()
			if e.processingJobs[source] == job {
				delete(e.processingJobs, source)
			}
			e.processingRevision++
			e.mu.Unlock()
		}()
		select {
		case e.processing <- struct{}{}:
			defer func() { <-e.processing }()
		case <-ctx.Done():
			return
		}
		if ctx.Err() != nil {
			return
		}
		e.mu.Lock()
		job.Queued, job.StartedAt = false, time.Now()
		job.Progress = recording.Progress{Phase: "Preparing conversion"}
		if repair {
			job.Progress.Phase = "Preparing repair"
		}
		e.processingRevision++
		e.mu.Unlock()
		deleteRaw := !repair && cfg.PostProcess && cfg.DeleteRawAfterProcessing
		var original os.FileInfo
		var originalErr error
		if deleteRaw {
			original, originalErr = snapshotFile(source)
		}
		if repair {
			e.log.Write("INFO", id, "Trying a tolerant MP4 repair; original FLV and existing copies are kept")
		} else {
			e.log.Write("INFO", id, "Creating a processed copy; raw FLV is kept until processing completes")
		}
		report := func(progress recording.Progress) {
			e.mu.Lock()
			defer e.mu.Unlock()
			if ctx.Err() == nil && e.processingJobs[source] == job && job.Progress != progress {
				job.Progress = progress
				job.Pass, job.Passes, job.Percent, job.Overall = job.tracker.update(progress)
				e.processingRevision++
			}
		}
		copy, err := process(ctx, source, cfg, report)
		if !repair && ctx.Err() == nil && recording.CanRepair(err) {
			normalErr := err
			e.log.WriteError("WARN", id, "Normal MP4 conversion failed: "+fault.Reason(err)+". Trying tolerant repair; original FLV and existing copies are kept", err)
			// Stay in this queue slot and job so cancellation and duplicate
			// protection cover both attempts. Repair never inherits cleanup or
			// custom output options, including when started automatically.
			repair, deleteRaw = true, false
			cfg.FFmpegArgs, cfg.DeleteRawAfterProcessing = "", false
			e.mu.Lock()
			job.Repair = true
			e.processingRevision++
			e.mu.Unlock()
			report(recording.Progress{Phase: "Preparing repair"})
			copy, err = e.repair(ctx, source, cfg, report)
			if err != nil && ctx.Err() == nil {
				err = fault.Action("Normal conversion and automatic repair failed: "+fault.Reason(err), errors.Join(err, normalErr))
			}
		}
		path := copy.Path
		deleteRaw = deleteRaw && !copy.Reencoded && !repair
		if e.ctx.Err() != nil {
			e.log.Write("INFO", id, "Processing interrupted on shutdown; raw FLV is preserved")
			return
		}
		if ctx.Err() != nil {
			err = fault.Wrap("MP4 conversion canceled; raw FLV kept", ctx.Err())
		}
		var copyInfo os.FileInfo
		if err == nil {
			copyInfo, err = snapshotFile(path)
			if err != nil || !copyInfo.Mode().IsRegular() || copyInfo.Size() == 0 {
				err = fault.Action("the processed copy is missing or invalid; raw FLV kept", err)
			}
		}
		if err == nil {
			// Cancellation and the history/cleanup commit have one ordering.
			// Once a verified copy is ready to commit, a late cancel cannot
			// report the completed conversion as canceled after raw cleanup.
			e.mu.Lock()
			if ctx.Err() != nil {
				err = fault.Wrap("MP4 conversion canceled; raw FLV kept", ctx.Err())
			} else {
				job.Cancelable = false
				job.Progress = recording.Progress{Phase: "Finishing conversion"}
				job.Percent, job.Overall = -1, 99
				e.processingRevision++
			}
			e.mu.Unlock()
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
			if errors.Is(err, context.Canceled) {
				level = "INFO"
			} else if fault.RequiresAction(err) {
				level = "ERROR"
			}
			message := "Processed copy failed: "
			if repair {
				message = "Recording repair failed: "
			}
			e.log.WriteError(level, id, message+fault.Reason(err), err)
			if !errors.Is(err, context.Canceled) {
				e.mu.Lock()
				e.processingFailures = append(e.processingFailures, ProcessingFailure{StreamerID: id, Path: source, Message: fault.Reason(err), Repair: repair})
				e.processingRevision++
				e.mu.Unlock()
			}
			return
		}
		deleted, err := e.store.CompleteProcessing(source, path, deleteRaw, func() error {
			if originalErr != nil {
				return fault.Wrap("the original FLV could not be checked before cleanup", originalErr)
			}
			return removeProcessedRaw(ctx, source, path, original, copyInfo)
		})
		if deleted {
			e.log.Write("INFO", id, "Processed copy ready; original FLV deleted")
			if err != nil {
				e.log.WriteError("ERROR", id, "The raw FLV was deleted, but its cleanup status could not be saved: "+fault.Reason(err)+". The processed copy is retained in history.", err)
			}
		} else if err != nil {
			e.log.WriteError("WARN", id, "Processed copy ready; raw FLV kept because cleanup could not complete: "+fault.Reason(err), err)
		} else if repair {
			if copy.Diagnostics != "" {
				e.log.WriteError("WARN", id, "Repaired MP4 ready; damaged media may have been skipped or reconstructed. Original FLV kept", fault.Wrap("FFmpeg repair diagnostics", errors.New(copy.Diagnostics)))
			} else {
				e.log.Write("INFO", id, "Repaired MP4 ready; original FLV and existing copies kept")
			}
		} else if copy.Reencoded {
			e.log.Write("INFO", id, "Processed MP4 ready after re-encoding; raw FLV kept")
		} else {
			e.log.Write("INFO", id, "Processed copy ready; raw FLV kept")
		}
	})
}

// snapshotFile captures a regular file's identity through its open handle.
// On Windows, Lstat loads file IDs lazily when SameFile is called; after a
// replacement that could identify the new file instead of the original one.
// Nonregular paths are returned without opening them so callers reject links
// and devices without following them or blocking on a pipe.
func snapshotFile(path string) (os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return before, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, statErr := f.Stat()
	closeErr := f.Close()
	if err := errors.Join(statErr, closeErr); err != nil {
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !after.Mode().IsRegular() ||
		!os.SameFile(info, after) || before.Size() != info.Size() ||
		!before.ModTime().Equal(info.ModTime()) || after.Size() != info.Size() ||
		!after.ModTime().Equal(info.ModTime()) {
		return nil, errors.New("the recording changed while its file identity was being checked")
	}
	return info, nil
}

// removeProcessedRaw rechecks both file identities immediately before unlinking
// only the original FLV. Replacements, edits, links and cancellation retain it.
func removeProcessedRaw(ctx context.Context, source, processed string, original, copyInfo os.FileInfo) error {
	if original == nil || copyInfo == nil || !strings.EqualFold(filepath.Ext(source), ".flv") {
		return errors.New("the original FLV could not be identified safely")
	}
	currentRaw, rawErr := snapshotFile(source)
	currentCopy, copyErr := snapshotFile(processed)
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
