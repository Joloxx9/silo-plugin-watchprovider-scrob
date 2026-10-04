package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// How a status is classified decides whether Silo retries the sync, stops the
// connection for bad credentials, or drops the event. Getting one wrong either
// hammers Scrob or silently loses a write.
func TestFaultClassificationPerStatus(t *testing.T) {
	cases := []struct {
		status int
		want   pluginv1.WatchSyncFaultCode
	}{
		{http.StatusUnauthorized, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL},
		{http.StatusForbidden, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL},
		{http.StatusTooManyRequests, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED},
		{http.StatusNotFound, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST},
		{http.StatusBadRequest, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST},
		{http.StatusConflict, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST},
		{http.StatusUnprocessableEntity, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST},
	}
	for _, tc := range cases {
		resp := &http.Response{StatusCode: tc.status, Header: http.Header{}}
		if got := faultForHTTPResponse(resp).GetCode(); got != tc.want {
			t.Errorf("HTTP %d = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// A server-side failure is temporary so the host retries it; a client-side one
// that is not a known rejection is permanent and must not be retried.
//
// Note that a temporary fault is deliberately not connection-wide: each event
// fails on its own and the batch continues. Only an invalid credential or a
// rate limit stops the whole call.
func TestServerErrorsRetryAndOtherClientErrorsDoNot(t *testing.T) {
	temporary := faultForHTTPResponse(&http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}})
	if temporary.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY {
		t.Errorf("HTTP 502 = %v, want TEMPORARY", temporary.GetCode())
	}
	if connectionWide(temporary) {
		t.Errorf("a temporary fault must not abort the batch today; update this test if that changes")
	}
	timeout := faultForHTTPResponse(&http.Response{StatusCode: http.StatusRequestTimeout, Header: http.Header{}})
	if timeout.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY {
		t.Errorf("HTTP 408 = %v, want TEMPORARY_FAILURE", timeout.GetCode())
	}
	permanent := faultForHTTPResponse(&http.Response{StatusCode: http.StatusTeapot, Header: http.Header{}})
	if permanent.GetCode() == pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY {
		t.Errorf("an unknown 4xx must not be retried forever")
	}
}

// Scrob's Retry-After is honoured in both forms HTTP allows, and a value that
// is neither is simply absent rather than an error.
func TestRetryAfterAcceptsSecondsAndDates(t *testing.T) {
	if got := retryAfter("30"); got != 30*time.Second {
		t.Errorf("retryAfter(\"30\") = %v, want 30s", got)
	}
	if got := retryAfter(" 5 "); got != 5*time.Second {
		t.Errorf("retryAfter with padding = %v, want 5s", got)
	}
	at := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	if got := retryAfter(at); got < 60*time.Second || got > 95*time.Second {
		t.Errorf("retryAfter(date) = %v, want roughly 90s", got)
	}
	for _, bad := range []string{"", "soon", "-5", "0"} {
		if got := retryAfter(bad); got != 0 {
			t.Errorf("retryAfter(%q) = %v, want 0", bad, got)
		}
	}
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if got := retryAfter(past); got != 0 {
		t.Errorf("a Retry-After in the past = %v, want 0", got)
	}
}

// A rate limit carries its delay to the host, which is the whole point of
// reporting it rather than failing.
func TestRateLimitCarriesRetryAfter(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	resp.Header.Set("Retry-After", "42")
	fault := faultForHTTPResponse(resp)
	if fault.GetRetryAfter().AsDuration() != 42*time.Second {
		t.Errorf("RetryAfter = %v, want 42s", fault.GetRetryAfter().AsDuration())
	}
}

// Credentials round-trip through the host unchanged, including the server URL
// that lives in the secret attributes.
func TestRefreshCredentialsReturnsTheStoredConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	s := NewServer(server.Client())
	ctx := authContext(server.URL)
	resp, err := s.RefreshCredentials(context.Background(), &pluginv1.WatchSyncRefreshCredentialsRequest{Context: ctx})
	if err != nil {
		t.Fatalf("RefreshCredentials() error: %v", err)
	}
	if resp.GetFault() != nil {
		t.Fatalf("fault: %v", resp.GetFault())
	}
	if got := resp.GetCredentials().GetAccessToken(); got != "key" {
		t.Errorf("AccessToken = %q, want the stored key", got)
	}
	if got := resp.GetCredentials().GetSecretAttributes()[configBaseURL]; got != server.URL {
		t.Errorf("server URL = %q, want %q", got, server.URL)
	}
}
