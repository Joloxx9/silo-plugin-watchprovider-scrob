package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	scrobbleStart = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START
	scrobblePause = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_PAUSE
	scrobbleStop  = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP
)

// playerServer answers the player webhook with status and records the body,
// path, query and key of the last request.
type playerCapture struct {
	body   playerWebhook
	path   string
	query  string
	header string
}

func playerServer(t *testing.T, status string) (*apiClient, *playerCapture, func()) {
	t.Helper()
	got := &playerCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.query, got.header = r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Api-Key")
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		_ = json.NewEncoder(w).Encode(playerResponse{Status: status})
	}))
	client, err := newAPIClient(server.URL, "secret-key", server.Client())
	if err != nil {
		server.Close()
		t.Fatalf("newAPIClient() error: %v", err)
	}
	return client, got, server.Close
}

func episodeEvent() *pluginv1.WatchSyncEvent {
	return &pluginv1.WatchSyncEvent{
		EventId:           "e1",
		PlaybackSessionId: "sess-7",
		DurationSeconds:   3600,
		Media: &pluginv1.WatchSyncMedia{
			MediaType:         pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
			Title:             "Ep",
			SeriesTitle:       "Show",
			ExternalIds:       map[string]string{"tmdb": "777"},
			SeriesExternalIds: map[string]string{"tmdb": "100", "tvdb": "200", "imdb": "tt0100"},
			SeasonNumber:      2,
			EpisodeNumber:     5,
		},
	}
}

func scrobbleOne(t *testing.T, client *apiClient, event *pluginv1.WatchSyncEvent, operation pluginv1.WatchSyncOperation) *pluginv1.WatchSyncApplyResult {
	t.Helper()
	events := []*pluginv1.WatchSyncEvent{event}
	results := newResultSet(events)
	if fault := scrobblePlayback(context.Background(), client, events, results, operation); fault != nil {
		t.Fatalf("scrobblePlayback() fault: %v", fault)
	}
	return results.list()[0]
}

// Live playback goes to Scrob's player webhook with the key in the header,
// never in the URL, and names Silo as its source.
func TestScrobbleReportsEpisodeToThePlayerWebhook(t *testing.T) {
	client, got, done := playerServer(t, "ok")
	defer done()

	event := episodeEvent()
	event.PositionSeconds = 61.4
	result := scrobbleOne(t, client, event, scrobblePause)

	if want := "/api/proxy" + playerWebhookPath; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
	if got.header != "secret-key" || strings.Contains(got.query, "secret-key") {
		t.Errorf("key in header %q and query %q, want the header only", got.header, got.query)
	}
	body := got.body
	if body.Source != "silo" || body.Event != "playback_paused" || body.SessionID != "sess-7" {
		t.Errorf("body = %+v, want a silo pause for the host's session", body)
	}
	if body.PositionSeconds != 61 || body.TotalSeconds != 3600 {
		t.Errorf("position = %d/%d, want 61/3600", body.PositionSeconds, body.TotalSeconds)
	}
	item := body.Item
	if item.Type != "episode" || item.ShowTitle != "Show" || *item.Season != 2 || *item.Episode != 5 {
		t.Errorf("item = %+v, want the numbered episode of Show", item)
	}
	// The webhook reads TVDB and IMDb as the series' ids; TMDB is the episode's.
	if want := map[string]string{"tmdb": "777", "tvdb": "200", "imdb": "tt0100"}; !equalMaps(item.UniqueID, want) {
		t.Errorf("uniqueid = %v, want %v", item.UniqueID, want)
	}
	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Errorf("status = %v, want APPLIED", result.GetStatus())
	}
}

// Silo sends a start both for a new playback and for a paused one resuming.
func TestScrobbleNamesStartResumeAndStop(t *testing.T) {
	client, got, done := playerServer(t, "ok")
	defer done()

	for _, tc := range []struct {
		operation pluginv1.WatchSyncOperation
		position  float64
		want      string
	}{
		{scrobbleStart, 0, "playback_started"},
		{scrobbleStart, 120, "playback_resumed"},
		{scrobbleStop, 300, "playback_stopped"},
	} {
		event := episodeEvent()
		event.PositionSeconds = tc.position
		scrobbleOne(t, client, event, tc.operation)
		if got.body.Event != tc.want {
			t.Errorf("operation %v at %v s = %q, want %q", tc.operation, tc.position, got.body.Event, tc.want)
		}
	}
}

// An episode without a TMDB id of its own still reaches Scrob through its
// series' TVDB id, which the webhook resolves.
func TestScrobbleReportsATVDBOnlyEpisode(t *testing.T) {
	client, got, done := playerServer(t, "ok")
	defer done()

	event := episodeEvent()
	event.Media.ExternalIds = nil
	event.Media.SeriesExternalIds = map[string]string{"tvdb": "200"}
	if r := scrobbleOne(t, client, event, scrobbleStart); r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("status = %v, want APPLIED", r.GetStatus())
	}
	if want := map[string]string{"tvdb": "200"}; !equalMaps(got.body.Item.UniqueID, want) {
		t.Errorf("uniqueid = %v, want %v", got.body.Item.UniqueID, want)
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
	event := episodeEvent()
	event.Media.MediaType = pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES
	if r := scrobbleOne(t, client, event, scrobbleStart); r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Errorf("status = %v, want REJECTED", r.GetStatus())
	}
}

