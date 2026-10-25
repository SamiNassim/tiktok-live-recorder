package tiktok

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
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
