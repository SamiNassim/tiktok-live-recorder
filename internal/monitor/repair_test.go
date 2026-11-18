package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tiktok-live-recorder/internal/fault"
	"tiktok-live-recorder/internal/recording"
	"tiktok-live-recorder/internal/state"
)

func TestRepairQueuePreservesOriginalAndPreviousCopy(t *testing.T) {
	for _, name := range []string{"success", "failed", "canceled", "shutdown"} {
		t.Run(name, func(t *testing.T) {
			s, log := fixture(t)
			cfg := s.Config()
			cfg.PollingEnabled, cfg.DeleteRawAfterProcessing, cfg.FFmpegArgs = false, true, "-c copy"
			if err := s.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			raw, previous, output := filepath.Join(dir, "source.flv"), filepath.Join(dir, "previous.mp4"), filepath.Join(dir, "source.repaired.mp4")
			for _, path := range []string{raw, previous} {
				if err := os.WriteFile(path, []byte("synthetic existing media"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now()
			row := state.Recording{ID: "completed", StreamerID: "demo_creator", Path: raw, ProcessedPath: previous, StartedAt: now.Add(-4 * time.Second), EndedAt: now}
			if err := s.Update(func(d *state.Data) error { d.Recordings = []state.Recording{row}; return nil }); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			e := &Engine{store: s, log: log, ctx: ctx, cancel: cancel, processing: make(chan struct{}, 2)}
			t.Cleanup(e.Close)
			started, finish := make(chan state.Config, 1), make(chan struct{})
			e.process = func(context.Context, string, state.Config, func(recording.Progress)) (recording.ProcessedCopy, error) {
				return recording.ProcessedCopy{}, errors.New("strict converter must not handle explicit repair")
			}
			const reason = "Synthetic repair failure; raw FLV kept"
			e.repair = func(ctx context.Context, source string, cfg state.Config, report func(recording.Progress)) (recording.ProcessedCopy, error) {
				report(recording.Progress{Phase: "Repairing MP4", MediaTime: 2 * time.Second})
				started <- cfg
				select {
				case <-ctx.Done():
					return recording.ProcessedCopy{}, ctx.Err()
				case <-finish:
				}
				if name == "failed" {
					return recording.ProcessedCopy{}, fault.Action(reason, errors.New("synthetic repair diagnostic"))
				}
				if err := os.WriteFile(output, []byte("synthetic verified repair"), 0600); err != nil {
					return recording.ProcessedCopy{}, err
				}
				// The queue must protect raw files independently of the processor's
				// Reencoded flag and the global cleanup preference.
				return recording.ProcessedCopy{Path: output, Diagnostics: "synthetic tolerated decoder error"}, nil
			}
			if err := e.RepairRecording(row.ID); err != nil {
				t.Fatal(err)
			}
			select {
			case observed := <-started:
				if observed.DeleteRawAfterProcessing || observed.FFmpegArgs != "" {
					t.Fatal("repair inherited cleanup or custom options", observed.DeleteRawAfterProcessing, observed.FFmpegArgs)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("repair did not start")
			}
			job := e.Snapshot().Processing[raw]
			if !job.Repair || job.Overall != 25 || job.Pass != 1 || job.Passes != 2 {
				t.Fatal("repair progress is not observable", job)
			}
			if e.RetryProcessing(row.ID) == nil || e.RepairRecording(row.ID) == nil {
				t.Fatal("repair and normal conversion did not share duplicate protection")
			}
			switch name {
			case "canceled":
				if err := e.CancelProcessing(row.ID); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				e.Close()
			default:
				close(finish)
			}
			waitFor(t, func() bool { return !e.IsProcessing(raw) })
			got := s.Data().Recordings[0]
			if name == "success" && got.ProcessedPath != output || name != "success" && got.ProcessedPath != previous || got.RawDeleted {
				t.Fatal("repair published an invalid result or changed raw cleanup status", got)
			}
			for _, path := range []string{raw, previous} {
				if data, err := os.ReadFile(path); err != nil || string(data) != "synthetic existing media" {
					t.Fatal("repair changed an existing file", err)
				}
			}
			if got := s.Config(); !got.DeleteRawAfterProcessing || got.FFmpegArgs != "-c copy" {
				t.Fatal("repair changed global conversion preferences")
			}
			failures := e.TakeProcessingFailures()
			if name == "failed" {
				if len(failures) != 1 || !failures[0].Repair || failures[0].Message != reason || got.Error != reason {
					t.Fatal("repair failure lost its mode or readable feedback", failures)
				}
			} else if len(failures) != 0 {
				t.Fatal("repair success or cancellation reported a failure", failures)
			}
			if name == "success" {
				found := false
				for _, entry := range log.Entries() {
					if strings.HasPrefix(entry.Message, "Repaired MP4 ready;") && strings.Contains(entry.Details, "synthetic tolerated decoder error") {
						found = true
					}
				}
				if !found {
					t.Fatal("successful repair discarded its decoder diagnostics")
				}
			}
		})
	}
}

func TestRepairCanCancelWhileWaitingForSharedConverterQueue(t *testing.T) {
	s, log := fixture(t)
	raw := filepath.Join(t.TempDir(), "source.flv")
	if err := os.WriteFile(raw, []byte("synthetic original media"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(d *state.Data) error {
		d.Recordings = []state.Recording{{ID: "completed", Path: raw, EndedAt: time.Now()}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	e := &Engine{store: s, log: log, ctx: ctx, cancel: cancel, processing: make(chan struct{}, 2)}
	t.Cleanup(e.Close)
	e.processing <- struct{}{}
	e.processing <- struct{}{}
	if err := e.RepairRecording("completed"); err != nil {
		t.Fatal(err)
	}
	if job := e.Snapshot().Processing[raw]; !job.Queued || !job.Repair {
		t.Fatal("repair bypassed the shared queue", job)
	}
	if err := e.CancelProcessing("completed"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !e.IsProcessing(raw) })
	if len(e.TakeProcessingFailures()) != 0 || s.Data().Recordings[0].ProcessedPath != "" {
		t.Fatal("a canceled queued repair was processed or reported as failed")
	}
}

func TestRejectedRepairIsLoggedWithReadableReason(t *testing.T) {
	s, log := fixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	e := &Engine{store: s, log: log, ctx: ctx, cancel: cancel}
	t.Cleanup(e.Close)
	err := e.RepairRecording("missing-recording")
	if err == nil {
		t.Fatal("missing recording was accepted for repair")
	}
	entries := log.Entries()
	if len(entries) != 1 || entries[0].Message != "Could not queue recording repair: "+fault.Reason(err) {
		t.Fatal("immediate repair failure was not explained in logs", entries)
	}
}
