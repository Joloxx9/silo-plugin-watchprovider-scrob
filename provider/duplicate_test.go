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
			var body scrobWatchEventCreate
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Source != "silo" {
				t.Errorf("exported play source = %q (err %v), want silo", body.Source, err)
			}
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
	if fault := markWatched(context.Background(), client, events, results, time.Time{}); fault != nil {
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
	if fault := markWatched(context.Background(), client, events, results, time.Time{}); fault != nil {
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
	if fault := markWatched(context.Background(), client, events, results, time.Time{}); fault != nil {
		t.Fatalf("markWatched() fault: %v", fault)
	}
	if len(posted) != 1 {
		t.Errorf("posted %v, want the export to proceed without the duplicate check", posted)
	}
}

// Scrob keeps a row per viewing. Undoing one rewatch must not take the title's
// whole history with it.
func TestMarkUnwatchedDeletesOnlyThePlayAtThatTime(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	match := scrobTime(at.Add(-20 * time.Second))
	other := scrobTime(at.Add(-72 * time.Hour))
	var deleted string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(scrobItemEventsResponse{Watched: true, Events: []scrobItemEvent{
			{ID: 77, WatchedAt: &match},
			{ID: 12, WatchedAt: &other},
		}})
	}))
	defer server.Close()

	client, err := newAPIClient(server.URL, "key", server.Client())
	if err != nil {
		t.Fatalf("newAPIClient() error: %v", err)
	}
	events := []*pluginv1.WatchSyncEvent{movieWatch(at)}
	results := newResultSet(events)
	if fault := markUnwatched(context.Background(), client, events, results); fault != nil {
		t.Fatalf("markUnwatched() fault: %v", fault)
	}
	if want := "/api/proxy/history/event/77"; deleted != want {
		t.Errorf("deleted %q, want %q (only the play at that time)", deleted, want)
	}
}

// unwatchServer answers the per-title lookup with held and fails the test on
// any delete.
func unwatchServer(t *testing.T, held []scrobItemEvent) (*apiClient, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			t.Errorf("unexpected delete of %s?%s", r.URL.Path, r.URL.RawQuery)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(scrobItemEventsResponse{Watched: len(held) > 0, Events: held})
	}))
	client, err := newAPIClient(server.URL, "key", server.Client())
	if err != nil {
		server.Close()
		t.Fatalf("newAPIClient() error: %v", err)
	}
	return client, server.Close
}

// A Silo play Scrob has no copy of, such as one imported from a media server
// with a different time, must not remove the title's other Scrob plays.
func TestMarkUnwatchedWithoutAMatchingPlayDeletesNothing(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	other := scrobTime(at.Add(-72 * time.Hour))
	client, done := unwatchServer(t, []scrobItemEvent{{ID: 12, WatchedAt: &other}})
	defer done()

	events := []*pluginv1.WatchSyncEvent{movieWatch(at)}
	results := newResultSet(events)
	if fault := markUnwatched(context.Background(), client, events, results); fault != nil {
		t.Fatalf("markUnwatched() fault: %v", fault)
	}
	if got := results.list(); len(got) != 1 ||
		got[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE {
		t.Errorf("results = %+v, want one NO_CHANGE", got)
	}
}

// With no timestamp there is no single play to name, so the event is rejected
// rather than guessed at.
func TestMarkUnwatchedWithoutATimeIsRejected(t *testing.T) {
	client, done := unwatchServer(t, nil)
	defer done()

	event := movieWatch(time.Now().UTC())
	event.OccurredAt = nil
	events := []*pluginv1.WatchSyncEvent{event}
	results := newResultSet(events)
	if fault := markUnwatched(context.Background(), client, events, results); fault != nil {
		t.Fatalf("markUnwatched() fault: %v", fault)
	}
	if got := results.list(); len(got) != 1 ||
		got[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Errorf("results = %+v, want one REJECTED", got)
	}
}

// A play from before the profile connected, often imported from a media
// server with its own timestamp, is not exported when Scrob already has the
// title watched at any time: that is almost surely the same viewing.
func TestMarkWatchedSkipsAnOlderPlayOfATitleScrobHas(t *testing.T) {
	connected := time.Now().UTC().Truncate(time.Second)
	held := scrobTime(connected.Add(-400 * 24 * time.Hour))
	client, posted, done := watchedExportServer(t, []scrobItemEvent{{ID: 1, WatchedAt: &held}})
	defer done()

	events := []*pluginv1.WatchSyncEvent{movieWatch(connected.Add(-24 * time.Hour))}
	results := newResultSet(events)
	if fault := markWatched(context.Background(), client, events, results, connected); fault != nil {
		t.Fatalf("markWatched() fault: %v", fault)
	}
	if len(*posted) != 0 {
		t.Errorf("posted %v, want no write for a title Scrob has watched", *posted)
	}
	if got := results.list(); got[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE {
		t.Errorf("status = %v, want NO_CHANGE", got[0].GetStatus())
	}
}

// An older play of a title Scrob has never seen watched is still exported, so
// the history Silo had before connecting reaches Scrob.
func TestMarkWatchedExportsAnOlderPlayOfATitleScrobLacks(t *testing.T) {
	connected := time.Now().UTC().Truncate(time.Second)
	client, posted, done := watchedExportServer(t, nil)
	defer done()

	events := []*pluginv1.WatchSyncEvent{movieWatch(connected.Add(-24 * time.Hour))}
	results := newResultSet(events)
	if fault := markWatched(context.Background(), client, events, results, connected); fault != nil {
		t.Fatalf("markWatched() fault: %v", fault)
	}
	if len(*posted) != 1 {
		t.Errorf("posted %v, want the play exported", *posted)
	}
}

// Without the lookup an older play could duplicate a viewing, so it is
// retried rather than exported blind.
func TestMarkWatchedRetriesAnOlderPlayWhenTheLookupFails(t *testing.T) {
	connected := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("unexpected export")
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client, _ := newAPIClient(server.URL, "key", server.Client())

	events := []*pluginv1.WatchSyncEvent{movieWatch(connected.Add(-24 * time.Hour))}
	results := newResultSet(events)
	if fault := markWatched(context.Background(), client, events, results, connected); fault != nil {
		t.Fatalf("markWatched() fault: %v", fault)
	}
	if got := results.list(); got[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY {
		t.Errorf("status = %v, want RETRY", got[0].GetStatus())
	}
}
