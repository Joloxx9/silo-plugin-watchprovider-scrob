package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

func seriesDropEvent(op pluginv1.WatchSyncOperation, ids map[string]string) *pluginv1.WatchSyncEvent {
	return &pluginv1.WatchSyncEvent{
		EventId:   "e1",
		Operation: op,
		Media: &pluginv1.WatchSyncMedia{
			MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES,
			Title:       "Game of Thrones",
			ExternalIds: ids,
		},
	}
}

func applyOneDrop(t *testing.T, handler http.HandlerFunc, event *pluginv1.WatchSyncEvent, removing bool) *pluginv1.WatchSyncApplyResult {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := newAPIClient(server.URL, "key", server.Client())
	if err != nil {
		t.Fatalf("newAPIClient() error: %v", err)
	}
	events := []*pluginv1.WatchSyncEvent{event}
	results := newResultSet(events)
	if fault := writeDropped(context.Background(), client, events, results, removing); fault != nil {
		t.Fatalf("writeDropped() connection fault: %v", fault)
	}
	return results.list()[0]
}

// Silo's dropped traversal is series-only, so Scrob's dropped movies have no
// key they could be imported under and must not reach the host.
func TestListDroppedImportsShowsAndDiscardsMovies(t *testing.T) {
	s, url, done := rpcServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/proxy/history/dropped" {
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"shows":[{"id":7,"tmdb_id":1399,"tvdb_id":121361,"title":"Game of Thrones"}],"movies":[{"id":3,"tmdb_id":550}]}`))
	})
	defer done()

	resp, err := s.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{
		Context:    authContext(url),
		StateKinds: []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_DROPPED},
	})
	if err != nil {
		t.Fatalf("ListRemoteState() error: %v", err)
	}
	if resp.GetFault() != nil {
		t.Fatalf("fault: %v", resp.GetFault())
	}
	if !resp.GetCompleteSnapshot() {
		t.Error("CompleteSnapshot = false; an undrop made in Scrob is only visible as an absence")
	}
	if got := len(resp.GetItems()); got != 1 {
		t.Fatalf("items = %d, want 1 (the movie must be discarded)", got)
	}
	item := resp.GetItems()[0]
	if item.GetMedia().GetMediaType() != pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES {
		t.Errorf("MediaType = %v, want SERIES", item.GetMedia().GetMediaType())
	}
	if item.GetDropped() == nil {
		t.Error("Dropped state missing")
	}
	if item.GetDropped().GetListedAt() != nil {
		t.Error("ListedAt set; Scrob stores dropped shows without timestamps")
	}
}

func TestWriteDroppedRejectsNonSeries(t *testing.T) {
	event := &pluginv1.WatchSyncEvent{
		EventId:   "e1",
		Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_DROPPED,
		Media: &pluginv1.WatchSyncMedia{
			MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
			ExternalIds: map[string]string{"tmdb": "550"},
		},
	}
	results := newResultSet([]*pluginv1.WatchSyncEvent{event})
	if fault := writeDropped(context.Background(), nil, []*pluginv1.WatchSyncEvent{event}, results, false); fault != nil {
		t.Fatalf("writeDropped() connection fault: %v", fault)
	}
	if got := results.list()[0].GetStatus(); got != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Fatalf("Status = %v, want REJECTED", got)
	}
}

func TestWriteDroppedRejectsWithoutExternalID(t *testing.T) {
	event := seriesDropEvent(pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_DROPPED, nil)
	results := newResultSet([]*pluginv1.WatchSyncEvent{event})
	if fault := writeDropped(context.Background(), nil, []*pluginv1.WatchSyncEvent{event}, results, false); fault != nil {
		t.Fatalf("writeDropped() connection fault: %v", fault)
	}
	if got := results.list()[0].GetStatus(); got != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Fatalf("Status = %v, want REJECTED", got)
	}
}

// Dropping addresses Scrob's own show id, so the TMDB identity has to be
// resolved through the show lookup before the write.
func TestWriteDroppedMarksSeriesUsingResolvedShowID(t *testing.T) {
	var sentShowID int
	result := applyOneDrop(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/proxy/shows/1399":
			_, _ = w.Write([]byte(`{"id":7,"tmdb_id":1399,"title":"Game of Thrones"}`))
		case "/api/proxy/history/drop/show":
			body, _ := io.ReadAll(r.Body)
			var in scrobDropShowIn
			_ = json.Unmarshal(body, &in)
			sentShowID = in.ShowID
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
	}, seriesDropEvent(pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_DROPPED, map[string]string{"tmdb": "1399"}), false)

	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("Status = %v, want APPLIED", result.GetStatus())
	}
	if sentShowID != 7 {
		t.Errorf("show_id = %d, want 7 (Scrob's own id, not the TMDB id)", sentShowID)
	}
}

// A TVDB-only series has no show lookup to go through, so an undrop falls back
// to the dropped list, which carries both external ids beside Scrob's id.
func TestWriteDroppedUndropsThroughDroppedListFallback(t *testing.T) {
	var deletedQuery string
	result := applyOneDrop(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/proxy/history/dropped":
			_, _ = w.Write([]byte(`{"shows":[{"id":7,"tmdb_id":1399,"tvdb_id":121361,"title":"Game of Thrones"}],"movies":[]}`))
		case r.URL.Path == "/api/proxy/history/drop/show" && r.Method == http.MethodDelete:
			deletedQuery = r.URL.RawQuery
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			t.Errorf("unexpected %s request to %s", r.Method, r.URL.Path)
		}
	}, seriesDropEvent(pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_UNMARK_DROPPED, map[string]string{"tvdb": "121361"}), true)

	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("Status = %v, want APPLIED", result.GetStatus())
	}
	if deletedQuery != "show_id=7" {
		t.Errorf("query = %q, want show_id=7", deletedQuery)
	}
}

// Scrob will not create a show row from a drop alone, so an untracked series
// is rejected with an explanation instead of failing and retrying forever.
func TestWriteDroppedRejectsUntrackedSeries(t *testing.T) {
	result := applyOneDrop(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/proxy/history/dropped" {
			_, _ = w.Write([]byte(`{"shows":[],"movies":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}, seriesDropEvent(pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_DROPPED, map[string]string{"tmdb": "1399"}), false)

	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Fatalf("Status = %v, want REJECTED", result.GetStatus())
	}
}

// Scrob being briefly unavailable during the show lookup is retried, not
// rejected as a series Scrob does not track.
func TestWriteDroppedRetriesWhenTheLookupFailsTemporarily(t *testing.T) {
	result := applyOneDrop(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}, seriesDropEvent(pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_DROPPED, map[string]string{"tmdb": "1399"}), false)

	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY {
		t.Fatalf("Status = %v, want RETRY", result.GetStatus())
	}
}
