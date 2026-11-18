package recording

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type flvTimestampRewind struct {
	kind          byte
	before, after uint32
}

type flvMediaClock struct{ audio, video uint32 }

func (c *flvMediaClock) advance(kind byte, stamp uint32) error {
	last := &c.video
	if kind == 8 {
		last = &c.audio
	}
	if stamp < *last {
		return &flvTimestampRewind{kind, *last, stamp}
	}
	*last = stamp
	return nil
}

func isFLVMedia(kind byte, payload []byte) bool {
	switch kind {
	case 8:
		return len(payload) > 1 && (payload[0]>>4 != 10 || payload[1] == 1)
	case 9:
		codec, packet, _, _ := videoPacket(payload)
		return codec != "" && (packet == 1 || packet == 3)
	default:
		return false
	}
}

func (e *flvTimestampRewind) Error() string {
	return fmt.Sprintf("FLV tag type %d moved backwards from %d to %d ms", e.kind, e.before, e.after)
}

// pictureDigest ignores non-picture NAL units such as SEI messages, which a
// reconnect can regenerate even when it sends the same encoded picture.
func pictureDigest(data []byte, codec string, lengthSize int) ([sha256.Size]byte, error) {
	hash := sha256.New()
	picture := false
	for len(data) != 0 {
		if len(data) < lengthSize || lengthSize < 1 {
			return [sha256.Size]byte{}, errors.New("incomplete NAL length in replayed FLV keyframe")
		}
		var size uint32
		for _, b := range data[:lengthSize] {
			size = size<<8 | uint32(b)
		}
		length := data[:lengthSize]
		data = data[lengthSize:]
		if size == 0 || uint64(size) > uint64(len(data)) {
			return [sha256.Size]byte{}, errors.New("incomplete NAL in replayed FLV keyframe")
		}
		nal := data[:size]
		if codec == "h264" && nal[0]&31 >= 1 && nal[0]&31 <= 5 || codec == "hevc" && (nal[0]>>1)&63 < 32 {
			hash.Write(length)
			hash.Write(nal)
			picture = true
		}
		data = data[size:]
	}
	if !picture {
		return [sha256.Size]byte{}, errors.New("FLV keyframe contains no coded picture")
	}
	var digest [sha256.Size]byte
	hash.Sum(digest[:0])
	return digest, nil
}

