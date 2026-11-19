package monitor

import (
	"math"
	"time"

	"tiktok-live-recorder/internal/recording"
)

// The estimate measures media processed across passes, not remaining wall time.
// Recovery can add passes. Completed work remains counted when that happens.
type conversionProgress struct {
	sourceDuration time.Duration
	duration       time.Duration
	completed      float64
	mediaTime      time.Duration
	phase          string
	pass, passes   int
	custom         bool
}

func mediaPass(phase string) bool {
	switch phase {
	case "Creating MP4", "Encoding MP4", "Repairing MP4", "Applying options", "Verifying MP4":
		return true
	}
	return false
}

func (p *conversionProgress) update(value recording.Progress) (pass, passes, percent, overall int) {
	if p.passes == 0 {
		p.passes = 2
		p.duration = p.sourceDuration
	}
	if value.Phase != p.phase {
		if mediaPass(p.phase) {
			p.completed += float64(p.mediaTime)
			// A successful output pass supplies a better duration for the
			// next pass than the recording's wall-clock interval does.
			if (value.Phase == "Verifying MP4" || value.Phase == "Applying options") && p.mediaTime > 0 {
				p.duration = p.mediaTime
			}
		}
		if value.Phase == "Preparing recovery" || value.Phase == "Preparing repair" {
			p.passes = p.pass + 2 // Encoding and final verification.
			if value.Phase == "Preparing recovery" && p.custom {
				p.passes++
			}
			if p.sourceDuration > 0 {
				p.duration = p.sourceDuration
			}
		}
		if mediaPass(value.Phase) {
			p.pass++
		}
		p.phase, p.mediaTime = value.Phase, 0
	}
	percent, overall = -1, -1
	remaining := max(0, p.passes-p.pass)
	done := p.completed
	if mediaPass(value.Phase) {
		p.mediaTime = max(p.mediaTime, value.MediaTime)
		percent = conversionPercent(float64(p.mediaTime), float64(p.duration))
		done += float64(p.mediaTime)
		remaining++
	}
	if p.duration > 0 {
		overall = conversionPercent(done, p.completed+float64(p.duration)*float64(remaining))
	}
	if value.Phase == "Saving MP4" || value.Phase == "Finishing conversion" {
		overall = 99 // Completion requires durable output and saved history.
	}
	return p.pass, p.passes, percent, overall
}

func conversionPercent(done, total float64) int {
	if total <= 0 || math.IsNaN(done) || math.IsNaN(total) || math.IsInf(done, 0) || math.IsInf(total, 0) {
		return -1
	}
	return int(min(99, max(0, math.Floor(100*done/total))))
}
