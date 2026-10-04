package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// authContext is a connection the way the host presents one: the API key in
// the access token and the server URL in the secret attributes the host
// persists for a plugin-sourced connection.
func authContext(baseURL string) *pluginv1.WatchSyncAuthenticatedContext {
	return &pluginv1.WatchSyncAuthenticatedContext{
		CapabilityId: capabilityID,
		Credentials:  credentials("key", baseURL),
	}
}

func rpcServer(t *testing.T, handler http.HandlerFunc) (*Server, string, func()) {
	t.Helper()
	server := httptest.NewServer(handler)
	return NewServer(server.Client()), server.URL, server.Close
}

func TestListRemoteStateRoutesEachFamilyToItsEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind pluginv1.WatchSyncRemoteStateKind
		path string
	}{
		{"watched", pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED, "/api/proxy/history"},
		{"progress", pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_PROGRESS, "/api/proxy/history/continue-watching"},
		{"rating", pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_RATING, "/api/proxy/ratings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked string
			s, url, done := rpcServer(t, func(w http.ResponseWriter, r *http.Request) {
				asked = r.URL.Path
				_, _ = w.Write([]byte(`{"results":[],"continue_watching":[],"total_pages":1}`))
			})
			defer done()

			resp, err := s.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{
				Context:    authContext(url),
				StateKinds: []pluginv1.WatchSyncRemoteStateKind{tc.kind},
			})
			if err != nil {
				t.Fatalf("ListRemoteState() error: %v", err)
			}
			if resp.GetFault() != nil {
				t.Fatalf("fault: %v", resp.GetFault())
			}
			if asked != tc.path {
				t.Errorf("asked %q, want %q", asked, tc.path)
			}
		})
	}
}

// Families Scrob has no source for must fault rather than answer an empty
// snapshot, which the host would read as "everything was removed upstream".
func TestListRemoteStateRefusesUnsupportedFamilies(t *testing.T) {
	s, url, done := rpcServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	})
	defer done()

	for _, kind := range []pluginv1.WatchSyncRemoteStateKind{
		pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_FAVORITE,
		pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST,
	} {
		resp, err := s.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{
			Context:    authContext(url),
			StateKinds: []pluginv1.WatchSyncRemoteStateKind{kind},
		})
		if err != nil {
			t.Fatalf("ListRemoteState() error: %v", err)
		}
		if resp.GetFault() == nil {
			t.Errorf("kind %v: want a fault, got none", kind)
		}
		if len(resp.GetItems()) != 0 {
			t.Errorf("kind %v: returned items alongside a fault", kind)
		}
	}
}

func TestListRemoteStateRejectsAMissingAPIKey(t *testing.T) {
	s, url, done := rpcServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	})
	defer done()

	resp, err := s.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{
		Context: &pluginv1.WatchSyncAuthenticatedContext{
			CapabilityId: capabilityID,
			Credentials:  credentials("", url),
		},
		StateKinds: []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_RATING},
	})
	if err != nil {
		t.Fatalf("ListRemoteState() error: %v", err)
	}
	if resp.GetFault() == nil {
		t.Fatalf("want a fault for a missing API key")
	}
}

// A plugin serves one capability; anything else is a host-side mix-up and must
// not reach Scrob with this connection's key.
func TestRPCsRejectAnotherCapability(t *testing.T) {
	s, url, done := rpcServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	})
	defer done()

	ctx := authContext(url)
	ctx.CapabilityId = "somebody-else"
	resp, err := s.GetAccount(context.Background(), &pluginv1.WatchSyncGetAccountRequest{Context: ctx})
	if err != nil {
		t.Fatalf("GetAccount() error: %v", err)
	}
	if resp.GetFault() == nil {
		t.Fatalf("want a fault for a foreign capability id")
	}
}

// ApplyEvents groups consecutive events by operation. Each group must reach the
// handler for its own operation, and an unsupported one is rejected per event
// rather than failing the whole batch.
func TestApplyEventsDispatchesEachOperation(t *testing.T) {
	var paths []string
	s, url, done := rpcServer(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{}`))
	})
	defer done()

	movie := func() *pluginv1.WatchSyncMedia {
		return &pluginv1.WatchSyncMedia{
			MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
			ExternalIds: map[string]string{"tmdb": "42"},
		}
	}
	resp, err := s.ApplyEvents(context.Background(), &pluginv1.WatchSyncApplyEventsRequest{
		Context: authContext(url),
		Events: []*pluginv1.WatchSyncEvent{
			{EventId: "a", Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED, Media: movie()},
			{EventId: "b", Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SET_RATING, Rating: 8, Media: movie()},
			{EventId: "c", Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_ADD_FAVORITE, Media: movie()},
		},
	})
	if err != nil {
		t.Fatalf("ApplyEvents() error: %v", err)
	}
	if resp.GetFault() != nil {
		t.Fatalf("fault: %v", resp.GetFault())
	}
	if len(resp.GetResults()) != 3 {
		t.Fatalf("results = %d, want one per event", len(resp.GetResults()))
	}
	byID := map[string]pluginv1.WatchSyncApplyStatus{}
	for _, r := range resp.GetResults() {
		byID[r.GetEventId()] = r.GetStatus()
	}
	if byID["c"] != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Errorf("favorite event = %v, want REJECTED", byID["c"])
	}
	if len(paths) < 2 {
		t.Errorf("requests = %v, want the watched and rating writes", paths)
	}
}

// An event with no id cannot be reported on by the host, so it is rejected
// before anything is sent upstream.
func TestApplyEventsRejectsAnEventWithoutAnID(t *testing.T) {
	s, url, done := rpcServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	})
	defer done()

	resp, err := s.ApplyEvents(context.Background(), &pluginv1.WatchSyncApplyEventsRequest{
		Context: authContext(url),
		Events: []*pluginv1.WatchSyncEvent{{
			Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED,
			Media: &pluginv1.WatchSyncMedia{
				MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
				ExternalIds: map[string]string{"tmdb": "42"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("ApplyEvents() error: %v", err)
	}
	if got := resp.GetResults(); len(got) != 1 ||
		got[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Errorf("results = %+v, want one REJECTED", got)
	}
}

// GetAccount doubles as the credential check, so an unauthorized key must
// surface as a credential fault the host can act on.
func TestGetAccountReportsAnInvalidKey(t *testing.T) {
	s, url, done := rpcServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"detail": "Invalid API key"})
	})
	defer done()

	resp, err := s.GetAccount(context.Background(), &pluginv1.WatchSyncGetAccountRequest{Context: authContext(url)})
	if err != nil {
		t.Fatalf("GetAccount() error: %v", err)
	}
	if resp.GetFault() == nil {
		t.Fatalf("want a fault for an invalid key")
	}
}
