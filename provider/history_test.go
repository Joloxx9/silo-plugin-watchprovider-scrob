package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

func scrobTimePtr(t time.Time) *scrobTime {
	st := scrobTime(t)
	return &st
}

func TestWatchedStateFromEventSkipsIncompleteOrUnidentified(t *testing.T) {
	if _, _, ok := watchedStateFromEvent(scrobHistoryEvent{Media: scrobMedia{Type: "movie", TMDBID: 1}}); ok {
		t.Fatalf("expected no state for an event with no watched_at")
	}
	watchedAt := scrobTimePtr(time.Now())
	if _, _, ok := watchedStateFromEvent(scrobHistoryEvent{Media: scrobMedia{Type: "movie"}, WatchedAt: watchedAt}); ok {
		t.Fatalf("expected no state for a movie with no external id")
	}
	if _, _, ok := watchedStateFromEvent(scrobHistoryEvent{
		Media:     scrobMedia{Type: "episode", TMDBID: 1, ShowTMDBID: 9},
		WatchedAt: watchedAt,
	}); ok {
		t.Fatalf("expected no state for an episode missing season/episode numbers")
	}
}

func TestListWatchedAggregatesPlaysAcrossPages(t *testing.T) {
	page := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page++
		var resp scrobHistoryResponse
		switch page {
		case 1:
			resp = scrobHistoryResponse{Page: 1, TotalPages: 2, Results: []scrobHistoryEvent{
				{Media: scrobMedia{Type: "movie", TMDBID: 1, Title: "A"}, WatchedAt: scrobTimePtr(time.Unix(100, 0))},
			}}
		case 2:
			resp = scrobHistoryResponse{Page: 2, TotalPages: 2, Results: []scrobHistoryEvent{
				{Media: scrobMedia{Type: "movie", TMDBID: 1, Title: "A"}, WatchedAt: scrobTimePtr(time.Unix(200, 0))},
			}}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client, err := newAPIClient(server.URL, "key", server.Client())
	if err != nil {
		t.Fatalf("newAPIClient() error: %v", err)
	}
	s := NewServer(server.Client())
	resp, rpcErr := s.listWatched(context.Background(), client, &pluginv1.WatchSyncListRemoteStateRequest{})
	if rpcErr != nil {
		t.Fatalf("listWatched() error: %v", rpcErr)
	}
	if resp.GetFault() != nil {
		t.Fatalf("listWatched() fault: %v", resp.GetFault())
	}
	if len(resp.GetItems()) != 1 {
		t.Fatalf("listWatched() = %d items, want 1", len(resp.GetItems()))
	}
	if resp.GetItems()[0].GetWatched().GetPlayCount() != 2 {
		t.Fatalf("PlayCount = %d, want 2", resp.GetItems()[0].GetWatched().GetPlayCount())
	}
	if !resp.GetItems()[0].GetWatched().GetLastWatchedAt().AsTime().Equal(time.Unix(200, 0)) {
		t.Fatalf("LastWatchedAt = %v, want %v", resp.GetItems()[0].GetWatched().GetLastWatchedAt().AsTime(), time.Unix(200, 0))
	}
	if page != 2 {
		t.Fatalf("fetched %d pages, want 2", page)
	}
}

func TestMarkUnwatchedTreatsNotFoundAsNoChange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	client, _ := newAPIClient(server.URL, "key", server.Client())
	event := &pluginv1.WatchSyncEvent{
		EventId:   "e1",
		Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_UNWATCHED,
		Media: &pluginv1.WatchSyncMedia{
			MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
			ExternalIds: map[string]string{"tmdb": "42"},
		},
	}
	results := newResultSet([]*pluginv1.WatchSyncEvent{event})
	if fault := markUnwatched(context.Background(), client, []*pluginv1.WatchSyncEvent{event}, results); fault != nil {
		t.Fatalf("markUnwatched() connection fault: %v", fault)
	}
	list := results.list()
	if list[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE {
		t.Fatalf("Status = %v, want NO_CHANGE", list[0].GetStatus())
	}
}

func TestWatchEventBodyRequiresSeriesIDForEpisode(t *testing.T) {
	event := &pluginv1.WatchSyncEvent{
		Media: &pluginv1.WatchSyncMedia{
			MediaType:     pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
			ExternalIds:   map[string]string{"tmdb": "42"},
			SeasonNumber:  1,
			EpisodeNumber: 2,
		},
	}
	if _, ok := watchEventBody(event); ok {
		t.Fatalf("watchEventBody() ok = true for episode with no series id, want false")
	}
	event.Media.SeriesExternalIds = map[string]string{"tmdb": "7"}
	body, ok := watchEventBody(event)
	if !ok {
		t.Fatalf("watchEventBody() ok = false, want true")
	}
	if body.SeriesTMDBID != 7 || body.SeasonNumber == nil || *body.SeasonNumber != 1 || body.EpisodeNumber == nil || *body.EpisodeNumber != 2 {
		t.Fatalf("unexpected body: %+v", body)
	}
}
