package recording

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"tiktok-live-recorder/internal/fault"
)

// processingError records whether the failing stage concerns damaged media.
// Keep this separate from its readable explanation and diagnostic causes.
type processingError struct {
	error
	repairable bool
}

func (e *processingError) Unwrap() error { return e.error }

// CanRepair reports whether a normal conversion failed on media that tolerant
// decoding may recover. Setup, filesystem and cancellation failures do not retry.
func CanRepair(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var file *os.PathError
	if errors.As(err, &file) {
		return false
	}
	var failure *processingError
	return errors.As(err, &failure) && failure.repairable
}

// FLV parsing errors describe the input; operating-system read failures do not.
func mediaInputFailure(err error) error {
	var file *os.PathError
	return &processingError{error: err, repairable: !errors.As(err, &file)}
}

func processingFailure(ctxErr, err error, stderr, operation string) error {
	if ctxErr != nil {
		message := "Processing was interrupted; raw FLV kept"
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			message = "Processing timed out; raw FLV kept"
		}
		return fault.Wrap(message, errors.Join(ctxErr, ffmpegCause(err, stderr)))
	}
	message, repairable := ffmpegMessage(operation, stderr)
	return fault.Action(message+"; raw FLV kept", &processingError{
		error:      fmt.Errorf("FFmpeg could not %s: %w", operation, ffmpegCause(err, stderr)),
		repairable: repairable,
	})
}

func ffmpegCause(err error, stderr string) error {
	if stderr == "" {
		return err
	}
	return errors.Join(err, errors.New("FFmpeg output:\n"+stderr))
}

func ffmpegMessage(operation, stderr string) (string, bool) {
	message := "FFmpeg could not " + operation
	if stderr == "" {
		return message, false
	}
	text := strings.ToLower(stderr)
	switch {
	case strings.Contains(text, "no space left"), strings.Contains(text, "not enough space on the disk"), strings.Contains(text, "disk quota exceeded"):
		return message + ": the disk is full. Free disk space before trying again", false
	case strings.Contains(text, "permission denied"), strings.Contains(text, "access is denied"):
		return message + ": access to the file was denied. Check the output folder permissions", false
	case strings.Contains(text, "input/output error"), strings.Contains(text, "read-only file system"), strings.Contains(text, "no such file or directory"), strings.Contains(text, "too many open files"):
		return message + ": a file could not be read or written. Check the recording drive and output folder", false
	case strings.Contains(text, "unknown encoder"), strings.Contains(text, "encoder not found"):
		return message + ": the selected encoder is unavailable in this FFmpeg installation", false
	case strings.Contains(text, "unrecognized option"), strings.Contains(text, "error splitting the argument list"),
		strings.Contains(text, "no such filter"), strings.Contains(text, "error applying option"), strings.Contains(text, "unable to parse option value"):
		return message + ": check the custom output options", false
	case strings.Contains(text, "invalid data found when processing input"),
		strings.Contains(text, "error while decoding"),
		strings.Contains(text, "error decoding"),
		strings.Contains(text, "error submitting packet to decoder"),
		strings.Contains(text, "error processing packet in decoder"),
		strings.Contains(text, "corrupt decoded frame"),
		strings.Contains(text, "packet corrupt"),
		strings.Contains(text, "invalid nal unit"),
		strings.Contains(text, "pps_id ") && strings.Contains(text, "out of range"),
		strings.Contains(text, "sps_id ") && strings.Contains(text, "out of range"),
		strings.Contains(text, "non-existing pps"), strings.Contains(text, "decode_slice_header error"):
		return "The recording contains damaged or incomplete media data", true
	default:
		return message + ". The diagnostic log contains FFmpeg's error output", canRecoverMP4(errors.New(stderr))
	}
}
