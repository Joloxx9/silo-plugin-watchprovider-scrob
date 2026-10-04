package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// An episode rating imports under the same provider item key the watched
// history produces for that episode, so Silo matches the two against one row.
func TestListRatingsImportsEpisodes(t *testing.T) {
	season, episode := 1, 3
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(scrobRatingsResponse{Results: []scrobRatingEntry{{
			Media: scrobMedia{
				Type: "episode", TMDBID: 555, Title: "Pilot",
				SeasonNumber: &season, EpisodeNumber: &episode,
				ShowTitle: "Show", ShowTMDBID: 100,
			},
			Rating: 8,
		}}})
	}))
	defer server.Close()

	client, err := newAPIClient(server.URL, "key", server.Client())
	if err != nil {
		t.Fatalf("newAPIClient() error: %v", err)
	}
	resp, rpcErr := NewServer(server.Client()).listRatings(context.Background(), client)
	if rpcErr != nil {
		t.Fatalf("listRatings() error: %v", rpcErr)
	}
	items := resp.GetItems()
	if len(items) != 1 {
		t.Fatalf("listRatings() = %d items, want 1", len(items))
	}

	got := items[0]
	wantKey := episodeKey(scrobIDs{TMDB: 100}, season, episode, scrobIDs{TMDB: 555})
	if got.GetProviderItemKey() != wantKey {
		t.Errorf("ProviderItemKey = %q, want %q (must match the watched history key)", got.GetProviderItemKey(), wantKey)
	}
	if got.GetMedia().GetMediaType() != pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE {
		t.Errorf("MediaType = %v, want EPISODE", got.GetMedia().GetMediaType())
	}
	if got.GetMedia().GetSeasonNumber() != int32(season) || got.GetMedia().GetEpisodeNumber() != int32(episode) {
		t.Errorf("season/episode = %d/%d, want %d/%d",
			got.GetMedia().GetSeasonNumber(), got.GetMedia().GetEpisodeNumber(), season, episode)
	}
	if got.GetMedia().GetSeriesTitle() != "Show" {
		t.Errorf("SeriesTitle = %q, want %q", got.GetMedia().GetSeriesTitle(), "Show")
	}
	if got.GetRating().GetRating() != 8 {
		t.Errorf("Rating = %d, want 8", got.GetRating().GetRating())
	}
}

// An episode with no season or episode number cannot be keyed, so it is
// skipped rather than imported under a key that would collide with the series.
func TestListRatingsSkipsUnnumberedEpisode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(scrobRatingsResponse{Results: []scrobRatingEntry{{
			Media:  scrobMedia{Type: "episode", TMDBID: 555, Title: "Pilot", ShowTMDBID: 100},
			Rating: 8,
		}}})
	}))
	defer server.Close()

	client, err := newAPIClient(server.URL, "key", server.Client())
	if err != nil {
		t.Fatalf("newAPIClient() error: %v", err)
	}
	resp, rpcErr := NewServer(server.Client()).listRatings(context.Background(), client)
	if rpcErr != nil {
		t.Fatalf("listRatings() error: %v", rpcErr)
	}
	if len(resp.GetItems()) != 0 {
		t.Fatalf("listRatings() = %d items, want 0", len(resp.GetItems()))
	}
}

// Scrob will not create a media row for an episode from a rating alone and
// answers 400. That is a rejection with an explanation, not a transport
// failure to retry forever.
func TestWriteRatingsRejectsEpisodeScrobDoesNotTrack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"Cannot create media row for episodes via rating"}`))
	}))
	defer server.Close()

	client, err := newAPIClient(server.URL, "key", server.Client())
	if err != nil {
		t.Fatalf("newAPIClient() error: %v", err)
	}
	event := &pluginv1.WatchSyncEvent{
		EventId: "e1",
		Rating:  8,
		Media: &pluginv1.WatchSyncMedia{
			MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
			ExternalIds: map[string]string{"tmdb": "555"},
		},
	}
	events := []*pluginv1.WatchSyncEvent{event}
	results := newResultSet(events)
	if fault := writeRatings(context.Background(), client, events, results, false); fault != nil {
		t.Fatalf("writeRatings() fault = %v, want nil (a rejection is not connection-wide)", fault)
	}
	got := results.list()
	if len(got) != 1 {
		t.Fatalf("results = %d, want 1", len(got))
	}
	if got[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Errorf("status = %v, want REJECTED", got[0].GetStatus())
	}
}
