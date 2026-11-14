package recording

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"tiktok-live-recorder/internal/fault"
)

// Only retry failures that can plausibly be fixed by decoding and rebuilding
// the MP4. I/O failures, bad arguments and cancellation keep their original error.
func canRecoverMP4(err error) bool {
	detail := strings.ToLower(fault.Details(err))
	for _, reason := range []string{"could not find tag for codec", "not currently supported in container", "non monotonically increasing dts", "non-monotonous dts"} {
		if strings.Contains(detail, reason) {
			return true
		}
	}
	return false
}

type flvSection struct {
	name         string
	audio, video bool
}

// recoverMP4 decodes each section with its own codec parameters, then joins
// the decoded streams before encoding one continuous track of each type.
func recoverMP4(ctx context.Context, path, source, output string) error {
	dir := filepath.Dir(output)
	sections, err := splitFLV(ctx, source, dir)
	if err != nil {
		return fmt.Errorf("split FLV at media changes: %w", err)
	}
	// A single MP4 video track needs a common canvas. Read the initial
	// dimensions from each section using FFmpeg's single-frame image output.
	width, height := 0, 0
	hasAudio, hasVideo := false, false
	for _, section := range sections {
		hasAudio = hasAudio || section.audio
		hasVideo = hasVideo || section.video
		if !section.video {
			continue
		}
		w, h, err := frameSize(ctx, path, section.name)
		if err != nil {
			return err
		}
		width, height = max(width, w), max(height, h)
	}
	width, height = (width+1)&^1, (height+1)&^1
	argv := []string{"-xerror"}
	var graph, labels strings.Builder
	for i, section := range sections {
		if hasVideo && !section.video {
			return errors.New("a section of the recording has audio but no video frames")
		}
		// Keep the concat and audio clocks intact. Both scale and pad must
		// recompute their geometry when the incoming picture size changes.
		argv = append(argv, "-reinit_filter:v", "0", "-err_detect", "explode", "-i", section.name)
		if hasVideo {
			fmt.Fprintf(&graph, "[%d:v:0]scale=w='max(2,trunc(min(%d,%d*dar)/2)*2)':h='max(2,trunc(min(%d,%d/dar)/2)*2)':eval=frame,setsar=1,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:eval=frame,format=yuv420p[v%d];", i, width, height, height, width, width, height, i)
			fmt.Fprintf(&labels, "[v%d]", i)
		}
		if hasAudio {
			if section.audio {
				fmt.Fprintf(&graph, "[%d:a:0]aresample=48000:async=1:first_pts=0,aformat=channel_layouts=stereo[a%d];", i, i)
			} else {
				// Concat pads shorter audio to the video duration between sections.
				fmt.Fprintf(&graph, "anullsrc=r=48000:cl=stereo,atrim=end_sample=1[a%d];", i)
			}
			fmt.Fprintf(&labels, "[a%d]", i)
		}
	}
	v, a := 0, 0
	if hasVideo {
		v = 1
	}
	if hasAudio {
		a = 1
	}
	fmt.Fprintf(&graph, "%sconcat=n=%d:v=%d:a=%d", labels.String(), len(sections), v, a)
	var maps []string
	if hasVideo {
		graph.WriteString("[video]")
		maps = append(maps, "-map", "[video]")
	}
	if hasAudio {
		graph.WriteString("[audio]")
		maps = append(maps, "-map", "[audio]")
	}
	argv = append(argv, "-filter_complex", graph.String())
	argv = append(argv, maps...)
	if hasVideo {
		encoder, err := h264Options(ctx, path)
		if err != nil {
			return err
		}
		argv = append(argv, encoder...)
		argv = append(argv, "-pix_fmt", "yuv420p", "-fps_mode:v", "passthrough", "-enc_time_base:v", "1:1000")
	}
	if hasAudio {
		argv = append(argv, "-c:a", "aac", "-b:a", "192k")
	}
	return runFFmpeg(ctx, path, nil, "encode a continuous MP4", append(argv, "-movflags", "+faststart", output)...)
}

func h264Options(ctx context.Context, path string) ([]string, error) {
	cmd := exec.CommandContext(ctx, path, "-hide_banner", "-encoders")
	configureCommand(cmd)
	list, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list FFmpeg encoders: %w", err)
	}
	for _, name := range []string{"libx264", "h264_videotoolbox", "h264_mf"} {
		if bytes.Contains(list, []byte(" "+name+" ")) {
			if name == "libx264" {
				return []string{"-c:v", name, "-preset", "veryfast", "-crf", "18"}, nil
			}
			return []string{"-c:v", name, "-b:v", "12M"}, nil
		}
	}
	return nil, errors.New("this FFmpeg installation has no supported H.264 encoder; select a build with libx264")
}

func frameSize(ctx context.Context, path, section string) (int, int, error) {
	cmd := exec.CommandContext(ctx, path, "-hide_banner", "-loglevel", "error", "-nostdin", "-i", section, "-map", "0:v:0", "-frames:v", "1", "-c:v", "png", "-threads", "1", "-f", "image2pipe", "pipe:1")
	configureCommand(cmd)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, ffmpegCause(err, stderr.String())
	}
	if len(out) < 24 || string(out[12:16]) != "IHDR" {
		return 0, 0, errors.New("FFmpeg could not read the section's video dimensions")
	}
	return int(binary.BigEndian.Uint32(out[16:20])), int(binary.BigEndian.Uint32(out[20:24])), nil
}
