package tiktok

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tiktok-live-recorder/internal/fault"
)

func TestQueueTimeoutIdentifiesUnsentRequest(t *testing.T) {
	c := NewClient()
	var calls atomic.Int32
	c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, io.EOF
	})
	c.next = time.Now().Add(time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := c.Check(ctx, "demo_creator")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "request queue wait timed out") || calls.Load() != 0 {
		t.Fatal("queue wait was confused with an API request", err, calls.Load())
	}
}

func TestUnreadableResponseRetainsCauseWithoutLoggingBody(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "not JSON: synthetic-response-secret") })
	_, err := c.Check(context.Background(), "demo_creator")
	if !errors.Is(err, ErrBlocked) || !strings.Contains(err.Error(), "unreadable response") || strings.Contains(fault.Details(err), "synthetic-response-secret") {
		t.Fatal("response error lost its context or retained the response body", err)
	}
}

func TestTransportDiagnosticsRetainTimeoutWithoutSignedURL(t *testing.T) {
	c := NewClient()
	c.Configure("sessionid=synthetic", 0)
	c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })
	_, err := c.Check(context.Background(), "demo_creator")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "TikTok did not answer") || !strings.Contains(fault.Details(err), "context deadline exceeded") || strings.Contains(fault.Details(err), "https://") {
		t.Fatal("network timeout was discarded or exposed a request URL", err)
	}
}
