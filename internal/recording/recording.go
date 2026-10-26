// Package recording saves original FLV bytes. FFmpeg is used only for an
// optional separate processed copy after the raw file has been synchronized.
package recording

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"tiktok-live-recorder/internal/state"
	"tiktok-live-recorder/internal/tiktok"
)

type Result struct {
	Path    string
	Bytes   int64
	EndedAt time.Time
	Err     error
	// TransportEnded distinguishes an HTTP body closing early from disk errors.
	TransportEnded bool
}
type Handle struct {
	path    string
	started time.Time
	cancel  context.CancelFunc
	done    chan struct{}
	result  Result
}

func (h *Handle) Stop()                 { h.cancel() }
func (h *Handle) Done() <-chan struct{} { return h.done }
func (h *Handle) Result() Result        { <-h.done; return h.result }
func (h *Handle) File() string          { return h.path }
func (h *Handle) Started() time.Time    { return h.started }

func Start(parent context.Context, s state.Streamer, source tiktok.Stream, cfg state.Config) (*Handle, error) {
	ctx, cancel := context.WithCancel(parent)
	body, err := tiktok.OpenFLV(ctx, source.Sources)
	if err != nil {
		cancel()
		return nil, err
	}
	h, err := start(ctx, cancel, s, cfg, body)
	if err != nil {
		body.Close()
		cancel()
	}
	return h, err
}

func start(ctx context.Context, cancel context.CancelFunc, s state.Streamer, cfg state.Config, body io.ReadCloser) (*Handle, error) {
	stamp := time.Now()
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, err
	}
	path := recordingPath(s, cfg, stamp, hex.EncodeToString(suffix[:]))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create recording folder: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("create raw FLV: %w", err)
	}
	h := &Handle{path: path, started: stamp, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		defer cancel()
		defer body.Close()
		// Closing the body unblocks stalled reads immediately on pause/quit.
		stopClose := context.AfterFunc(ctx, func() { body.Close() })
		defer stopClose()
		writer := &syncWriter{file: file, last: time.Now()}
		count, copyErr := io.CopyBuffer(writer, body, make([]byte, 128<<10))
		if ctx.Err() != nil && !writer.failed {
			copyErr = nil
		}
		syncErr, closeErr := file.Sync(), file.Close()
		transportEnded := count > 0 && errors.Is(copyErr, io.ErrUnexpectedEOF) && !writer.failed && syncErr == nil && closeErr == nil
		err := errors.Join(copyErr, syncErr, closeErr)
		if count == 0 && err == nil {
			err = errors.New("no FLV bytes were received")
		}
		r := Result{Path: path, Bytes: count, EndedAt: time.Now(), Err: err, TransportEnded: transportEnded}
		h.result = r
	}()
	return h, nil
}

func recordingPath(s state.Streamer, cfg state.Config, stamp time.Time, suffix string) string {
	id := safeName("@" + strings.TrimPrefix(s.ID, "@"))
	dir := cfg.RecordingPath
	if cfg.FolderPerStreamer {
		dir = filepath.Join(dir, id+"_"+safeName(s.DisplayName()))
	}
	return filepath.Join(dir, id+"_"+stamp.Format("2006-01-02_15-04-05")+"_"+suffix+".flv")
}

type syncWriter struct {
	file   *os.File
	last   time.Time
	failed bool
}

func (w *syncWriter) Write(p []byte) (int, error) {
	n, err := w.file.Write(p)
	if err == nil && time.Since(w.last) >= time.Second {
		err = w.file.Sync()
		w.last = time.Now()
	}
	if err != nil {
		w.failed = true
	}
	return n, err
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			b.WriteByte('_')
		} else {
			b.WriteRune(r)
		}
		if b.Len() >= 90 {
			break
		}
	}
	v := strings.Trim(b.String(), " .")
	if v == "" {
		v = "stream"
	}
	base := strings.ToUpper(strings.SplitN(v, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || (len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
		v = "_" + v
	}
	return v
}
