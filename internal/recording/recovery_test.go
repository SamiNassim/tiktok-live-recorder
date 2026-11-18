package recording

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"tiktok-live-recorder/internal/fault"
	"tiktok-live-recorder/internal/ffmpeg"
	"tiktok-live-recorder/internal/state"
)

func TestProcessingRecoversPictureTimestampJitter(t *testing.T) {
	path, err := ffmpeg.Find("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.flv")
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=128x96:rate=20", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "4", "-c:v", "libx264", "-threads:v", "1", "-preset", "ultrafast", "-g", "20", "-bf", "3", "-x264-params", "b-adapt=0:scenecut=0", "-c:a", "aac", "-flvflags", "no_duration_filesize", fixture}
	if out, err := exec.CommandContext(ctx, path, args...).CombinedOutput(); err != nil {
		if bytes.Contains(out, []byte("Unknown encoder")) {
			t.Skip("synthetic fixture needs libx264")
		}
		t.Fatalf("generate fixture: %v: %s", err, out)
	}
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	clean := bytes.Clone(data)
	// Missing composition offsets on B-frames produce backward and duplicate
	// presentation times without a reconnect or a codec change. Include two
	// distinct interframes sharing a decode time; neither may be discarded.
	offset := int(binary.BigEndian.Uint32(data[5:9])) + 4
	var frames int
	var previousDecode uint32
	for offset < len(data) {
		h := data[offset : offset+11]
		size := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
		payload := data[offset+11 : offset+11+size]
		if h[0] == 9 && payload[1] == 1 {
			frames++
			if frames == 33 {
				h[4], h[5], h[6], h[7] = byte(previousDecode>>16), byte(previousDecode>>8), byte(previousDecode), byte(previousDecode>>24)
			}
			previousDecode = flvTimestamp(h)
			payload[2], payload[3], payload[4] = 0, 0, 0
		}
		offset += size + 15
	}
	for _, test := range []struct {
		custom bool
		tail   string
	}{{}, {custom: true}, {tail: "header"}, {tail: "payload"}, {tail: "trailer"}} {
		t.Run(fmt.Sprintf("custom=%v/tail=%s", test.custom, test.tail), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			raw := bytes.Clone(data)
			if test.tail != "" {
				raw = bytes.Clone(clean)
			}
			switch test.tail {
			case "header":
				raw = append(raw, 9, 0, 0, 25, 0)
			case "payload":
				raw = appendFLVTag(raw, 9, 5000, make([]byte, 25))
				raw = raw[:len(raw)-10]
			case "trailer":
				raw = raw[:len(raw)-2]
			}
			source := filepath.Join(t.TempDir(), "jitter.flv")
			if err := os.WriteFile(source, raw, 0600); err != nil {
				t.Fatal(err)
			}
			cfg := state.Defaults()
			cfg.FFmpegPath, cfg.DeleteRawAfterProcessing = path, true
			if test.custom {
				cfg.FFmpegArgs = "-map 0:v:0 -map 0:a:0 -c copy -movflags +faststart"
			}
			copy, err := Process(ctx, source, cfg)
			if err != nil {
				t.Fatal(fault.Details(err))
			}
			if !copy.Reencoded {
				t.Fatal("invalid presentation timestamps did not trigger recovery")
			}
			out, err := exec.CommandContext(ctx, path, "-hide_banner", "-loglevel", "error", "-xerror", "-i", copy.Path, "-map", "0:v:0", "-fps_mode", "passthrough", "-enc_time_base:v", "demux", "-f", "framemd5", "-").CombinedOutput()
			if err != nil {
				t.Fatalf("decode recovered MP4: %v: %s", err, out)
			}
			count, previous := 0, int64(-1)
			var tick float64
			for line := range strings.SplitSeq(string(out), "\n") {
				if strings.HasPrefix(line, "#tb 0:") {
					var num, den int
					if _, err := fmt.Sscanf(line, "#tb 0: %d/%d", &num, &den); err != nil || den <= 0 {
						t.Fatal("invalid decoded time base", line)
					}
					tick = float64(num) / float64(den)
				}
				if !strings.HasPrefix(line, "0,") {
					continue
				}
				pts, err := strconv.ParseInt(strings.TrimSpace(strings.Split(line, ",")[2]), 10, 64)
				if err != nil || pts <= previous {
					t.Fatal("recovery did not produce ordered picture timestamps", err)
				}
				previous = pts
				count++
			}
			if count != frames || frames != 80 {
				t.Fatalf("recovery lost or duplicated pictures: got %d, want %d", count, frames)
			}
			if end := float64(previous) * tick; end < 3.8 || end > 4.2 {
				t.Fatalf("recovery changed the video duration: last picture at %.3fs", end)
			}
			out, err = exec.CommandContext(ctx, path, "-hide_banner", "-loglevel", "error", "-xerror", "-i", copy.Path, "-map", "0:a:0", "-c:a", "pcm_s16le", "-ar", "48000", "-ac", "2", "-f", "s16le", "-").CombinedOutput()
			if err != nil {
				t.Fatalf("decode recovered audio: %v", err)
			}
			if seconds := float64(len(out)) / (48000 * 2 * 2); seconds < 3.9 || seconds > 4.2 {
				t.Fatalf("recovery lost audio or changed its duration: %.3fs", seconds)
			}
			if got, err := os.ReadFile(source); err != nil || !bytes.Equal(got, raw) {
				t.Fatal("recovery changed the original FLV", err)
			}
		})
	}
}