// The webhook answers 200 with "ignored" when it cannot identify the title.
func TestScrobbleRejectsATitleScrobCannotIdentify(t *testing.T) {
	client, _, done := playerServer(t, "ignored")
	defer done()

	r := scrobbleOne(t, client, episodeEvent(), scrobbleStart)
	if r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED ||
		r.GetFault().GetSafeMessage() != errScrobbleUnidentified {
		t.Errorf("result = %+v, want REJECTED as unidentified", r)
	}
}

// A server error from the webhook rejects the event with an explanation
// instead of retrying it: the usual cause, a title Scrob cannot create because
// it arrived without a name, fails the same way on every retry.
func TestScrobbleRejectsAServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, "key", server.Client())
	if err != nil {
		t.Fatalf("newAPIClient() error: %v", err)
	}
	r := scrobbleOne(t, client, episodeEvent(), scrobbleStart)
	if r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Fatalf("status = %v, want REJECTED", r.GetStatus())
	}
	if msg := r.GetFault().GetSafeMessage(); !strings.Contains(msg, "HTTP 500") || !strings.Contains(msg, "update Silo") {
		t.Errorf("message = %q, want the status and what to do", msg)
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

// stopServer records what a stop sends to Scrob. Now Playing lists this
// playback's session under both webhooks, as after an update mid-playback,
// and another playback's session.
func stopServer(t *testing.T) (*apiClient, *[]string, *scrobWatchEventCreate, func()) {
	t.Helper()
	calls := []string{}
	var written scrobWatchEventCreate
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/proxy/history":
			_ = json.NewDecoder(r.Body).Decode(&written)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/proxy/history/now-playing":
			_, _ = w.Write([]byte(`{"now_playing":[{"session_key":"kodi:3:sess-7"},{"session_key":"jellyfin:3:sess-7"},{"session_key":"kodi:3:other"}]}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			_ = json.NewEncoder(w).Encode(playerResponse{Status: "ok"})
		}
	}))
	client, err := newAPIClient(server.URL, "key", server.Client())
	if err != nil {
		server.Close()
		t.Fatalf("newAPIClient() error: %v", err)
	}
	return client, &calls, &written, server.Close
}

func stopEvent(completed bool, position float64) *pluginv1.WatchSyncEvent {
	event := episodeEvent()
	event.OccurredAt = timestamppb.New(time.Date(2026, 10, 23, 20, 15, 30, 0, time.UTC))
	event.PositionSeconds = position
	event.DurationSeconds = 100
	event.Completed = completed
	return event
}

const (
	nowPlayingCall    = "GET /api/proxy/history/now-playing"
	dismissKodi       = "DELETE /api/proxy/history/session/kodi:3:sess-7"
	dismissOldVersion = "DELETE /api/proxy/history/session/jellyfin:3:sess-7"
)

// A play Silo counted is written at Silo's time, so the next import does not
// bring it back as a second viewing, and its session is dismissed rather than
// stopped through the webhook.
func TestScrobbleStopRecordsAFinishedPlayAtSilosTime(t *testing.T) {
	client, calls, written, done := stopServer(t)
	defer done()

	r := scrobbleOne(t, client, stopEvent(true, 95), scrobbleStop)
	want := []string{"POST /api/proxy/history", nowPlayingCall, dismissKodi, dismissOldVersion}
	if strings.Join(*calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", *calls, want)
	}
	if written.WatchedAt != "2026-10-23T20:15:30Z" || written.Source != "silo" || !written.Completed {
		t.Errorf("written play = %+v, want completed at Silo's time from silo", *written)
	}
	if r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Errorf("status = %v, want APPLIED", r.GetStatus())
	}
}

// A stop Scrob would count as a play but Silo did not is dismissed, so Scrob
// holds no play Silo lacks.
func TestScrobbleStopDismissesAPlaySiloDidNotCount(t *testing.T) {
	client, calls, _, done := stopServer(t)
	defer done()

	scrobbleOne(t, client, stopEvent(false, 92), scrobbleStop)
	if want := []string{nowPlayingCall, dismissKodi, dismissOldVersion}; strings.Join(*calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", *calls, want)
	}
}

// An unfinished stop ends the session through the webhook, which records no
// finished play, and also clears a session the previous version opened.
func TestScrobbleStopSendsAnUnfinishedStopToTheWebhook(t *testing.T) {
	client, calls, _, done := stopServer(t)
	defer done()

	scrobbleOne(t, client, stopEvent(false, 40), scrobbleStop)
	want := []string{"POST /api/proxy" + playerWebhookPath, nowPlayingCall, dismissKodi, dismissOldVersion}
	if strings.Join(*calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", *calls, want)
	}
}

// Specials sit in season 0, which Scrob matches like any other season.
func TestScrobbleReportsASpecial(t *testing.T) {
	client, got, done := playerServer(t, "ok")
	defer done()

	event := episodeEvent()
	event.Media.SeasonNumber = 0
	if r := scrobbleOne(t, client, event, scrobbleStart); r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("status = %v, want APPLIED", r.GetStatus())
	}
	if got.body.Item.Season == nil || *got.body.Item.Season != 0 {
		t.Errorf("season = %v, want 0 sent explicitly", got.body.Item.Season)
	}
}
