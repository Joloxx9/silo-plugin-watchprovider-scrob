package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// watchedExportServer answers the per-title lookup with the plays Scrob
// already holds, and records every exported write.
func watchedExportServer(t *testing.T, held []scrobItemEvent) (*apiClient, *[]string, func()) {
	t.Helper()
	posted := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted = append(posted, r.URL.Path)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if r.URL.Query().Get("media_type") == "" {
			t.Errorf("per-title lookup without a media_type: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(scrobItemEventsResponse{Watched: len(held) > 0, Events: held})
	}))
	client, err := newAPIClient(server.URL, "key", server.Client())
	if err != nil {
		server.Close()
		t.Fatalf("newAPIClient() error: %v", err)
	}
	return client, &posted, server.Close
}

func movieWatch(at time.Time) *pluginv1.WatchSyncEvent {
	return &pluginv1.WatchSyncEvent{
		EventId:    "e1",
		OccurredAt: timestamppb.New(at),
		Media: &pluginv1.WatchSyncMedia{
			MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
			Title:       "Heat",
			ExternalIds: map[string]string{"tmdb": "42"},
		},
	}
}

// Scrob writes a play of its own when a live scrobble finishes a title. Without
// this check the same viewing is counted twice: once by the scrobble and once
// by this export.
func TestMarkWatchedSkipsAPlayScrobAlreadyRecorded(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	close := scrobTime(at.Add(-30 * time.Second))
	client, posted, done := watchedExportServer(t, []scrobItemEvent{{ID: 1, WatchedAt: &close}})
	defer done()

	events := []*pluginv1.WatchSyncEvent{movieWatch(at)}
	results := newResultSet(events)
	if fault := markWatched(context.Background(), client, events, results); fault != nil {
		t.Fatalf("markWatched() fault: %v", fault)
	}
	if len(*posted) != 0 {
		t.Errorf("posted %v, want no write for a play Scrob already has", *posted)
	}
	if got := results.list(); len(got) != 1 ||
		got[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE {
		t.Errorf("results = %+v, want one NO_CHANGE", got)
	}
}

// A play far enough from anything Scrob holds is a genuine rewatch and must
// still be exported.
func TestMarkWatchedExportsARewatchHoursLater(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	old := scrobTime(at.Add(-6 * time.Hour))
	client, posted, done := watchedExportServer(t, []scrobItemEvent{{ID: 1, WatchedAt: &old}})
	defer done()

	events := []*pluginv1.WatchSyncEvent{movieWatch(at)}
	results := newResultSet(events)
	if fault := markWatched(context.Background(), client, events, results); fault != nil {
		t.Fatalf("markWatched() fault: %v", fault)
	}
	if len(*posted) != 1 {
		t.Errorf("posted %v, want the rewatch to be exported", *posted)
	}
	if got := results.list(); len(got) != 1 ||
		got[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Errorf("results = %+v, want one APPLIED", got)
	}
}

// Losing the duplicate check is better than refusing to export, so a failed
// history read must not stop the batch.
func TestMarkWatchedStillExportsWhenTheHistoryReadFails(t *testing.T) {
	posted := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted = append(posted, r.URL.Path)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client, err := newAPIClient(server.URL, "key", server.Client())
	if err != nil {
		t.Fatalf("newAPIClient() error: %v", err)
	}
	events := []*pluginv1.WatchSyncEvent{movieWatch(time.Now().UTC())}
	results := newResultSet(events)
	if fault := markWatched(context.Background(), client, events, results); fault != nil {
		t.Fatalf("markWatched() fault: %v", fault)
	}
	if len(posted) != 1 {
		t.Errorf("posted %v, want the export to proceed without the duplicate check", posted)
	}
}
