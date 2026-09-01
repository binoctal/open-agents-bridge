package bridge

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/config"
)

// Registration handshake bridge-side coverage (task 3.5): report assembly,
// the three-state probe classification, bounded resend, mismatch handling,
// and the no-API environment (probe unreachable must not panic or block).

func TestBuildCapabilityReportPinsTheWireFields(t *testing.T) {
	report := buildCapabilityReport("0.6.2", probeUnreachable, "dial timeout", false)

	want := map[string]interface{}{
		"version":       "0.6.2",
		"callbackProbe": "unreachable",
		"e2ee":          false,
		"detail":        "dial timeout",
	}
	if len(report) != len(want) {
		t.Fatalf("report has %d fields, want %d: %+v", len(report), len(want), report)
	}
	for k, v := range want {
		if report[k] != v {
			t.Errorf("report[%q] = %v, want %v", k, report[k], v)
		}
	}
}

func TestClassifyCallbackProbe(t *testing.T) {
	client := &http.Client{Timeout: 2 * time.Second}

	t.Run("2xx is ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				t.Errorf("missing bearer token, got %q", r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		result, _ := classifyCallbackProbe(client, srv.URL, "tok")
		if result != probeOK {
			t.Errorf("result = %q, want %q", result, probeOK)
		}
	})

	t.Run("401 is reachable but auth failed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		result, detail := classifyCallbackProbe(client, srv.URL, "tok")
		if result != probeReachableAuthFail {
			t.Errorf("result = %q, want %q", result, probeReachableAuthFail)
		}
		if detail == "" {
			t.Error("auth-failed detail must carry the HTTP status for the mismatch event")
		}
	})

	t.Run("transport error is unreachable", func(t *testing.T) {
		// A closed server: connection refused, not an HTTP status.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		srv.Close()

		result, _ := classifyCallbackProbe(client, srv.URL, "tok")
		if result != probeUnreachable {
			t.Errorf("result = %q, want %q", result, probeUnreachable)
		}
	})

	t.Run("non-auth error status is still ok", func(t *testing.T) {
		// A 500 proves reachability and token acceptance; failing the
		// probe on it would flap whenever the route has a bad day.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		result, _ := classifyCallbackProbe(client, srv.URL, "tok")
		if result != probeOK {
			t.Errorf("result = %q, want %q", result, probeOK)
		}
	})
}

func TestDeliverCapabilityReportRetriesThenGivesUp(t *testing.T) {
	b := &Bridge{config: &config.Config{}, done: make(chan struct{})}

	calls := 0
	send := func(Message) error {
		calls++
		return errors.New("write failed")
	}

	start := time.Now()
	b.deliverCapabilityReport(buildCapabilityReport("dev", probeOK, "", true), probeOK, send)
	elapsed := time.Since(start)

	if calls != capabilityReportRetries {
		t.Errorf("send called %d times, want %d", calls, capabilityReportRetries)
	}
	// Retries must back off (2s + 4s) — proving the loop waits rather
	// than hot-spinning the failures.
	if elapsed < 5*time.Second {
		t.Errorf("retry loop returned after %v, want >= 5s of backoff", elapsed)
	}
}

func TestDeliverCapabilityReportSucceedsOnRetry(t *testing.T) {
	b := &Bridge{config: &config.Config{}, done: make(chan struct{})}

	calls := 0
	var gotType string
	send := func(msg Message) error {
		calls++
		if calls < 2 {
			return errors.New("write failed")
		}
		gotType = msg.Type
		return nil
	}

	done := make(chan struct{})
	go func() {
		// First backoff is 2s; success lands right after it.
		b.deliverCapabilityReport(buildCapabilityReport("dev", probeOK, "", true), probeOK, send)
		close(done)
	}()

	select {
	case <-done:
		if calls != 2 {
			t.Errorf("send called %d times, want 2", calls)
		}
		if gotType != "bridge:capability_report" {
			t.Errorf("message type = %q, want bridge:capability_report", gotType)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("deliverCapabilityReport did not succeed after retry")
	}
}

func TestSendCapabilityReportNoAPIDoesNotPanic(t *testing.T) {
	// No API, no connection: the probe classifies unreachable and the
	// send lands in the offline buffer (nil conn is not an error path).
	// The whole report must complete without panicking or hanging.
	b := &Bridge{config: &config.Config{ServerURL: "ws://127.0.0.1:1", DeviceToken: "tok"}, done: make(chan struct{})}

	done := make(chan struct{})
	go func() {
		b.sendCapabilityReport()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * probeTimeout):
		t.Fatal("sendCapabilityReport hung without an API")
	}

	b.offlineMu.Lock()
	defer b.offlineMu.Unlock()
	if len(b.offlineBuf) != 1 {
		t.Fatalf("offline buffer has %d messages, want 1", len(b.offlineBuf))
	}
	msg := b.offlineBuf[0]
	if msg.Type != "bridge:capability_report" {
		t.Errorf("buffered type = %q, want bridge:capability_report", msg.Type)
	}
	if payload, ok := msg.Payload.(map[string]interface{}); ok {
		if payload["callbackProbe"] != probeUnreachable {
			t.Errorf("callbackProbe = %v, want %q", payload["callbackProbe"], probeUnreachable)
		}
		if payload["e2ee"] != false {
			t.Errorf("e2ee = %v, want false (no keys loaded)", payload["e2ee"])
		}
	} else {
		t.Errorf("payload is %T, want map[string]interface{}", msg.Payload)
	}
}

func TestHandleHandshakeMismatchDoesNotPanic(t *testing.T) {
	// notify-send is typically absent in CI; Send's error is swallowed
	// by design, so the assertion is purely "survives the verdict".
	b := &Bridge{config: &config.Config{}, done: make(chan struct{})}

	b.handleHandshakeMismatch(Message{
		Type: "handshake:mismatch",
		Payload: map[string]interface{}{
			"mismatches": []interface{}{
				map[string]interface{}{"reason": "callback_auth_failed", "detail": "HTTP 401"},
			},
		},
		Timestamp: time.Now().UnixMilli(),
	})

	// A payload of the wrong shape must not panic either.
	b.handleHandshakeMismatch(Message{Type: "handshake:mismatch", Payload: "garbage"})
}
