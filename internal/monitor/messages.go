package monitor

import (
	"fmt"
	"time"

	"tiktok-live-recorder/internal/fault"
	"tiktok-live-recorder/internal/recording"
	"tiktok-live-recorder/internal/state"
)

func nextCheckMessage(cfg state.Config, it *item) string {
	if !cfg.PollingEnabled {
		return "Polling is disabled."
	}
	if it.row.Streamer.Paused {
		return "Monitoring is paused."
	}
	delay := max(time.Second, time.Until(it.row.NextCheck).Round(time.Second))
	if !it.recoveryUntil.IsZero() {
		return fmt.Sprintf("Checking for a resumed stream in %s; recovery checks are limited to %s. The request queue may add a delay.", delay, recoveryWindow)
	}
	return fmt.Sprintf("Next check queued in %s; the request queue may add a delay.", delay)
}

func recordingCompletion(result recording.Result, stopped bool) (level, message string) {
	if result.TransportEnded && !fault.RequiresAction(result.Err) && result.Bytes > 0 {
		return "INFO", fmt.Sprintf("The video connection closed; received raw FLV saved (%d bytes).", result.Bytes)
	}
	if result.Err == nil && result.Bytes > 0 {
		if stopped {
			return "INFO", fmt.Sprintf("Recording stopped; raw FLV finalized (%d bytes).", result.Bytes)
		}
		return "INFO", fmt.Sprintf("Raw FLV finalized (%d bytes).", result.Bytes)
	}
	level, message = "WARN", "Recording stopped: "+fault.Reason(result.Err)+"."
	if fault.RequiresAction(result.Err) {
		level = "ERROR"
	}
	if result.Bytes > 0 {
		message += fmt.Sprintf(" Existing raw FLV kept (%d bytes); file completeness could not be confirmed.", result.Bytes)
	} else {
		message = "Recording stopped before any saved media could be confirmed."
		if result.Err != nil {
			message += " " + fault.Reason(result.Err) + "."
		}
	}
	return level, message
}
