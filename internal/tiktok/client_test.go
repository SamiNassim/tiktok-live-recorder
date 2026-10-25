package tiktok

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIDs(t *testing.T) {
	for _, raw := range []string{"demo_creator", "@Demo_Creator", "https://www.tiktok.com/@demo_creator/live?lang=en", "https://tiktok.com/@demo_creator", "tiktok.com/@demo_creator"} {
		if id, err := ParseID(raw); err != nil || id != "demo_creator" {
			t.Fatal(raw, id, err)
		}
	}
	for _, raw := range []string{"", "https://evil.invalid/@demo_creator", "https://tiktok.com.evil.invalid/@demo_creator", "https://www.tiktok.com/@demo_creator/video/123", "https://user@tiktok.com/@demo_creator", "@bad..name", "@bad.", "@spaces here"} {
		if _, err := ParseID(raw); err == nil {
			t.Fatal("accepted", raw)
		}
	}
}

func TestCookieParsing(t *testing.T) {
	for _, raw := range []string{`Cookie: sessionid=synthetic; tt-target-idc=testdc`, `{"tt-target-idc":"testdc","sessionid":"synthetic"}`} {
		got, err := ParseCookie(raw)
		if err != nil || got != "sessionid=synthetic; tt-target-idc=testdc" {
			t.Fatal(got, err)
		}
	}
	for _, raw := range []string{"token-only", "sessionid=synthetic", "tt-target-idc=testdc", "sessionid=x; tt-target-idc=test\nInjected: yes"} {
		if _, err := ParseCookie(raw); err == nil {
			t.Fatal("accepted invalid cookie")
		}
	}
}

func sdkFixture(t *testing.T) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"data": map[string]any{
		"origin": map[string]any{"main": map[string]string{"flv": "https://media.example.invalid/source.flv?sign=synthetic", "sdk_params": `{"VCodec":"h264","resolution":"1920x1080","vbitrate":4000000}`}},
		"hd":     map[string]any{"main": map[string]string{"flv": "https://media.example.invalid/hd.flv", "sdk_params": `{"VCodec":"h264","resolution":"1280x720","vbitrate":1800000}`}},
		"ao":     map[string]any{"main": map[string]string{"flv": "https://media.example.invalid/audio.flv"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func webFixture(t *testing.T, status int) map[string]any {
	t.Helper()
	return map[string]any{"statusCode": 0, "data": map[string]any{"user": map[string]any{"roomId": "7000000000000000001", "uniqueId": "demo_creator", "nickname": "Demo Creator"}, "liveRoom": map[string]any{"status": status, "streamData": map[string]any{"pull_data": map[string]string{"stream_data": sdkFixture(t)}}}}}
}

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c := NewClient()
	c.base = server.URL
	c.webcast = server.URL
	c.http = server.Client()
	c.Configure("", 0)
	return c
}

func TestSingleRequestDiscoveryAndOriginalQuality(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("uniqueId") != "demo_creator" || r.Header.Get("User-Agent") != UserAgent {
			t.Error("incorrect request identity")
		}
		json.NewEncoder(w).Encode(webFixture(t, 2))
	})
	s, err := c.Check(context.Background(), "@demo_creator")
	if err != nil || !s.Live || s.RoomID != "7000000000000000001" || len(s.Sources) != 2 || s.Sources[0].Quality != "origin" || s.Sources[0].Width != 1920 || calls.Load() != 1 {
		t.Fatal(s, err, calls.Load())
	}
}

func TestOfflineAndErrorsAreDistinct(t *testing.T) {
	for _, test := range []struct {
		name, body string
		want       error
	}{
		{"not found", `{"statusCode":19881007,"message":"user_not_found"}`, ErrNotFound},
		{"missing schema", `{}`, ErrBlocked},
		{"login", `{"statusCode":4003110}`, ErrAccess},
		{"HTML", `<html>challenge</html>`, ErrBlocked},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(test.body)) })
			_, err := c.Check(context.Background(), "demo_creator")
			if !errors.Is(err, test.want) {
				t.Fatal(err)
			}
		})
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(webFixture(t, 4)) })
	s, err := c.Check(context.Background(), "demo_creator")
	if err != nil || s.Live || len(s.Sources) > 0 {
		t.Fatal(s, err)
	}
}

func TestFallbackAndKnownLiveWithoutPlayback(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "api-live") {
			w.Write([]byte(`{"statusCode":0,"data":{"liveRoom":{"status":2},"user":{"roomId":"7000000000000000001"}}}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"status_code": 0, "data": map[string]any{"status": 2, "id_str": "7000000000000000001", "stream_url": map[string]any{"flv_pull_url": map[string]string{"HD1": "https://media.example.invalid/live.flv"}}}})
	})
	s, err := c.Check(context.Background(), "demo_creator")
	if err != nil || !s.Live || len(s.Sources) != 1 {
		t.Fatal(s, err)
	}
	c.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAccountValidationUsesCookiesAndChecksExpiry(t *testing.T) {
	valid := true
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Cookie"), "sessionid=synthetic") {
			t.Error("cookie missing")
		}
		if valid {
			w.Write([]byte(`{"message":"success","data":{"user_id_str":"7000000000000000002","username":"demo_viewer","screen_name":"Demo Viewer"}}`))
		} else {
			w.Write([]byte(`{"message":"error","data":{"error_code":13}}`))
		}
	})
	c.Configure("sessionid=synthetic; tt-target-idc=testdc", 0)
	a, err := c.Account(context.Background())
	if err != nil || a.Username != "demo_viewer" {
		t.Fatal(a, err)
	}
	valid = false
	if _, err = c.Account(context.Background()); err == nil {
		t.Fatal("accepted expired session")
	}
}

func TestCanceledRateWaitAndRedactedTransportErrors(t *testing.T) {
	c := NewClient()
	c.next = time.Now().Add(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Check(ctx, "demo_creator"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	c.next = time.Time{}
	c.Configure("sessionid=synthetic", 0)
	c.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("https://secret.invalid/?token=synthetic-secret")
	})}
	_, err := c.Check(context.Background(), "demo_creator")
	if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatal(err)
	}
}