// splitFLV writes temporary FLV inputs with their own headers. Timestamps are
// kept within each section. FFmpeg rebases each input as a unit, preserving
// the relative timing of its audio and video.
func splitFLV(ctx context.Context, source, dir string) ([]flvSection, error) {
	f, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	in := bufio.NewReaderSize(f, 128<<10)
	var header [9]byte
	if _, err := io.ReadFull(in, header[:]); err != nil {
		return nil, err
	}
	offset := int(binary.BigEndian.Uint32(header[5:]))
	if string(header[:3]) != "FLV" || header[3] != 1 || offset < 9 || offset > 64<<10 {
		return nil, errors.New("invalid FLV header")
	}
	prefix := make([]byte, offset+4)
	copy(prefix, header[:])
	if _, err := io.ReadFull(in, prefix[9:]); err != nil {
		return nil, err
	}

	var sections []flvSection
	var out *os.File
	var section flvSection
	var audioHeader, videoHeader, heldAudio []byte
	var heldAudioStamp uint32
	var codec string
	var lengthSize int
	type keyframe struct {
		stamp  uint32
		digest [sha256.Size]byte
	}
	var recentKeys [16]keyframe
	nextKey := 0
	var lastVideo, lastAudio uint32
	var hasVideoTime, hasAudioTime bool
	var audioRestart bool
	var replayVideoUntil, replayAudioUntil uint32
	var replayVideo, replayAudio bool
	closeSection := func() error {
		if out == nil {
			return nil
		}
		err := out.Close()
		out = nil
		if section.audio || section.video {
			sections = append(sections, section)
		} else {
			err = errors.Join(err, os.Remove(section.name))
		}
		return err
	}
	defer func() {
		if out != nil {
			out.Close()
		}
	}()
	openSection := func() error {
		section = flvSection{name: filepath.Join(dir, fmt.Sprintf("part-%d.flv", len(sections)))}
		out, err = os.OpenFile(section.name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = out.Write(bytes.Join([][]byte{prefix, audioHeader, videoHeader}, nil))
		return err
	}

	var tag []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var h [11]byte
		if _, err := io.ReadFull(in, h[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, err
		}
		size := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
		tag = append(tag[:0], h[:]...)
		tag = append(tag, make([]byte, size+4)...)
		if n, err := io.ReadFull(in, tag[11:]); err != nil {
			// A transport interruption can leave a final partial packet. Only
			// complete tags go into the derived inputs, and the original stays
			// available after recovery for any future repair attempt.
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				if n < size {
					break
				}
				// The media payload is complete when only PreviousTagSize is
				// missing. Rebuild that framing in the temporary input so a
				// fully received final picture or audio packet also survives.
				binary.BigEndian.PutUint32(tag[len(tag)-4:], uint32(size+11))
			} else {
				return nil, err
			}
		}
		payload := tag[11 : 11+size]
		stamp := flvTimestamp(h[:])
		switch h[0] {
		case 9:
			current, packet, offset, key := videoPacket(payload)
			if current == "" {
				return nil, errors.New("unsupported video tag while rebuilding FLV")
			}
			if packet == 0 {
				if codec != "" && current != codec {
					if err := closeSection(); err != nil {
						return nil, err
					}
					hasVideoTime, hasAudioTime = false, false
					replayVideo, replayAudio = false, false
					audioRestart = false
					recentKeys = [16]keyframe{}
				}
				config := payload[offset:]
				lengthOffset := 4
				if current == "hevc" {
					lengthOffset = 21
				}
				if len(config) <= lengthOffset || config[0] != 1 {
					return nil, errors.New("invalid FLV sequence header while rebuilding MP4")
				}
				lengthSize = int(config[lengthOffset]&3) + 1
				codec = current
				videoHeader = append(videoHeader[:0], tag...)
				if out != nil && !replayVideo {
					// Later configuration records remain visible to the decoder.
					if _, err := out.Write(tag); err != nil {
						return nil, err
					}
				}
				continue
			}
			if packet != 1 && packet != 3 {
				continue
			}
			if current != codec || len(videoHeader) == 0 {
				return nil, errors.New("video codec changed without a new FLV sequence header")
			}
			var digest [sha256.Size]byte
			if key {
				if len(payload) < offset {
					return nil, errors.New("incomplete FLV video packet")
				}
				digest, err = pictureDigest(payload[offset:], codec, lengthSize)
				if err != nil {
					return nil, err
				}
			}
			// Equal decode times can occur on distinct interframes. Keep them
			// in the same decoder section; recovery repairs presentation times
			// after decoding. An equal keyframe may still be replayed footage.
			if hasVideoTime && (stamp < lastVideo || stamp == lastVideo && key) && (!replayVideo || key) {
				wasReplay := replayVideo
				if !key {
					return nil, errors.New("FLV video timestamps reset outside a keyframe")
				}
				replayVideo = false
				for _, previous := range recentKeys {
					if previous.stamp == stamp && previous.digest == digest {
						replayVideo, replayVideoUntil = true, lastVideo
						replayAudio, replayAudioUntil = hasAudioTime, lastAudio
						break
					}
				}
				if !replayVideo {
					// A reset can also start a new broadcast interval. Retain
					// its footage when the first picture differs from the old one.
					if err := closeSection(); err != nil {
						return nil, err
					}
					hasAudioTime, replayAudio = false, false
					recentKeys = [16]keyframe{}
				} else if !wasReplay {
					heldAudio = nil
				}
				audioRestart = false
			}
			if replayVideo {
				if stamp <= replayVideoUntil || !key {
					continue
				}
				if err := closeSection(); err != nil {
					return nil, err
				}
				replayVideo = false
			}
			if out == nil {
				if !key {
					return nil, errors.New("FLV video section has no initial keyframe")
				}
				if err := openSection(); err != nil {
					return nil, err
				}
				if _, err := out.Write(heldAudio); err != nil {
					return nil, err
				}
				section.audio = len(heldAudio) != 0
				if section.audio {
					// Restore the audio clock for packets queued before this keyframe.
					hasAudioTime = true
					lastAudio = heldAudioStamp
				}
				heldAudio = nil
			}
			if key {
				recentKeys[nextKey] = keyframe{stamp, digest}
				nextKey = (nextKey + 1) % len(recentKeys)
			}
			hasVideoTime, lastVideo, section.video = true, stamp, true
		case 8:
			if len(payload) < 2 {
				return nil, errors.New("incomplete FLV audio packet")
			}
			if payload[0]>>4 == 10 && payload[1] == 0 {
				audioHeader = append(audioHeader[:0], tag...)
				if out != nil && !replayAudio {
					if _, err := out.Write(tag); err != nil {
						return nil, err
					}
				}
				continue
			}
			if hasAudioTime && stamp < lastAudio && !replayAudio {
				if len(videoHeader) == 0 {
					if err := closeSection(); err != nil {
						return nil, err
					}
				} else {
					audioRestart = true
				}
			}
			if audioRestart {
				if len(heldAudio)+len(tag) > 8<<20 {
					return nil, errors.New("FLV audio timestamps reset without a new video boundary")
				}
				heldAudio = append(heldAudio, tag...)
				heldAudioStamp = stamp
				continue
			}
			if replayAudio && stamp <= replayAudioUntil {
				continue
			}
			replayAudio = false
			hasAudioTime, lastAudio = true, stamp
			if (out == nil || replayVideo) && len(videoHeader) != 0 {
				if len(heldAudio)+len(tag) > 8<<20 {
					return nil, errors.New("too much audio before the first video frame")
				}
				heldAudio = append(heldAudio, tag...)
				heldAudioStamp = stamp
				continue
			}
			if out == nil {
				if err := openSection(); err != nil {
					return nil, err
				}
			}
			section.audio = true
		default:
			continue
		}
		if _, err := out.Write(tag); err != nil {
			return nil, err
		}
	}
	if audioRestart {
		return nil, errors.New("FLV audio timestamps reset without a new video boundary")
	}
	if err := closeSection(); err != nil {
		return nil, err
	}
	if len(sections) == 0 {
		return nil, errors.New("FLV contains no complete audio or video sections")
	}
	return sections, nil
}

func flvTimestamp(header []byte) uint32 {
	return uint32(header[7])<<24 | uint32(header[4])<<16 | uint32(header[5])<<8 | uint32(header[6])
}
