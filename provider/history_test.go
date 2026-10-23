package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
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

// historyServer serves pages of Scrob history, pageSize plays per page, and
// records the page numbers and sizes asked for.
func historyServer(t *testing.T, plays []scrobHistoryEvent) (*Server, *apiClient, *[]string, func()) {
	t.Helper()
	asked := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
		asked = append(asked, r.URL.Query().Get("page")+"/"+r.URL.Query().Get("page_size"))
		start := min((page-1)*size, len(plays))
		end := min(start+size, len(plays))
		_ = json.NewEncoder(w).Encode(scrobHistoryResponse{
			Page: page, TotalPages: max(1, (len(plays)+size-1)/size), Results: plays[start:end],
		})
	}))
	client, err := newAPIClient(server.URL, "key", server.Client())
	if err != nil {
		server.Close()
		t.Fatalf("newAPIClient() error: %v", err)
	}
	return NewServer(server.Client()), client, &asked, server.Close
}

func moviePlay(tmdb int, at time.Time) scrobHistoryEvent {
	return scrobHistoryEvent{Media: scrobMedia{Type: "movie", TMDBID: tmdb, Title: "M"}, WatchedAt: scrobTimePtr(at)}
}

func listWatchedPage(t *testing.T, s *Server, client *apiClient, req *pluginv1.WatchSyncListRemoteStateRequest) *pluginv1.WatchSyncListRemoteStateResponse {
	t.Helper()
	resp, err := s.listWatched(context.Background(), client, req)
	if err != nil {
		t.Fatalf("listWatched() error: %v", err)
	}
	if resp.GetFault() != nil {
		t.Fatalf("listWatched() fault: %v", resp.GetFault())
	}
	return resp
}

// A first import reads the whole history one page per call, so a large
// history no longer has to fit in one call, and only the last page carries
// the cursor.
func TestListWatchedPagesThroughAFirstImport(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s, client, asked, done := historyServer(t, []scrobHistoryEvent{
		moviePlay(1, base.Add(3*time.Hour)), moviePlay(2, base.Add(2*time.Hour)), moviePlay(3, base.Add(time.Hour)),
	})
	defer done()
	s.now = func() time.Time { return base.Add(24 * time.Hour) }

	first := listWatchedPage(t, s, client, &pluginv1.WatchSyncListRemoteStateRequest{PageSize: 2})
	if len(first.GetItems()) != 2 || first.GetNextPageToken() == "" || first.GetNextCursor() != "" {
		t.Fatalf("first page = %d items, token %q, cursor %q; want 2 items and a page token only",
			len(first.GetItems()), first.GetNextPageToken(), first.GetNextCursor())
	}
	second := listWatchedPage(t, s, client, &pluginv1.WatchSyncListRemoteStateRequest{PageSize: 2, PageToken: first.GetNextPageToken()})
	if len(second.GetItems()) != 1 || second.GetNextPageToken() != "" || second.GetNextCursor() == "" {
		t.Fatalf("second page = %d items, token %q, cursor %q; want 1 item and the cursor",
			len(second.GetItems()), second.GetNextPageToken(), second.GetNextCursor())
	}
	if first.GetCompleteSnapshot() || second.GetCompleteSnapshot() {
		t.Errorf("an import must not claim a complete snapshot")
	}
	if want := []string{"1/2", "2/2"}; strings.Join(*asked, ",") != strings.Join(want, ",") {
		t.Errorf("asked pages %v, want %v", *asked, want)
	}
	var cursor watchedCursor
	if !decodeToken(second.GetNextCursor(), &cursor) || !cursor.Newest.Equal(base.Add(3*time.Hour)) || !cursor.FullAt.Equal(base.Add(24*time.Hour)) {
		t.Errorf("cursor = %+v, want the newest play and this full import's start", cursor)
	}
}

