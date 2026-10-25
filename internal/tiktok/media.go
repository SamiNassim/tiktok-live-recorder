package tiktok

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// OpenFLV validates the header, then returns every original byte, including
// that header. No tags, timestamps, metadata, or codec packets are rewritten.
func OpenFLV(ctx context.Context, sources []Source) (io.ReadCloser, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 15 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return readDeadlineConn{conn}, nil
		},
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Scheme != "https" {
			return errors.New("unexpected media redirect")
		}
		return nil
	}}
	reader, err := openFLV(ctx, client, sources)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	return &mediaBody{Reader: reader, close: func() error { err := reader.Close(); transport.CloseIdleConnections(); return err }}, nil
}

func openFLV(ctx context.Context, client *http.Client, sources []Source) (io.ReadCloser, error) {
	var last error
	for _, source := range sources {
		if !validMediaURL(source.URL) {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", UserAgent)
		req.Header.Set("Referer", Origin+"/")
		// Do not copy the account session or API cookie jar to media hosts.
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = errors.New("could not connect to the TikTok video CDN")
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			last = fmt.Errorf("video CDN returned HTTP %d", resp.StatusCode)
			continue
		}
		prefix := make([]byte, 9)
		_, err = io.ReadFull(resp.Body, prefix)
		if err == nil && string(prefix[:3]) == "FLV" && prefix[3] == 1 {
			offset := int(binary.BigEndian.Uint32(prefix[5:9]))
			if offset >= 9 && offset <= 64<<10 {
				rest := make([]byte, offset-9+4)
				if _, err = io.ReadFull(resp.Body, rest); err == nil {
					return &mediaBody{Reader: io.MultiReader(bytes.NewReader(prefix), bytes.NewReader(rest), resp.Body), close: resp.Body.Close}, nil
				}
			}
		}
		resp.Body.Close()
		last = errors.New("video CDN returned no valid FLV header; trying another rendition")
	}
	if last == nil {
		last = ErrUnavailable
	}
	return nil, last
}

type mediaBody struct {
	io.Reader
	close func() error
}

func (b *mediaBody) Close() error { return b.close() }

type readDeadlineConn struct{ net.Conn }

func (c readDeadlineConn) Read(p []byte) (int, error) {
	_ = c.SetReadDeadline(time.Now().Add(25 * time.Second))
	return c.Conn.Read(p)
}