func TestMP4RecoveryRecognizesTimestampErrorsOnly(t *testing.T) {
	for _, message := range []string{
		"Application provided invalid, non monotonically increasing dts to muxer",
		"Non-monotonous DTS in output stream",
		"Non-monotonic DTS; previous: 200, current: 199",
		"Could not find tag for codec",
		"Codec not currently supported in container",
	} {
		if !canRecoverMP4(fmt.Errorf("%s", message)) {
			t.Fatal("recoverable media error was not recognized", message)
		}
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, fmt.Errorf("No space left on device"), fmt.Errorf("Unknown encoder"), fmt.Errorf("Invalid argument"), fmt.Errorf("Invalid data found when processing input")} {
		if canRecoverMP4(err) {
			t.Fatal("nonrecoverable error would be retried", err)
		}
	}
}

func TestRecoveryPreservesPicturesAcrossEarlyParameterChanges(t *testing.T) {
	path, err := ffmpeg.Find("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	var raw []byte
	for i, size := range []string{"96x64", "160x128"} {
		part := filepath.Join(dir, fmt.Sprintf("source-%d.flv", i))
		args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=" + size + ":rate=20", "-t", "2", "-c:v", "libx264", "-threads:v", "1", "-preset", "ultrafast", "-g", "20", "-bf", "3", "-x264-params", "repeat-headers=0:scenecut=0", "-flvflags", "no_duration_filesize", part}
		if out, err := exec.CommandContext(ctx, path, args...).CombinedOutput(); err != nil {
			if bytes.Contains(out, []byte("Unknown encoder")) {
				t.Skip("synthetic fixture needs libx264")
			}
			t.Fatalf("generate fixture: %v: %s", err, out)
		}
		data, err := os.ReadFile(part)
		if err != nil {
			t.Fatal(err)
		}
		offset := int(binary.BigEndian.Uint32(data[5:9])) + 4
		if i == 0 {
			raw = append(raw, data[:offset]...)
		}
		for offset < len(data) {
			h := data[offset : offset+11]
			n := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
			if h[0] == 9 {
				raw = appendFLVTag(raw, 9, flvTimestamp(h)+uint32(i*2000), data[offset+11:offset+11+n])
			}
			offset += n + 15
		}
	}
	source, output := filepath.Join(dir, "source.flv"), filepath.Join(dir, "recovered.mp4")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := recoverMP4(ctx, path, source, output); err != nil {
		t.Fatal(fault.Details(err))
	}
	if err := verifyCopy(ctx, path, output); err != nil {
		t.Fatal(fault.Details(err))
	}
	out, err := exec.CommandContext(ctx, path, "-hide_banner", "-loglevel", "error", "-xerror", "-i", output, "-map", "0:v:0", "-fps_mode", "passthrough", "-enc_time_base:v", "demux", "-f", "framemd5", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("decode recovered pictures: %v: %s", err, out)
	}
	if count := bytes.Count(out, []byte("\n0,")); count != 80 {
		t.Fatalf("parameter changes lost or duplicated pictures: got %d, want 80", count)
	}
	if got, err := os.ReadFile(source); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("parameter recovery changed the original FLV", err)
	}
}

