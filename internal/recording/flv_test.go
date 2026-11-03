package recording

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"tiktok-live-recorder/internal/ffmpeg"
	"tiktok-live-recorder/internal/state"
)

func appendFLVTag(dst []byte, kind byte, stamp uint32, payload []byte) []byte {
	size := len(payload)
	dst = append(dst, kind, byte(size>>16), byte(size>>8), byte(size), byte(stamp>>16), byte(stamp>>8), byte(stamp), byte(stamp>>24), 0, 0, 0)
	dst = append(dst, payload...)
	return binary.BigEndian.AppendUint32(dst, uint32(size+11))
}

func TestProcessingInputPreservesHeadersMetadataAudioAndPartialTail(t *testing.T) {
	full := []byte{'F', 'L', 'V', 1, 5, 0, 0, 0, 12, 1, 2, 3, 0, 0, 0, 0}
	full = appendFLVTag(full, 18, 0, []byte("synthetic metadata"))
	full = appendFLVTag(full, 8, 1234, []byte{0xaf, 1, 2, 3, 4})
	full = appendFLVTag(full, 9, 1234, []byte{0x12, 2, 3, 4, 5}) // Unrelated video codec.
	for _, trim := range []int{0, 1, 8, 20} {
		raw := full[:len(full)-trim]
		path := filepath.Join(t.TempDir(), "source.flv")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		input, err := processingInput(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(input)
		input.Close()
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatal("processing discarded unrelated bytes or a partial final tag", trim, err)
		}
	}
}

func TestProcessingInputUsesCurrentParametersAndNormalizesNALLengths(t *testing.T) {
	// Small synthetic configuration records exercise framing, not a decoder.
	for _, enhanced := range []bool{false, true} {
		for _, codec := range []string{"h264", "hevc"} {
			t.Run(fmt.Sprintf("%s/enhanced=%v", codec, enhanced), func(t *testing.T) {
				config := func(width byte, parameter byte) []byte {
					if codec == "h264" {
						return []byte{1, 66, 0, 30, 0xfc | (width - 1), 0xe1, 0, 1, parameter, 1, 0, 1, 0x68}
					}
					b := make([]byte, 23)
					b[0], b[21], b[22] = 1, 0xfc|(width-1), 1
					return append(b, 0xa1, 0, 1, 0, 1, parameter)
				}
				header := func(packet byte, key bool) []byte {
					frame := byte(0x20)
					if key {
						frame = 0x10
					}
					if enhanced {
						fourcc := "avc1"
						if codec == "hevc" {
							fourcc = "hvc1"
						}
						b := append([]byte{0x80 | frame | packet}, fourcc...)
						if packet == 1 {
							b = append(b, 0, 0, 3)
						}
						return b
					}
					id := byte(7)
					if codec == "hevc" {
						id = 12
					}
					return []byte{frame | id, packet, 0, 0, 3}
				}
				var video flvVideo
				for index, width := range []byte{2, 1, 4} {
					parameter := byte(0x67 + index)
					cfg := append(header(0, true), config(width, parameter)...)
					out, keep, err := video.appendTag(nil, cfg)
					if err != nil || keep != (index == 0) {
						t.Fatal("sequence header was not handled", index, keep, err)
					}
					if index == 0 && out[len(header(0, true))+map[string]int{"h264": 4, "hevc": 21}[codec]]&3 != 3 {
						t.Fatal("initial configuration does not use four-byte NAL lengths")
					}
					for frame, key := range []bool{false, true, false} {
						for packetIndex, packet := range []byte{1, 3} {
							if packet == 3 && !enhanced {
								continue
							}
							h := header(packet, key)
							data := append(slices.Clone(h), make([]byte, width)...)
							data[len(data)-1] = 2
							data = append(data, 0x65, 0x42)
							out, keep, err := video.appendTag(nil, data)
							want := slices.Clone(h)
							if key || (frame == 0 && packetIndex == 0) {
								want = append(want, 0, 0, 0, 1, parameter)
								if codec == "h264" {
									want = append(want, 0, 0, 0, 1, 0x68)
								}
							}
							want = append(want, 0, 0, 0, 2, 0x65, 0x42)
							if err != nil || !keep || !bytes.Equal(out, want) {
								t.Fatalf("lost current parameters, composition offset, or encoded NAL: %x != %x (%v)", out, want, err)
							}
						}
					}
				}
			})
		}
	}
}

