package recording

import (
	"bytes"
	"math"
	"strconv"
	"strings"
	"time"
)

// Progress describes the current conversion phase and the media processed in
// that phase. MediaTime is a media clock, not a guessed completion percentage.
type Progress struct {
	Phase     string
	MediaTime time.Duration
	Speed     float64
}

func reportPhase(report func(Progress), phase string) {
	if report != nil {
		report(Progress{Phase: phase})
	}
}

func processingPhase(operation string) string {
	switch operation {
	case "create the processed copy":
		return "Creating MP4"
	case "encode a continuous MP4":
		return "Encoding MP4"
	case "create a repaired MP4":
		return "Repairing MP4"
	case "apply the custom MP4 options":
		return "Applying options"
	case "verify the processed copy":
		return "Verifying MP4"
	default:
		return "Converting to MP4"
	}
}

// FFmpeg writes key=value records to stdout, independently of its diagnostic
// stderr. Never treat progress=end as proof of a successful conversion.
type progressWriter struct {
	report  func(Progress)
	value   Progress
	line    []byte
	discard bool
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			end = len(p)
		}
		if len(w.line)+end > 4096 {
			w.discard = true
			w.line = w.line[:0]
		} else if !w.discard {
			w.line = append(w.line, p[:end]...)
		}
		if end == len(p) {
			break
		}
		if !w.discard {
			w.parse(strings.TrimSpace(string(w.line)))
		}
		w.line, w.discard = w.line[:0], false
		p = p[end+1:]
	}
	return n, nil
}

func (w *progressWriter) parse(line string) {
	key, value, ok := strings.Cut(line, "=")
	if !ok {
		return
	}
	switch key {
	case "out_time_us":
		us, err := strconv.ParseInt(value, 10, 64)
		if err == nil && us >= 0 && us <= math.MaxInt64/int64(time.Microsecond) {
			w.value.MediaTime = max(w.value.MediaTime, time.Duration(us)*time.Microsecond)
		}
	case "speed":
		speed, err := strconv.ParseFloat(strings.TrimSuffix(value, "x"), 64)
		if err == nil && speed >= 0 && !math.IsNaN(speed) && !math.IsInf(speed, 0) {
			w.value.Speed = speed
		}
	case "progress":
		if (value == "continue" || value == "end") && w.report != nil {
			w.report(w.value)
		}
	}
}
