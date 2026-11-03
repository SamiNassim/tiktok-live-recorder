package recording

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
)

// processingInput supplies a derived FLV stream to FFmpeg. Live encoders can
// replace their sequence headers without starting a new HTTP response. Carry
// those parameter sets in keyframes so stream-copy outputs also survive seeks.
// The source file is opened read-only; recording still saves original bytes.
func processingInput(ctx context.Context, path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	var header [9]byte
	n, err := io.ReadFull(f, header[:])
	offset := int64(binary.BigEndian.Uint32(header[5:]))
	if err != nil || string(header[:3]) != "FLV" || header[3] != 1 || offset < 9 || offset > 64<<10 {
		// Leave invalid/unsupported inputs to FFmpeg's normal diagnostics.
		return struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(header[:n]), f), f}, nil
	}
	prefix := make([]byte, offset+4)
	copy(prefix, header[:])
	n, err = io.ReadFull(f, prefix[9:])
	if err != nil {
		return struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(prefix[:9+n]), f), f}, nil
	}
	return &processingFLV{ctx: ctx, source: f, pending: prefix}, nil
}

type processingFLV struct {
	ctx     context.Context
	source  *os.File
	pending []byte
	input   []byte
	output  []byte
	video   flvVideo
	err     error
}

func (r *processingFLV) Close() error { return r.source.Close() }

func (r *processingFLV) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		if r.err != nil {
			return 0, r.err
		}
		if err := r.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *processingFLV) next() error {
	var header [11]byte
	n, err := io.ReadFull(r.source, header[:])
	if err != nil {
		// Keep a partial tail visible to FFmpeg, including its strict cleanup
		// verification. Never silently discard bytes from an interrupted tag.
		r.pending, r.err = append(r.output[:0], header[:n]...), err
		if errors.Is(err, io.ErrUnexpectedEOF) {
			r.err = io.EOF
		}
		if n == 0 {
			return err
		}
		return nil
	}
	size := int(header[1])<<16 | int(header[2])<<8 | int(header[3])
	r.input = append(r.input[:0], header[:]...)
	r.input = append(r.input, make([]byte, size+4)...)
	n, err = io.ReadFull(r.source, r.input[11:])
	if err != nil {
		r.pending, r.err = r.input[:11+n], err
		if errors.Is(err, io.ErrUnexpectedEOF) {
			r.err = io.EOF
		}
		return nil
	}
	if header[0] != 9 {
		r.pending = r.input
		return nil
	}
	r.output = append(r.output[:0], header[:]...)
	payload, keep, err := r.video.appendTag(r.output, r.input[11:11+size])
	if err != nil {
		return err
	}
	if !keep {
		return nil
	}
	size = len(payload) - len(header)
	if size > 0xffffff {
		return errors.New("video parameter sets exceed the FLV tag size limit")
	}
	payload[1], payload[2], payload[3] = byte(size>>16), byte(size>>8), byte(size)
	r.output = binary.BigEndian.AppendUint32(payload, uint32(size+11))
	r.pending = r.output
	return nil
}

type flvVideo struct {
	codec             string
	lengthSize        int
	parameters        []byte // Annex B parameter sets for FFmpeg's processing input.
	pendingParameters bool
}

