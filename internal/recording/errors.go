package recording

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"tiktok-live-recorder/internal/fault"
)

func processingFailure(ctxErr, err error, stderr, operation string) error {
	if ctxErr != nil {
		message := "Processing was interrupted; raw FLV kept"
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			message = "Processing timed out; raw FLV kept"
		}
		return fault.Wrap(message, errors.Join(ctxErr, ffmpegCause(err, stderr)))
	}
	return fault.Action(ffmpegMessage(operation, stderr)+"; raw FLV kept",
		fmt.Errorf("FFmpeg could not %s: %w", operation, ffmpegCause(err, stderr)))
}

func ffmpegCause(err error, stderr string) error {
	if stderr == "" {
		return err
	}
	return errors.Join(err, errors.New("FFmpeg output:\n"+stderr))
}

func ffmpegMessage(operation, stderr string) string {
	message := "FFmpeg could not " + operation
	if stderr == "" {
		return message
	}
	text := strings.ToLower(stderr)
	switch {
	case strings.Contains(text, "no space left"), strings.Contains(text, "not enough space on the disk"):
		return message + ": the disk is full. Free disk space before trying again"
	case strings.Contains(text, "permission denied"), strings.Contains(text, "access is denied"):
		return message + ": access to the file was denied. Check the output folder permissions"
	case strings.Contains(text, "unknown encoder"), strings.Contains(text, "encoder not found"):
		return message + ": the selected encoder is unavailable in this FFmpeg installation"
	case strings.Contains(text, "unrecognized option"), strings.Contains(text, "error splitting the argument list"):
		return message + ": check the custom output options"
	case strings.Contains(text, "invalid data found when processing input"),
		strings.Contains(text, "error while decoding"),
		strings.Contains(text, "corrupt decoded frame"),
		strings.Contains(text, "packet corrupt"),
		strings.Contains(text, "invalid nal unit"):
		return "The recording contains damaged or incomplete media data"
	default:
		return message + ". The diagnostic log contains FFmpeg's error output"
	}
}
