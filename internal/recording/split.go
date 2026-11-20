package recording

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"tiktok-live-recorder/internal/fault"
)

// Clip reports a durable completed part and its already-open successor.
type Clip struct {
	Result
	StartedAt     time.Time
	NextPath      string
	NextStartedAt time.Time
}

// TakeClips drains completed parts without blocking the incoming media stream.
func (h *Handle) TakeClips() []Clip {
	h.mu.Lock()
	defer h.mu.Unlock()
	clips := h.clips
	h.clips = nil
	return clips
}

type splitCapture struct {
	handle  *Handle
	writer  *syncWriter
	path    string
	started time.Time
	part    int
}

func (c *splitCapture) finish(copyErr error) Result {
	syncErr, closeErr := c.writer.file.Sync(), c.writer.file.Close()
	transportEnded := c.writer.count > 0 && errors.Is(copyErr, io.ErrUnexpectedEOF) && !c.writer.failed && syncErr == nil && closeErr == nil
	if copyErr != nil && !c.writer.failed && !fault.RequiresAction(copyErr) {
		copyErr = fault.Wrap("the video connection was interrupted: "+fault.Reason(copyErr), copyErr)
	}
	if syncErr != nil {
		syncErr = fault.Action("could not synchronize the raw FLV to disk; check disk space and drive connectivity", syncErr)
	}
	if closeErr != nil {
		closeErr = fault.Action("could not close the raw FLV file safely; check the recording drive", closeErr)
	}
	err := errors.Join(copyErr, syncErr, closeErr)
	if c.writer.count == 0 && err == nil {
		err = fault.Wrap("the video connection ended before any FLV data was received", nil)
	}
	return Result{Path: c.path, Bytes: c.writer.count, EndedAt: time.Now(), Err: err, TransportEnded: transportEnded}
}

func (c *splitCapture) rotate(prefix []byte) error {
	path := fmt.Sprintf("%s_part%03d.flv", strings.TrimSuffix(c.handle.path, "_part001.flv"), c.part+1)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fault.Action("could not create the next raw FLV clip; check disk space and folder permissions", err)
	}
	writer := &syncWriter{file: file, last: time.Now()}
	if _, err = writer.Write(prefix); err != nil {
		_ = file.Close()
		return err
	}
	result := c.finish(nil)
	started := time.Now()
	clip := Clip{Result: result, StartedAt: c.started, NextPath: path, NextStartedAt: started}
	c.path, c.started, c.writer = path, started, writer
	c.part++
	c.handle.mu.Lock()
	c.handle.clips = append(c.handle.clips, clip)
	c.handle.mu.Unlock()
	return result.Err
}

// copyFLV cuts only between complete source tags, at video keyframes (or audio
// packets for audio-only streams). It repeats original initialization bytes in
// each clip; tag payloads, metadata, and timestamps are never rewritten.
func (c *splitCapture) copyFLV(source io.Reader, interval time.Duration, limit int64) error {
	header := make([]byte, 9)
	n, err := io.ReadFull(source, header)
	if err != nil {
		_, writeErr := c.writer.Write(header[:n])
		return errors.Join(err, writeErr)
	}
	offset := int(binary.BigEndian.Uint32(header[5:9]))
	if string(header[:3]) != "FLV" || header[3] != 1 || offset < 9 || offset > 64<<10 {
		// Preserve unsupported input verbatim rather than guessing tag boundaries.
		if _, err := c.writer.Write(header); err != nil {
			return err
		}
		_, err := io.Copy(c.writer, source)
		return err
	}
	header = append(header, make([]byte, offset-9+4)...)
	n, err = io.ReadFull(source, header[9:])
	if _, writeErr := c.writer.Write(header[:9+n]); err != nil || writeErr != nil {
		return errors.Join(err, writeErr)
	}
	video := header[4]&1 != 0
	var metadata, audioHeader, videoHeader []byte
	var elapsed, clipStart time.Duration
	var lastStamp uint32
	clock, media := false, false
	var tag []byte
	for {
		tag = append(tag[:0], make([]byte, 11)...)
		n, err := io.ReadFull(source, tag)
		if err != nil {
			_, writeErr := c.writer.Write(tag[:n])
			if errors.Is(err, io.EOF) && n == 0 {
				err = nil
			}
			return errors.Join(err, writeErr)
		}
		size := int(tag[1])<<16 | int(tag[2])<<8 | int(tag[3])
		tag = append(tag, make([]byte, size+4)...)
		n, err = io.ReadFull(source, tag[11:])
		if err != nil {
			_, writeErr := c.writer.Write(tag[:11+n])
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return errors.Join(err, writeErr)
		}
		payload := tag[11 : 11+size]
		boundary, packet := false, false
		switch tag[0] {
		case 18:
			metadata = append(metadata[:0], tag...)
		case 9:
			video = true
			codec, kind, _, key := videoPacket(payload)
			if codec != "" && kind == 0 {
				videoHeader = append(videoHeader[:0], tag...)
			} else {
				packet = len(payload) > 0 && (codec == "" || kind == 1 || kind == 3)
				boundary = packet && (key || codec == "" && payload[0]>>4 == 1)
			}
		case 8:
			if len(payload) >= 2 && payload[0]>>4 == 10 && payload[1] == 0 {
				audioHeader = append(audioHeader[:0], tag...)
			} else {
				packet = len(payload) > 0
				boundary = packet && !video
			}
		}
		if packet && (tag[0] == 9 || !video) {
			stamp := flvTimestamp(tag)
			if clock {
				// Unsigned deltas cover wraparound; backwards encoder resets do not
				// create a huge duration or change any bytes in the saved recording.
				if delta := stamp - lastStamp; delta < 1<<31 {
					elapsed += time.Duration(delta) * time.Millisecond
				}
			}
			lastStamp, clock = stamp, true
		}
		due := interval > 0 && elapsed-clipStart >= interval || limit > 0 && c.writer.count+int64(len(tag)) > limit
		var rotateErr error
		if media && boundary && due {
			prefix := append([]byte(nil), header...)
			prefix = append(prefix, metadata...)
			prefix = append(prefix, audioHeader...)
			prefix = append(prefix, videoHeader...)
			rotateErr = c.rotate(prefix)
			clipStart, media = elapsed, false
		}
		// Even if rotation fails, preserve the packet already read from the wire.
		if _, err := c.writer.Write(tag); err != nil {
			return errors.Join(rotateErr, err)
		}
		if rotateErr != nil {
			return rotateErr
		}
		media = media || packet && (tag[0] == 9 || !video)
	}
}