// After a recent full import, an import stops at plays older than the newest
// one it saw, less the overlap, instead of reading the whole history again.
func TestListWatchedStopsAnIncrementalImportAtTheCursor(t *testing.T) {
	newest := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	fullAt := newest.Add(time.Hour)
	s, client, asked, done := historyServer(t, []scrobHistoryEvent{
		moviePlay(1, newest.Add(2*time.Hour)),
		moviePlay(2, newest.Add(-time.Hour)),
		moviePlay(3, newest.Add(-incrementalOverlap-time.Hour)),
		moviePlay(4, newest.Add(-30*24*time.Hour)),
	})
	defer done()
	s.now = func() time.Time { return fullAt.Add(6 * time.Hour) }

	cursor := encodeToken(watchedCursor{Newest: newest, FullAt: fullAt})
	// The first page is all newer than the stop, so the second is read.
	first := listWatchedPage(t, s, client, &pluginv1.WatchSyncListRemoteStateRequest{PageSize: 2, Cursor: cursor})
	resp := listWatchedPage(t, s, client, &pluginv1.WatchSyncListRemoteStateRequest{
		PageSize: 2, Cursor: cursor, PageToken: first.GetNextPageToken(),
	})
	if len(resp.GetItems()) != 0 {
		t.Errorf("second page items = %d, want none past the stop", len(resp.GetItems()))
	}
	if resp.GetNextPageToken() != "" || resp.GetNextCursor() == "" {
		t.Fatalf("import did not finish at the stop: token %q", resp.GetNextPageToken())
	}
	if len(*asked) != 2 {
		t.Errorf("asked pages %v, want two pages and no further", *asked)
	}
	var next watchedCursor
	if !decodeToken(resp.GetNextCursor(), &next) || !next.Newest.Equal(newest.Add(2*time.Hour)) || !next.FullAt.Equal(fullAt) {
		t.Errorf("cursor = %+v, want the new newest play and the earlier full import kept", next)
	}
}

// A day after the last full import, the next import is full again, so plays
// added to Scrob with old dates still reach Silo.
func TestListWatchedRunsAFullImportOnceADay(t *testing.T) {
	newest := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	s, client, _, done := historyServer(t, []scrobHistoryEvent{
		moviePlay(1, newest), moviePlay(2, newest.Add(-30*24*time.Hour)),
	})
	defer done()
	s.now = func() time.Time { return newest.Add(fullImportInterval + time.Hour) }

	resp := listWatchedPage(t, s, client, &pluginv1.WatchSyncListRemoteStateRequest{
		Cursor: encodeToken(watchedCursor{Newest: newest, FullAt: newest}),
	})
	if len(resp.GetItems()) != 2 {
		t.Errorf("items = %d, want the old play too", len(resp.GetItems()))
	}
}

// A title's plays on one page become one item at its latest play.
func TestListWatchedFoldsATitlesPlaysOnAPage(t *testing.T) {
	s, client, _, done := historyServer(t, []scrobHistoryEvent{
		moviePlay(1, time.Unix(200, 0)), moviePlay(1, time.Unix(100, 0)),
	})
	defer done()

	resp := listWatchedPage(t, s, client, &pluginv1.WatchSyncListRemoteStateRequest{})
	if len(resp.GetItems()) != 1 {
		t.Fatalf("items = %d, want 1", len(resp.GetItems()))
	}
	watched := resp.GetItems()[0].GetWatched()
	if watched.GetPlayCount() != 2 || !watched.GetLastWatchedAt().AsTime().Equal(time.Unix(200, 0)) {
		t.Errorf("watched = %+v, want 2 plays, latest at 200", watched)
	}
}

func TestListWatchedRejectsAForeignPageToken(t *testing.T) {
	s, client, _, done := historyServer(t, nil)
	defer done()
	resp, err := s.listWatched(context.Background(), client, &pluginv1.WatchSyncListRemoteStateRequest{PageToken: "not-a-token"})
	if err != nil || resp.GetFault() == nil {
		t.Errorf("listWatched() = %v, %v; want a fault", resp, err)
	}
}

// A play deleted between the lookup and the delete is already in the desired
// state.
func TestMarkUnwatchedTreatsNotFoundAsNoChange(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	held := scrobTime(at)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(scrobItemEventsResponse{Watched: true, Events: []scrobItemEvent{{ID: 5, WatchedAt: &held}}})
	}))
	defer server.Close()
	client, _ := newAPIClient(server.URL, "key", server.Client())
	event := &pluginv1.WatchSyncEvent{
		EventId:    "e1",
		Operation:  pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_UNWATCHED,
		OccurredAt: timestamppb.New(at),
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

// An episode with its own id but no series id is still exported, looked up
// by that id; with series ids it carries the show and numbers too.
func TestWatchEventBodyForAnEpisode(t *testing.T) {
	event := &pluginv1.WatchSyncEvent{
		Media: &pluginv1.WatchSyncMedia{
			MediaType:     pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
			ExternalIds:   map[string]string{"tmdb": "42"},
			SeasonNumber:  1,
			EpisodeNumber: 2,
		},
	}
	if body, ok := watchEventBody(event); !ok || body.TMDBID != 42 || body.SeasonNumber != nil {
		t.Fatalf("watchEventBody() = %+v, %v; want the episode's own id only", body, ok)
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
