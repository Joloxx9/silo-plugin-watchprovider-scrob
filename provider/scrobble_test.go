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

// Scrob accepts live playback only through a media-server webhook, keyed by the
// session id and authenticated by the api_key query parameter.
func TestScrobbleReportsEpisodeAsJellyfinWebhook(t *testing.T) {
	var got jellyfinWebhook
	var path, apiKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		apiKey = r.URL.Query().Get("api_key")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	client, err := newAPIClient(server.URL, "secret-key", server.Client())
	if err != nil {
		t.Fatalf("newAPIClient() error: %v", err)
	}
	event := &pluginv1.WatchSyncEvent{
		EventId:           "e1",
		PlaybackSessionId: "sess-7",
		PositionSeconds:   61,
		DurationSeconds:   3600,
		Media: &pluginv1.WatchSyncMedia{
			MediaType:         pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
			Title:             "Ep",
			SeriesTitle:       "Show",
			ExternalIds:       map[string]string{"tmdb": "777"},
			SeriesExternalIds: map[string]string{"tmdb": "100"},
			SeasonNumber:      2,
			EpisodeNumber:     5,
		},
	}
	events := []*pluginv1.WatchSyncEvent{event}
	results := newResultSet(events)
	if fault := scrobblePlayback(context.Background(), client, events, results, "PlaybackStart"); fault != nil {
		t.Fatalf("scrobblePlayback() fault: %v", fault)
	}

	if apiKey != "secret-key" {
		t.Errorf("api_key = %q, want the connection's key", apiKey)
	}
	if want := "/api/proxy" + jellyfinWebhookPath; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if got.NotificationType != "PlaybackStart" {
		t.Errorf("NotificationType = %q, want PlaybackStart", got.NotificationType)
	}
	if got.Item.Type != "Episode" || got.Item.SeriesProviderIds.Tmdb != "100" {
		t.Errorf("item = %+v, want an episode carrying its series TMDB id", got.Item)
	}
	if got.Item.ParentIndexNumber == nil || *got.Item.ParentIndexNumber != 2 ||
		got.Item.IndexNumber == nil || *got.Item.IndexNumber != 5 {
		t.Errorf("season/episode numbers missing from %+v", got.Item)
	}
	if got.Session.ID != "sess-7" {
		t.Errorf("Session.Id = %q, want the host's playback session id", got.Session.ID)
	}
	if got.Session.PlayState.PositionTicks != 61*ticksPerSecond {
		t.Errorf("PositionTicks = %d, want %d", got.Session.PlayState.PositionTicks, 61*ticksPerSecond)
	}
	if got.Item.RunTimeTicks != 3600*ticksPerSecond {
		t.Errorf("RunTimeTicks = %d, want %d", got.Item.RunTimeTicks, 3600*ticksPerSecond)
	}
	if r := results.list(); len(r) != 1 || r[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Errorf("results = %+v, want one APPLIED", r)
	}
}

// A series-level event names no playable item, so there is nothing to report.
func TestScrobbleRejectsUnplayableMedia(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer server.Close()

	client, err := newAPIClient(server.URL, "key", server.Client())
	if err != nil {
		t.Fatalf("newAPIClient() error: %v", err)
	}
	events := []*pluginv1.WatchSyncEvent{{
		EventId: "e1",
		Media: &pluginv1.WatchSyncMedia{
			MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES,
			ExternalIds: map[string]string{"tmdb": "100"},
		},
	}}
	results := newResultSet(events)
	if fault := scrobblePlayback(context.Background(), client, events, results, "PlaybackStart"); fault != nil {
		t.Fatalf("scrobblePlayback() fault: %v", fault)
	}
	if r := results.list(); len(r) != 1 || r[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Errorf("results = %+v, want one REJECTED", r)
	}
}
