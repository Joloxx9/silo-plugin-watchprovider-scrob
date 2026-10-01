package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

func TestProviderRatingRounds(t *testing.T) {
	cases := map[float64]int32{0: 0, 7: 7, 7.4: 7, 7.6: 8, 10: 10}
	for in, want := range cases {
		if got := providerRating(in); got != want {
			t.Fatalf("providerRating(%v) = %d, want %d", in, got, want)
		}
	}
}

func TestListRatingsSkipsSeasonRatings(t *testing.T) {
	// Regression test: a season rating is stored against the same series
	// Media row as a whole-series rating in Scrob, distinguished only by a
	// top-level season_number - media.type stays "series" either way.
	// Importing both under the series' key would have the season rating
	// silently clobber the real whole-series one.
	season := 2
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := scrobRatingsResponse{
			Results: []scrobRatingEntry{
				{Media: scrobMedia{Type: "series", TMDBID: 100, Title: "Show"}, Rating: 9},
				{Media: scrobMedia{Type: "series", TMDBID: 100, Title: "Show"}, Rating: 6, SeasonNumber: &season},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client, err := newAPIClient(server.URL, "key", server.Client())
	if err != nil {
		t.Fatalf("newAPIClient() error: %v", err)
	}
	s := NewServer(server.Client())
	resp, rpcErr := s.listRatings(context.Background(), client)
	if rpcErr != nil {
		t.Fatalf("listRatings() error: %v", rpcErr)
	}
	if resp.GetFault() != nil {
		t.Fatalf("listRatings() fault: %v", resp.GetFault())
	}
	if len(resp.GetItems()) != 1 {
		t.Fatalf("listRatings() = %d items, want 1 (the season rating must be skipped)", len(resp.GetItems()))
	}
	if resp.GetItems()[0].GetRating().GetRating() != 9 {
		t.Fatalf("Items[0].Rating = %d, want 9 (the whole-series rating, not the season's)", resp.GetItems()[0].GetRating().GetRating())
	}
	if !resp.GetCompleteSnapshot() {
		t.Fatalf("CompleteSnapshot = false, want true")
	}
}

func TestListRatingsSkipsUnrated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := scrobRatingsResponse{Results: []scrobRatingEntry{
			{Media: scrobMedia{Type: "movie", TMDBID: 1}, Rating: 0},
			{Media: scrobMedia{Type: "episode", TMDBID: 2}, Rating: 8},
		}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()
	client, _ := newAPIClient(server.URL, "key", server.Client())
	s := NewServer(server.Client())
	resp, err := s.listRatings(context.Background(), client)
	if err != nil {
		t.Fatalf("listRatings() error: %v", err)
	}
	if len(resp.GetItems()) != 0 {
		t.Fatalf("listRatings() = %d items, want 0 (unrated movie and episode-kind rating both skipped)", len(resp.GetItems()))
	}
}

func TestWriteRatingsRejectsWithoutTMDBID(t *testing.T) {
	results := newResultSet(nil)
	events := []*pluginv1.WatchSyncEvent{{
		EventId: "e1",
		Media:   &pluginv1.WatchSyncMedia{MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE},
		Rating:  8,
	}}
	results = newResultSet(events)
	if fault := writeRatings(context.Background(), nil, events, results, false); fault != nil {
		t.Fatalf("writeRatings() connection fault: %v", fault)
	}
	list := results.list()
	if list[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Fatalf("Status = %v, want REJECTED", list[0].GetStatus())
	}
}

func TestRemoveRatingTreatsNotFoundAsNoChange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	client, _ := newAPIClient(server.URL, "key", server.Client())
	event := &pluginv1.WatchSyncEvent{
		EventId:   "e1",
		Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_RATING,
		Media: &pluginv1.WatchSyncMedia{
			MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
			ExternalIds: map[string]string{"tmdb": "42"},
		},
	}
	results := newResultSet([]*pluginv1.WatchSyncEvent{event})
	if fault := writeRatings(context.Background(), client, []*pluginv1.WatchSyncEvent{event}, results, true); fault != nil {
		t.Fatalf("writeRatings() connection fault: %v", fault)
	}
	list := results.list()
	if list[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE {
		t.Fatalf("Status = %v, want NO_CHANGE", list[0].GetStatus())
	}
}