func TestProcessingInputRejectsIncompleteVideoConfiguration(t *testing.T) {
	for _, codec := range []string{"h264", "hevc"} {
		for size := 0; size < 40; size++ {
			_, _, err := videoParameters(codec, append([]byte{1}, make([]byte, size)...))
			if err == nil && size < map[string]int{"h264": 6, "hevc": 22}[codec] {
				t.Fatal("accepted truncated decoder configuration", codec, size)
			}
		}
	}
}

func FuzzProcessingVideoTags(f *testing.F) {
	f.Add([]byte{0x17, 0, 0, 0, 0, 1, 66, 0, 30, 0xff, 0xe1, 0, 1, 0x67, 1, 0, 1, 0x68})
	f.Add([]byte{0x17, 1, 0, 0, 0, 0, 0, 0, 2, 0x65, 0x42})
	f.Add([]byte{0x90, 'h', 'v', 'c', '1', 1})
	f.Fuzz(func(t *testing.T, tag []byte) {
		if len(tag) > 64<<10 {
			t.Skip()
		}
		var video flvVideo
		_, _, _ = video.appendTag(nil, tag)
		// Also exercise malformed coded packets after a valid sequence header.
		video = flvVideo{codec: "h264", lengthSize: 4, parameters: []byte{0, 0, 0, 1, 0x67}}
		_, _, _ = video.appendTag(nil, tag)
	})
}

