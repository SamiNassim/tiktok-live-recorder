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
	"strings"
	"testing"
	"time"

	"tiktok-live-recorder/internal/ffmpeg"
	"tiktok-live-recorder/internal/state"
)

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