func TestProcessingRecoversResolutionAndCodecChangesWithOverlap(t *testing.T) {
	path, err := ffmpeg.Find("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	encoders, err := exec.CommandContext(ctx, path, "-hide_banner", "-encoders").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoders, []byte(" libx264 ")) || !bytes.Contains(encoders, []byte(" libx265 ")) {
		t.Skip("synthetic fixture needs H.264 and HEVC encoders")
	}
	dir := t.TempDir()
	var raw []byte
	for i, codec := range []string{"libx265", "libx265", "libx264", "libx264"} {
		part := filepath.Join(dir, fmt.Sprintf("part-%d.flv", i))
		video := "testsrc2=size=128x96:rate=10"
		if i == 1 {
			// Change resolution inside a continuous section with audio. The
			// shared concat graph must keep its clock through the change.
			video = "color=red:size=96x96:rate=10"
		} else if i == 3 {
			// New footage starts from zero with the same codec but different
			// pictures and dimensions. It must survive alongside the replay.
			video = "color=red:size=128x96:rate=10"
		}
		args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", video, "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "2", "-c:v", codec, "-threads:v", "1", "-preset", "ultrafast", "-g", "10", "-bf", "0", "-c:a", "aac", "-flvflags", "no_duration_filesize"}
		if codec == "libx265" {
			args = append(args, "-x265-params", "pools=1:frame-threads=1:log-level=error")
		}
		if out, err := exec.CommandContext(ctx, path, append(args, part)...).CombinedOutput(); err != nil {
			t.Fatalf("generate fixture: %v: %s", err, out)
		}
		data, err := os.ReadFile(part)
		if err != nil {
			t.Fatal(err)
		}
		offset := int(binary.BigEndian.Uint32(data[5:9])) + 4
		if i == 0 {
			raw = append(raw, data[:offset]...)
		}
		if i == 1 {
			for offset < len(data) {
				header := data[offset : offset+11]
				size := int(header[1])<<16 | int(header[2])<<8 | int(header[3])
				raw = appendFLVTag(raw, header[0], flvTimestamp(header)+2000, data[offset+11:offset+11+size])
				offset += size + 15
			}
		} else {
			raw = append(raw, data[offset:]...)
		}
		if i == 2 {
			// A reconnect sends the same sequence headers and repeats footage.
			raw = append(raw, data[offset:]...)
		}
	}
	source := filepath.Join(dir, "raw.flv")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := state.Defaults()
	cfg.FFmpegPath, cfg.DeleteRawAfterProcessing = path, true
	copy, err := Process(ctx, source, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !copy.Reencoded || filepath.Ext(copy.Path) != ".mp4" {
		t.Fatal("expected automatic MP4 recovery")
	}
	out, err := exec.CommandContext(ctx, path, "-hide_banner", "-loglevel", "error", "-xerror", "-i", copy.Path, "-map", "0:v:0", "-fps_mode", "passthrough", "-f", "framemd5", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("decode MP4: %v: %s", err, out)
	}
	count := 0
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.HasPrefix(line, "0,") {
			count++
		}
	}
	if count != 80 {
		t.Fatalf("expected 80 original frames with overlap removed, got %d", count)
	}
	// Sample both borders and the centre of every frame after the picture
	// becomes narrower. A successful decode can still contain bad padding.
	const pixels = "[0:v]split=3[l][c][r];[l]crop=8:96:0:0,scale=1:1:flags=area,format=rgb24[lb];[c]crop=8:96:60:0,scale=1:1:flags=area,format=rgb24[cb];[r]crop=8:96:120:0,scale=1:1:flags=area,format=rgb24[rb];[lb][cb][rb]hstack=inputs=3"
	out, err = exec.CommandContext(ctx, path, "-hide_banner", "-loglevel", "error", "-xerror", "-ss", "2.2", "-i", copy.Path, "-t", "1.6", "-filter_complex", pixels, "-an", "-c:v", "rawvideo", "-threads:v", "1", "-f", "rawvideo", "-").Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			t.Fatalf("sample recovery frames: %v: %s", err, exit.Stderr)
		}
		t.Fatal(err)
	}
	pixelReader := bytes.NewReader(out)
	if pixelReader.Len() < 10*9 || pixelReader.Len()%9 != 0 {
		t.Fatalf("expected full RGB samples from the narrower section, got %d bytes", pixelReader.Len())
	}
	for i := 0; pixelReader.Len() > 0; i++ {
		var sample [9]byte
		if _, err := io.ReadFull(pixelReader, sample[:]); err != nil {
			t.Fatal(err)
		}
		if sample[3] < 200 || max(sample[4], sample[5]) > 16 ||
			max(sample[0], sample[1], sample[2], sample[6], sample[7], sample[8]) > 16 {
			t.Fatalf("frame %d should have a red picture with black borders, got RGB samples %v", i, sample)
		}
	}
	for _, offset := range []string{"1.5", "2.5", "4.5", "6.5"} {
		out, err := exec.CommandContext(ctx, path, "-hide_banner", "-loglevel", "error", "-xerror", "-ss", offset, "-i", copy.Path, "-map", "0:v:0", "-frames:v", "1", "-f", "framemd5", "-").CombinedOutput()
		if err != nil || !bytes.Contains(out, []byte("\n0,")) {
			t.Fatalf("seek at %s: %v: %s", offset, err, out)
		}
	}
	if got, err := os.ReadFile(source); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("recovery changed the original FLV", err)
	}
}
