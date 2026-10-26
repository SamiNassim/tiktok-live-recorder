package tiktok

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestInactiveRoomIsNotAnAccessFailureAndNeedsNoFallback(t *testing.T) {
	calls := 0
	status := 3
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !strings.Contains(r.URL.Path, "api-live") {
			t.Error("a recognized room state triggered a fallback request")
		}
		json.NewEncoder(w).Encode(webFixture(t, status))
	})
	for _, next := range []int{2, 3, 3, 2, 4} {
		status = next
		s, err := c.Check(context.Background(), "demo_creator")
		if err != nil || s.Live != (next == 2) || (next != 2 && len(s.Sources) != 0) {
			t.Fatal("room transition was misclassified", next, s.Live, err)
		}
	}
	if calls != 5 {
		t.Fatal("unexpected extra discovery requests", calls)
	}
}

func TestFallbackRecognizesInactiveAndFinishedRooms(t *testing.T) {
	for _, body := range []string{
		`{"status_code":0,"data":{"status":3}}`,
		`{"status_code":30003,"data":{"message":"room has finished","prompts":[]}}`,
	} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "api-live") {
				w.Write([]byte(`{}`))
				return
			}
			w.Write([]byte(body))
		})
		s, err := c.Check(context.Background(), "demo_creator")
		if err != nil || s.Live || len(s.Sources) != 0 {
			t.Fatal("finished room became an API failure", s.Live, err)
		}
	}
}

func TestFinishedFallbackDoesNotOverrideKnownLivePresence(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "api-live") {
			w.Write([]byte(`{"statusCode":0,"data":{"liveRoom":{"status":2}}}`))
			return
		}
		w.Write([]byte(`{"status_code":30003}`))
	})
	s, err := c.Check(context.Background(), "demo_creator")
	if !s.Live || !errors.Is(err, ErrUnavailable) {
		t.Fatal("contradictory fallback incorrectly marked a live room offline", s.Live, err)
	}
}

func TestFinishedFallbackDoesNotHidePrimaryAccessFailure(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "api-live") {
			w.Write([]byte(`{"statusCode":4003110}`))
			return
		}
		w.Write([]byte(`{"status_code":30003}`))
	})
	_, err := c.Check(context.Background(), "demo_creator")
	if !errors.Is(err, ErrAccess) {
		t.Fatal("inactive fallback hid an explicit access failure", err)
	}
}

func TestUnknownRoomStatesRemainErrorsWithNumericDiagnostics(t *testing.T) {
	for _, body := range []string{
		`{"statusCode":0,"data":{"liveRoom":{"status":99}}}`,
		`{"statusCode":99999}`,
	} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
		_, err := c.Check(context.Background(), "demo_creator")
		if !errors.Is(err, ErrBlocked) || !strings.Contains(err.Error(), "99") || strings.Contains(err.Error(), "access may") {
			t.Fatal("unknown response lost its diagnostic code or implied a block", err)
		}
	}
}
