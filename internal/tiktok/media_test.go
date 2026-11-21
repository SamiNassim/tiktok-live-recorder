package tiktok

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"tiktok-live-recorder/internal/network"
)

func TestMediaIsByteExactAndNeverReceivesAccountCookies(t *testing.T) {
	raw := append([]byte{'F', 'L', 'V', 1, 5, 0, 0, 0, 9, 0, 0, 0, 0}, []byte("unaltered metadata and codec payload")...)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Error("account cookies leaked to CDN")
		}
		if r.URL.Path == "/bad.flv" {
			w.Write([]byte("not media"))
			return
		}
		w.Write(raw)
	}))
	defer server.Close()
	body, err := openFLV(context.Background(), server.Client(), []Source{{URL: server.URL + "/bad.flv"}, {URL: server.URL + "/raw.flv"}})
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil || !bytes.Equal(raw, got) {
		t.Fatal("FLV changed", err)
	}
}

func TestDisabledProxyRecordsOverHTTP1WhenServerOffersHTTP2(t *testing.T) {
	raw := append([]byte{'F', 'L', 'V', 1, 5, 0, 0, 0, 9, 0, 0, 0, 0}, []byte("unaltered metadata and codec payload")...)
	var proto atomic.Value
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto.Store(r.Proto)
		_, _ = w.Write(raw)
	}))
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	server.StartTLS()
	defer server.Close()

	// A saved proxy address must not be used, or negotiated, while the switch is off.
	proxy := network.Config{URL: "http://127.0.0.1:9"}
	transport, err := mediaTransport(proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	if transport.TLSClientConfig == nil || !slices.Equal(transport.TLSClientConfig.NextProtos, []string{"http/1.1"}) {
		t.Fatal("media transport still offers HTTP/2", transport.TLSClientConfig)
	}
	cfg := transport.TLSClientConfig.Clone()
	cfg.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	transport.TLSClientConfig = cfg
	if !slices.Equal(transport.TLSClientConfig.NextProtos, []string{"http/1.1"}) {
		t.Fatal("trusting the test certificate restored the HTTP/2 offer")
	}

	body, err := openFLV(t.Context(), &http.Client{Transport: network.Protect(transport, proxy)}, []Source{{URL: server.URL + "/raw.flv"}})
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil || !bytes.Equal(raw, got) || proto.Load() != "HTTP/1.1" {
		t.Fatalf("proto=%v err=%v bytes=%d", proto.Load(), err, len(got))
	}
}
