package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

func progressServer(t *testing.T, entries []scrobProgressEntry) (*apiClient, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/api/proxy/history/continue-watching" && got != "/history/continue-watching" {
			t.Errorf("path = %q, want the continue-watching endpoint", got)
		}
		_ = json.NewEncoder(w).Encode(scrobContinueWatchingResponse{ContinueWatching: entries})
	}))
	client, err := newAPIClient(server.URL, "key", server.Client())
	if err != nil {
		server.Close()
		t.Fatalf("newAPIClient() error: %v", err)
	}
	return client, server.Close
}

// Resume positions import for movies and episodes, under the same keys the
// watched history uses.
func TestListProgressImportsMoviesAndEpisodes(t *testing.T) {
	season, episode := 2, 5
	client, done := progressServer(t, []scrobProgressEntry{
		{Media: scrobMedia{Type: "movie", TMDBID: 42, Title: "Heat"}, ProgressFraction: 0.375},
		{Media: scrobMedia{
			Type: "episode", TMDBID: 777, Title: "Ep", SeasonNumber: &season, EpisodeNumber: &episode,
			ShowTitle: "Show", ShowTMDBID: 100,
		}, ProgressFraction: 0.10},
	})
	defer done()

	resp, rpcErr := NewServer(http.DefaultClient).listProgress(context.Background(), client)
	if rpcErr != nil {
		t.Fatalf("listProgress() error: %v", rpcErr)
	}
	if resp.GetFault() != nil {
		t.Fatalf("listProgress() fault: %v", resp.GetFault())
	}
	items := resp.GetItems()
	if len(items) != 2 {
		t.Fatalf("listProgress() = %d items, want 2", len(items))
	}
	// Scrob reports 0.375 of the runtime; the contract wants 37.5 percent.
	if got := items[0].GetProgress().GetProgressPercent(); got != 37.5 {
		t.Errorf("movie progress = %v, want 37.5 (Scrob sent the fraction 0.375)", got)
	}
	if got := items[0].GetProviderItemKey(); got != movieKey(scrobIDs{TMDB: 42}) {
		t.Errorf("movie key = %q, want the watched-history movie key", got)
	}
	if items[1].GetMedia().GetMediaType() != pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE {
		t.Errorf("second item is not an episode")
	}
	if got := items[1].GetProviderItemKey(); got != episodeKey(scrobIDs{TMDB: 100}, season, episode, scrobIDs{TMDB: 777}) {
		t.Errorf("episode key = %q, want the watched-history episode key", got)
	}
	if !resp.GetCompleteSnapshot() {
		t.Errorf("CompleteSnapshot = false, want true")
	}
}

// The contract reserves 100 for a completed play, which belongs in the watched
// family. Importing it as progress would show a title finished and resumable
// at once.
func TestListProgressSkipsPositionsOutsideRange(t *testing.T) {
	client, done := progressServer(t, []scrobProgressEntry{
		{Media: scrobMedia{Type: "movie", TMDBID: 1, Title: "Done"}, ProgressFraction: 1.0},
		{Media: scrobMedia{Type: "movie", TMDBID: 2, Title: "Unstarted"}, ProgressFraction: 0},
	})
	defer done()

	resp, rpcErr := NewServer(http.DefaultClient).listProgress(context.Background(), client)
	if rpcErr != nil {
		t.Fatalf("listProgress() error: %v", rpcErr)
	}
	if len(resp.GetItems()) != 0 {
		t.Fatalf("listProgress() = %d items, want 0", len(resp.GetItems()))
	}
}