func (v *flvVideo) appendTag(dst, tag []byte) ([]byte, bool, error) {
	if len(tag) < 5 {
		return append(dst, tag...), true, nil
	}
	codec, packet, offset, key := "", int(tag[1]), 5, tag[0]>>4 == 1
	if tag[0]&0x80 != 0 { // Enhanced FLV uses FourCC instead of a codec nibble.
		packet, key = int(tag[0]&15), tag[0]>>4&7 == 1
		switch string(tag[1:5]) {
		case "avc1":
			codec = "h264"
		case "hvc1", "hev1":
			codec = "hevc"
		}
		if packet == 1 { // CodedFrames carries a three-byte composition offset.
			offset = 8
		}
	} else {
		switch tag[0] & 15 {
		case 7:
			codec = "h264"
		case 12:
			codec = "hevc"
		}
	}
	if codec == "" || (packet != 0 && packet != 1 && packet != 3) {
		return append(dst, tag...), true, nil
	}
	if v.codec != "" && v.codec != codec {
		return nil, false, errors.New("the raw FLV changes video codec; keep the original recording")
	}
	if packet == 0 {
		lengthOffset, arrays, err := videoParameters(codec, tag[offset:])
		if err != nil {
			return nil, false, err
		}
		first := v.codec == ""
		v.codec, v.lengthSize, v.parameters = codec, int(tag[offset+lengthOffset]&3)+1, arrays
		v.pendingParameters = true
		if !first {
			// Repeated global headers can replace a muxer's initial settings.
			// Their current contents are preserved in subsequent keyframes.
			return dst, false, nil
		}
		// The private FFmpeg pipe carries Annex B headers and packets. Older
		// HEVC transport filters otherwise insert stale initial parameter sets
		// immediately before a new-resolution picture's slice data.
		dst = append(dst, tag[:offset]...)
		dst = append(dst, v.parameters...)
		return dst, true, nil
	}
	if v.codec == "" || len(tag) < offset {
		return nil, false, errors.New("the raw FLV lacks a complete video sequence header")
	}
	dst = append(dst, tag[:offset]...)
	if key || v.pendingParameters {
		dst = append(dst, v.parameters...)
	}
	for data := tag[offset:]; len(data) > 0; {
		if len(data) < v.lengthSize {
			return nil, false, errors.New("the raw FLV contains a partial video NAL length")
		}
		var size uint32
		for _, b := range data[:v.lengthSize] {
			size = size<<8 | uint32(b)
		}
		data = data[v.lengthSize:]
		if uint64(size) > uint64(len(data)) {
			return nil, false, errors.New("the raw FLV contains a partial video NAL unit")
		}
		dst = append(dst, 0, 0, 0, 1)
		dst = append(dst, data[:size]...)
		data = data[size:]
	}
	v.pendingParameters = false
	return dst, true, nil
}

// videoParameters reads AVCDecoderConfigurationRecord or
// HEVCDecoderConfigurationRecord. Payloads remain encoded and are not decoded.
func videoParameters(codec string, config []byte) (int, []byte, error) {
	var parameters []byte
	readArrays := func(data []byte, count int) ([]byte, error) {
		for range count {
			if len(data) < 2 {
				return nil, io.ErrUnexpectedEOF
			}
			size := int(binary.BigEndian.Uint16(data))
			data = data[2:]
			if size == 0 || size > len(data) {
				return nil, io.ErrUnexpectedEOF
			}
			parameters = append(parameters, 0, 0, 0, 1)
			parameters = append(parameters, data[:size]...)
			data = data[size:]
		}
		return data, nil
	}
	if len(config) == 0 || config[0] != 1 {
		return 0, nil, errors.New("invalid FLV video sequence header")
	}
	if codec == "h264" {
		if len(config) < 7 {
			return 0, nil, io.ErrUnexpectedEOF
		}
		data, err := readArrays(config[6:], int(config[5]&31))
		if err != nil || len(data) == 0 {
			return 0, nil, io.ErrUnexpectedEOF
		}
		data, err = readArrays(data[1:], int(data[0]))
		if err != nil {
			return 0, nil, err
		}
		if len(data) >= 4 && config[1] != 66 && config[1] != 77 && config[1] != 88 {
			if _, err := readArrays(data[4:], int(data[3])); err != nil {
				return 0, nil, err
			}
		}
		return 4, parameters, nil
	}
	if len(config) < 23 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	data := config[23:]
	for range int(config[22]) {
		if len(data) < 3 {
			return 0, nil, io.ErrUnexpectedEOF
		}
		var err error
		data, err = readArrays(data[3:], int(binary.BigEndian.Uint16(data[1:3])))
		if err != nil {
			return 0, nil, err
		}
	}
	return 21, parameters, nil
}