func TestProcessingPreservesResolutionChanges(t *testing.T) {
	path, err := ffmpeg.Find("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	encoders, err := exec.CommandContext(ctx, path, "-hide_banner", "-encoders").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, codec := range []string{"h264", "hevc"} {
		t.Run(codec, func(t *testing.T) {
			encoder := map[string]string{"h264": "libx264", "hevc": "libx265"}[codec]
			if !strings.Contains(string(encoders), " "+encoder+" ") {
				t.Skip("synthetic fixture encoder is not installed")
			}
			dir := t.TempDir()
			var raw []byte
			for index, size := range []string{"96x64", "160x128", "128x96"} {
				part := filepath.Join(dir, fmt.Sprintf("part-%d.flv", index))
				duration := "2"
				if index == 0 {
					duration = "6" // Change after FFmpeg's initial stream probe.
				}
				args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-n", "-f", "lavfi", "-i", "testsrc2=size=" + size + ":rate=5", "-t", duration, "-c:v", encoder, "-threads", "1", "-preset", "ultrafast", "-g", "5", "-bf", "0", "-pix_fmt", "yuv420p"}
				if codec == "h264" {
					args = append(args, "-x264-params", "repeat-headers=0:scenecut=0")
				} else {
					args = append(args, "-x265-params", "pools=1:frame-threads=1:repeat-headers=0:scenecut=0:log-level=error")
				}
				args = append(args, "-flvflags", "no_duration_filesize", "-f", "flv", part)
				if out, err := exec.CommandContext(ctx, path, args...).CombinedOutput(); err != nil {
					t.Fatalf("create synthetic video: %v: %s", err, out)
				}
				data, err := os.ReadFile(part)
				if err != nil {
					t.Fatal(err)
				}
				offset := int(binary.BigEndian.Uint32(data[5:9])) + 4
				if index == 0 {
					raw = append(raw, data[:offset]...)
				}
				shift := uint32(0)
				if index > 0 {
					shift = uint32(6000 + (index-1)*2000)
				}
				for offset < len(data) {
					h := data[offset : offset+11]
					n := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
					payload := data[offset+11 : offset+11+n]
					if h[0] == 9 || index == 0 {
						stamp := uint32(h[4])<<16 | uint32(h[5])<<8 | uint32(h[6]) | uint32(h[7])<<24
						raw = appendFLVTag(raw, h[0], stamp+shift, payload)
					}
					offset += n + 15
				}
			}
			source := filepath.Join(dir, "changing.flv")
			if err := os.WriteFile(source, raw, 0600); err != nil {
				t.Fatal(err)
			}
			// Decode the original sequentially; direct raw seeks can skip its
			// sequence headers. Compare pixel hashes independently of timestamps.
			frames := func(t *testing.T, input string) []string {
				t.Helper()
				out, err := exec.CommandContext(ctx, path, "-hide_banner", "-nostdin", "-loglevel", "error", "-xerror", "-i", input, "-map", "0:v:0", "-an", "-vf", "scale=96:64", "-fps_mode", "passthrough", "-f", "framemd5", "-").CombinedOutput()
				if err != nil {
					t.Fatalf("decode synthetic video: %v: %s", err, out)
				}
				var hashes []string
				for line := range strings.SplitSeq(string(out), "\n") {
					if strings.HasPrefix(line, "0,") {
						fields := strings.Split(line, ",")
						hashes = append(hashes, strings.TrimSpace(fields[len(fields)-1]))
					}
				}
				return hashes
			}
			want := frames(t, source)
			if len(want) != 50 {
				t.Fatal("fixture did not contain every expected frame", len(want))
			}
			for _, extension := range []string{"mkv", "mp4", "ts"} {
				t.Run(extension, func(t *testing.T) {
					cfg := state.Defaults()
					cfg.FFmpegPath, cfg.OutputExtension = path, extension
					cfg.DeleteRawAfterProcessing = true
					cfg.FFmpegArgs += " -metadata title=Example -bsf:v null"
					output, err := Process(ctx, source, cfg)
					if err != nil {
						t.Fatal(err)
					}
					if got := frames(t, output); !slices.Equal(got, want) {
						t.Fatal("processed frames changed or disappeared", len(got), len(want))
					}
					if extension == "ts" {
						return // MPEG-TS has no keyframe seek index.
					}
					for _, stamp := range []string{"6.4", "8.4"} {
						out, err := exec.CommandContext(ctx, path, "-hide_banner", "-nostdin", "-loglevel", "error", "-xerror", "-ss", stamp, "-i", output, "-t", "0.4", "-map", "0:v:0", "-an", "-f", "framemd5", "-").CombinedOutput()
						if err != nil || !bytes.Contains(out, []byte("0,")) {
							t.Fatalf("seek skipped current decoder settings: %v: %s", err, out)
						}
					}
				})
			}
			t.Run("custom encoding", func(t *testing.T) {
				if !strings.Contains(string(encoders), " libx264 ") {
					t.Skip("synthetic H.264 encoder is not installed")
				}
				input := filepath.Join(dir, "custom.flv")
				if err := os.WriteFile(input, raw, 0600); err != nil {
					t.Fatal(err)
				}
				cfg := state.Defaults()
				cfg.FFmpegPath, cfg.DeleteRawAfterProcessing = path, true
				cfg.FFmpegArgs = "-map 0:v:0 -c:v libx264 -preset ultrafast -crf 18 -vf scale=96:64"
				output, err := Process(ctx, input, cfg)
				if err != nil {
					t.Fatal(err)
				}
				if got := frames(t, output); len(got) != len(want) {
					t.Fatal("custom encoder lost frames", len(got), len(want))
				}
			})
			if codec == "h264" {
				t.Run("cleanup rejects broken decoder configuration", func(t *testing.T) {
					input := filepath.Join(dir, "unsafe-copy.flv")
					if err := os.WriteFile(input, raw, 0600); err != nil {
						t.Fatal(err)
					}
					cfg := state.Defaults()
					cfg.FFmpegPath, cfg.DeleteRawAfterProcessing = path, true
					cfg.FFmpegArgs = "-map 0:v:0 -c copy -bsf:v filter_units=remove_types=7|8"
					output, err := Process(ctx, input, cfg)
					if err == nil || output != "" || !strings.Contains(err.Error(), "verify the processed copy") {
						t.Fatal("broken video was authorized for raw cleanup", output, err)
					}
					if _, err := os.Stat(strings.TrimSuffix(input, ".flv") + ".processed.mkv"); !os.IsNotExist(err) {
						t.Fatal("published an unverified processed copy", err)
					}
					if got, err := os.ReadFile(input); err != nil || !bytes.Equal(got, raw) {
						t.Fatal("failed verification changed the original FLV", err)
					}
				})
			}
			after, err := os.ReadFile(source)
			if err != nil || sha256.Sum256(after) != sha256.Sum256(raw) {
				t.Fatal("processing changed the original FLV", err)
			}
		})
	}
}
