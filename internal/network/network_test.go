package network

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tiktok-live-recorder/internal/fault"
)

func TestProxyValidation(t *testing.T) {
	for _, address := range []string{"http://127.0.0.1:8080", "https://proxy.invalid:443", "socks5://proxy.invalid:1080", "socks5h://[::1]:1080"} {
		if err := (Config{Enabled: true, URL: address, Username: "synthetic-user", Password: "synthetic-password"}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, cfg := range []Config{
		{Enabled: true}, {Enabled: true, URL: "ftp://proxy.invalid:21"}, {Enabled: true, URL: "http://proxy.invalid"},
		{Enabled: true, URL: "http://proxy.invalid:0"}, {Enabled: true, URL: "http://proxy.invalid:65536"},
		{Enabled: true, URL: "http://user:secret@proxy.invalid:80"}, {Enabled: true, URL: "http://proxy.invalid:80/path"},
		{Enabled: true, URL: "http://proxy.invalid:80?secret"}, {Enabled: true, URL: "http://proxy.invalid:80", Password: "synthetic"},
		{Enabled: true, URL: "http://proxy.invalid:80", Username: "bad:name"}, {Enabled: true, URL: "socks5://proxy.invalid:80", Username: strings.Repeat("x", 256)},
		{Enabled: true, URL: "http://proxy.invalid:80", Username: "bad\r\nname"},
	} {
		if err := cfg.Validate(); err == nil {
			t.Fatal("accepted invalid proxy configuration")
		}
	}
	if err := (Config{URL: "invalid disabled draft"}).Validate(); err != nil {
		t.Fatal("disabled proxy was validated", err)
	}
}

func TestHTTPAndHTTPSProxyTunnelAuthenticationAndCookieIsolation(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[secure], func(t *testing.T) {
			target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Proxy-Authorization") != "" {
					t.Error("proxy credentials leaked to destination")
				}
				if r.Header.Get("Cookie") != "sessionid=synthetic" {
					t.Error("destination cookie missing")
				}
				_, _ = io.WriteString(w, "synthetic response")
			}))
			defer target.Close()
			var connects atomic.Int64
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "CONNECT" {
					t.Error("expected HTTPS tunnel")
					w.WriteHeader(400)
					return
				}
				connects.Add(1)
				if r.Header.Get("Cookie") != "" {
					t.Error("account cookie sent to proxy CONNECT")
				}
				want := "Basic " + base64.StdEncoding.EncodeToString([]byte("synthetic-user:synthetic-password"))
				if r.Header.Get("Proxy-Authorization") != want {
					t.Error("proxy authentication missing")
					w.WriteHeader(407)
					return
				}
				upstream, err := net.DialTimeout("tcp", strings.TrimPrefix(target.URL, "https://"), time.Second)
				if err != nil {
					t.Error(err)
					w.WriteHeader(502)
					return
				}
				client, buffer, err := w.(http.Hijacker).Hijack()
				if err != nil {
					upstream.Close()
					t.Error(err)
					return
				}
				_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
				_ = buffer.Flush()
				go func() { _, _ = io.Copy(upstream, buffer); _ = upstream.Close() }()
				_, _ = io.Copy(client, upstream)
				_ = client.Close()
				_ = upstream.Close()
			})
			proxy := httptest.NewUnstartedServer(handler)
			if secure {
				proxy.StartTLS()
			} else {
				proxy.Start()
			}
			defer proxy.Close()
			cfg := Config{Enabled: true, URL: proxy.URL, Username: "synthetic-user", Password: "synthetic-password"}
			transport, err := NewTransport(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer transport.CloseIdleConnections()
			roots := x509.NewCertPool()
			roots.AddCert(target.Certificate())
			if secure {
				roots.AddCert(proxy.Certificate())
			}
			transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
			client := &http.Client{Transport: Protect(transport, cfg), Timeout: 5 * time.Second}
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, target.URL, nil)
			req.Header.Set("Cookie", "sessionid=synthetic")
			response, err := client.Do(req)
			if err != nil {
				t.Fatal(fault.Details(err))
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || string(body) != "synthetic response" || connects.Load() != 1 {
				t.Fatal("request did not traverse the explicit proxy", err)
			}
		})
	}
}

func TestSOCKS5RemoteDNSAndAuthentication(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		run := func() error {
			head := make([]byte, 2)
			if _, err := io.ReadFull(conn, head); err != nil {
				return err
			}
			methods := make([]byte, int(head[1]))
			if _, err := io.ReadFull(conn, methods); err != nil {
				return err
			}
			if _, err := conn.Write([]byte{5, 2}); err != nil {
				return err
			}
			if _, err := io.ReadFull(conn, head); err != nil {
				return err
			}
			user := make([]byte, int(head[1]))
			if _, err := io.ReadFull(conn, user); err != nil {
				return err
			}
			size := []byte{0}
			if _, err := io.ReadFull(conn, size); err != nil {
				return err
			}
			password := make([]byte, int(size[0]))
			if _, err := io.ReadFull(conn, password); err != nil {
				return err
			}
			if string(user) != "synthetic-user" || string(password) != "synthetic-password" {
				return errors.New("wrong SOCKS credentials")
			}
			if _, err := conn.Write([]byte{1, 0}); err != nil {
				return err
			}
			connect := make([]byte, 5)
			if _, err := io.ReadFull(conn, connect); err != nil {
				return err
			}
			if connect[0] != 5 || connect[1] != 1 || connect[3] != 3 {
				return errors.New("SOCKS did not request remote DNS")
			}
			host := make([]byte, int(connect[4])+2)
			if _, err := io.ReadFull(conn, host); err != nil {
				return err
			}
			if string(host[:len(host)-2]) != "media.invalid" {
				return errors.New("wrong remote DNS destination")
			}
			if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
				return err
			}
			req, err := http.ReadRequest(bufio.NewReader(conn))
			if err != nil {
				return err
			}
			defer req.Body.Close()
			if req.Header.Get("Proxy-Authorization") != "" {
				return errors.New("SOCKS credentials leaked into HTTP request")
			}
			_, err = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 9\r\nConnection: close\r\n\r\nsynthetic")
			return err
		}
		done <- run()
	}()
	cfg := Config{Enabled: true, URL: "socks5://" + listener.Addr().String(), Username: "synthetic-user", Password: "synthetic-password"}
	transport, err := NewTransport(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: Protect(transport, cfg), Timeout: 5 * time.Second}
	response, err := client.Get("http://media.invalid:8080/raw.flv")
	if err != nil {
		t.Fatal(fault.Details(err))
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(data) != "synthetic" {
		t.Fatal("SOCKS request failed", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestProxyFailureNeverFallsBackOrExposesDetails(t *testing.T) {
	var direct atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { direct.Add(1) }))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(407) }))
	proxy.Close()
	cfg := Config{Enabled: true, URL: proxy.URL, Username: "synthetic-user", Password: "synthetic-password"}
	transport, err := NewTransport(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: Protect(transport, cfg), Timeout: time.Second}
	_, err = client.Get(target.URL)
	if err == nil || direct.Load() != 0 {
		t.Fatal("failed proxy bypassed", err)
	}
	details := fault.Details(err)
	for _, secret := range []string{cfg.URL, cfg.Username, cfg.Password} {
		if strings.Contains(details, secret) {
			t.Fatal("proxy details exposed")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
	_, err = client.Do(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("proxy obscured cancellation", err)
	}
}
