package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"tiktok-live-recorder/internal/monitor"
	"tiktok-live-recorder/internal/state"
)

func recordingPlaybackPath(r state.Recording) string {
	if r.ProcessedPath != "" {
		return r.ProcessedPath
	}
	return r.Path
}

func (a *application) recordingProcessing(r state.Recording) bool {
	_, ok := a.snapshot.Processing[r.Path]
	return ok
}

func conversionStatus(job monitor.ProcessingState) string {
	if job.Queued {
		return "Queued…"
	}
	phase := "Converting"
	switch job.Progress.Phase {
	case "Preparing conversion":
		phase = "Preparing"
	case "Preparing recovery", "Applying options":
		phase = job.Progress.Phase
	case "Encoding MP4":
		phase = "Encoding"
	case "Verifying MP4":
		phase = "Validating"
	case "Saving MP4":
		phase = "Saving"
	case "Finishing conversion":
		phase = "Finishing"
	case "Canceling conversion":
		return "Canceling…"
	}
	if job.Overall >= 0 {
		return fmt.Sprintf("%s %d%%", phase, job.Overall)
	}
	return phase + "…"
}

func conversionDetails(job monitor.ProcessingState, now time.Time) string {
	parts := []string{conversionStatus(job)}
	if job.Progress.MediaTime > 0 {
		parts = append(parts, "Processed this pass: "+duration(job.Progress.MediaTime))
	}
	if job.Progress.Speed > 0 {
		parts = append(parts, fmt.Sprintf("Speed: %.1f×", job.Progress.Speed))
	}
	if !job.StartedAt.IsZero() {
		parts = append(parts, "Elapsed: "+duration(now.Sub(job.StartedAt)))
	}
	if !job.Queued {
		parts = append(parts, "Overall estimates media work across all passes, not time remaining. Recovery can add passes and revise the estimate.")
	}
	return strings.Join(parts, "\n")
}

func (a *application) recordingMenu(m *ui.Menu, r state.Recording, services ui.Services) {
	path := recordingPlaybackPath(r)
	if m.Item("Open recording").Disabled(a.busy || path == "" || r.EndedAt.IsZero()).Chosen() {
		a.work(func() error { return mygo.Shell.OpenPath(path) })
	}
	if m.Item("Show in folder").Disabled(path == "").Chosen() {
		a.reveal(path)
	}
	if m.Item("Copy file path").Disabled(path == "").Chosen() {
		services.WriteClipboard(path)
	}
	if r.ProcessedPath != "" {
		m.Separator()
		if m.Item("Open original FLV").Disabled(a.busy || r.RawDeleted || r.Path == "" || r.EndedAt.IsZero()).Chosen() {
			a.work(func() error { return mygo.Shell.OpenPath(r.Path) })
		}
		if m.Item("Show original FLV in folder").Disabled(r.RawDeleted || r.Path == "").Chosen() {
			a.reveal(r.Path)
		}
		if m.Item("Copy original FLV path").Disabled(r.RawDeleted || r.Path == "").Chosen() {
			services.WriteClipboard(r.Path)
		}
	}
	{
		m.Separator()
		label := "Convert to MP4"
		processing := a.recordingProcessing(r)
		if processing {
			label = "Converting to MP4…"
		} else if r.ProcessedPath != "" {
			label = "Convert again"
		} else if r.Error != "" {
			label = "Retry MP4 conversion"
		}
		if m.Item(label).Disabled(a.busy || a.preview || a.engine == nil || processing || r.RawDeleted || r.Path == "" || r.EndedAt.IsZero()).Chosen() {
			a.work(func() error {
				if err := a.engine.RetryProcessing(r.ID); err != nil {
					return err
				}
				a.update(func() {
					a.toasts = append(a.toasts, "MP4 conversion queued. The original FLV is kept until processing completes.")
				})
				return nil
			})
		}
		if processing && m.Item("Cancel conversion").Disabled(a.busy || a.preview || a.engine == nil || !a.snapshot.Processing[r.Path].Cancelable).Chosen() {
			a.work(func() error { return a.engine.CancelProcessing(r.ID) })
		}
	}
	m.Separator()
	if m.Item("Remove from history").Disabled(a.busy || a.preview || r.EndedAt.IsZero()).Chosen() {
		a.work(func() error {
			if err := a.store.RemoveRecording(r.ID); err != nil {
				return err
			}
			a.update(func() { a.toasts = append(a.toasts, "Recording removed from history. Files kept.") })
			return nil
		})
	}
}

func (a *application) recordingHistoryDialog(c *ui.Context) {
	ui.Modal(c, &a.clearHistoryOpen, func() {
		ui.Text(c, "Clear recording history?").FontSize(22).FontWeight(600)
		ui.Text(c, "Remove all completed recordings from this list?\nFiles stay on disk. Active recordings stay in the list.").Width(400)
		ui.Row(c).Justify(ui.End).Gap(10).Children(func() {
			if ui.Button(c, "Cancel").Clicked() {
				a.clearHistoryOpen = false
			}
			if ui.PrimaryButton(c, "Clear history").Disabled(a.busy || a.preview).Clicked() {
				a.work(func() error {
					n, err := a.store.ClearRecordingHistory()
					if err != nil {
						return err
					}
					message := fmt.Sprintf("%d recordings removed from history. Files kept.", n)
					if n == 1 {
						message = "1 recording removed from history. File kept."
					}
					a.update(func() { a.toasts = append(a.toasts, message) })
					return nil
				})
				a.clearHistoryOpen = false
			}
		})
	})
}
