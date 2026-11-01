package fault

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestDiagnosticsOmitSignedURLsAndSessionCookies(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://media.example/live.flv?token=synthetic-media-secret", Err: fmt.Errorf("proxy failed for https://proxy.example/?signature=synthetic-proxy-secret; sessionid=synthetic-cookie; sid_guard=synthetic-guard")}
	detail := Details(Wrap("could not open the stream", err))
	for _, secret := range []string{"synthetic-media-secret", "synthetic-proxy-secret", "synthetic-cookie", "synthetic-guard", "https://"} {
		if strings.Contains(detail, secret) {
			t.Fatal("diagnostics contain signed URLs or cookie values")
		}
	}
	if !strings.Contains(detail, "proxy failed") || !strings.Contains(detail, "could not open the stream") {
		t.Fatal("redaction lost the useful operation and cause", detail)
	}
}

func TestDiskFailureTakesPrecedenceOverJoinedTransportError(t *testing.T) {
	err := errors.Join(Wrap("video connection timed out", context.DeadlineExceeded), Action("raw FLV could not be synchronized to disk", errors.New("device unavailable")))
	if Reason(err) != "raw FLV could not be synchronized to disk" || !RequiresAction(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("media interruption masked a storage failure", err)
	}
}
