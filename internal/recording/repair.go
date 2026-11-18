package recording

import (
	"context"

	"tiktok-live-recorder/internal/fault"
	"tiktok-live-recorder/internal/state"
)

// Repair creates a separate MP4 using tolerant decoding. It can conceal or skip
// damaged media, so the original is always retained, even after verification.
func Repair(ctx context.Context, source string, cfg state.Config) (ProcessedCopy, error) {
	return RepairWithProgress(ctx, source, cfg, nil)
}

// RepairWithProgress uses a fixed repair profile, independently of custom output
// options. Publication and full output verification are shared with conversion.
func RepairWithProgress(ctx context.Context, source string, cfg state.Config, report func(Progress)) (ProcessedCopy, error) {
	cfg.FFmpegArgs = ""
	cfg.DeleteRawAfterProcessing = false
	return processWithProgress(ctx, source, cfg, report, true)
}

func repairMP4Progress(ctx context.Context, path, source, output string, report func(Progress)) (string, error) {
	encoder, err := h264Options(ctx, path)
	if err != nil {
		if ctx.Err() != nil {
			return "", processingFailure(ctx.Err(), err, "", "create a repaired MP4")
		}
		return "", fault.Action("Could not create a repaired MP4; select an FFmpeg build with an H.264 encoder. Raw FLV kept", err)
	}
	// Read the original directly: a tolerant decoder must be able to continue
	// beyond damaged packets or timestamp resets. All repairs affect this new
	// playback copy; FFmpeg never writes to the source FLV.
	args := []string{
		"-fflags", "+discardcorrupt+genpts", "-err_detect", "ignore_err", "-i", source,
		"-map", "0:v?", "-map", "0:a?",
		"-vf", "settb=1/1000,setpts='if(isnan(PREV_OUTPTS),PTS,max(PTS,PREV_OUTPTS+1))',scale=trunc(iw/2)*2:trunc(ih/2)*2,format=yuv420p",
	}
	args = append(args, encoder...)
	args = append(args,
		"-threads:v", "4", "-fps_mode:v", "passthrough", "-enc_time_base:v", "1:1000",
		"-c:a", "aac", "-b:a", "192k", "-af", "aresample=48000:async=1:first_pts=0",
		"-movflags", "+faststart", output,
	)
	return runFFmpegProgressMode(ctx, path, nil, "create a repaired MP4", report, true, args...)
}
