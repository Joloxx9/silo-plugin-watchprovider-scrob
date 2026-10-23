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
)

// fakeLists is a Scrob server with lists, recording every write.
type fakeLists struct {
	lists  []scrobList
	items  map[int][]scrobListItem
	writes []string
	nextID int
	// addStatus, when set, answers every item add.
	addStatus int
}

func (f *fakeLists) serve(t *testing.T) (*apiClient, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/proxy")
		switch {
		case r.Method == http.MethodGet && path == "/lists":
			_ = json.NewEncoder(w).Encode(scrobListsResponse{Lists: f.lists})
		case r.Method == http.MethodPost && path == "/lists":
			var in scrobListIn
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.nextID++
			f.lists = append(f.lists, scrobList{ID: f.nextID, Name: in.Name})
			f.writes = append(f.writes, "create "+in.Name)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(scrobList{ID: f.nextID, Name: in.Name})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/lists/"):
			id := atoi(strings.TrimPrefix(path, "/lists/"))
			_ = json.NewEncoder(w).Encode(scrobListDetail{Items: f.items[id]})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/items"):
			var in scrobListItemIn
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.writes = append(f.writes, "add "+path+" "+in.MediaType)
			if f.addStatus != 0 {
				w.WriteHeader(f.addStatus)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodDelete:
			f.writes = append(f.writes, "delete "+path)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, path)
		}
	}))
	client, err := newAPIClient(server.URL, "key", server.Client())
	if err != nil {
		server.Close()
		t.Fatalf("newAPIClient() error: %v", err)
	}
	return client, server.Close
}

func atoi(value string) int {
	n := 0
	for _, c := range value {
		n = n*10 + int(c-'0')
	}
	return n
}

func watchlistEvent(op pluginv1.WatchSyncOperation, kind pluginv1.WatchSyncMediaType, ids map[string]string) *pluginv1.WatchSyncEvent {
	return &pluginv1.WatchSyncEvent{
		EventId:   "e1",
		Operation: op,
		Media:     &pluginv1.WatchSyncMedia{MediaType: kind, Title: "T", ExternalIds: ids},
	}
}

const (
	opAddToWatchlist      = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_ADD_TO_WATCHLIST
	opRemoveFromWatchlist = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_FROM_WATCHLIST
	movieType             = pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE
	seriesType            = pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES
)

func writeOne(t *testing.T, client *apiClient, event *pluginv1.WatchSyncEvent, removing bool) *pluginv1.WatchSyncApplyResult {
	t.Helper()
	events := []*pluginv1.WatchSyncEvent{event}
	results := newResultSet(events)
	if fault := writeWatchlist(context.Background(), client, events, results, removing); fault != nil {
		t.Fatalf("writeWatchlist() fault: %v", fault)
	}
	return results.list()[0]
}

// Before the first export there is no list, and an empty complete snapshot
// would claim Silo's watchlist was emptied in Scrob. The traversal is an
// empty incremental one instead, and nothing is created by reading.
func TestListWatchlistWithoutTheListReportsNothing(t *testing.T) {
	f := &fakeLists{lists: []scrobList{{ID: 7, Name: "MDBList - Watchlist"}}}
	client, done := f.serve(t)
	defer done()

	resp, err := NewServer(nil).listWatchlist(context.Background(), client)
	if err != nil || resp.GetFault() != nil {
		t.Fatalf("listWatchlist() = %v, %v", resp, err)
	}
	if resp.GetCompleteSnapshot() || len(resp.GetItems()) != 0 || len(f.writes) != 0 {
		t.Errorf("response = %+v, writes %v; want an empty incremental traversal and no writes", resp, f.writes)
	}
}

// The plugin's list is read as a complete snapshot of movies and whole
// series; a season, a person or another list's items are left out.
func TestListWatchlistReadsOnlyThePluginsList(t *testing.T) {
	season := 2
	f := &fakeLists{
		lists: []scrobList{{ID: 7, Name: "MDBList - Watchlist"}, {ID: 9, Name: watchlistName}},
		items: map[int][]scrobListItem{
			7: {{ID: 70, Media: scrobMedia{Type: "movie", TMDBID: 1}}},
			9: {
				{ID: 90, Media: scrobMedia{Type: "movie", TMDBID: 42, Title: "Heat"}, AddedAt: scrobTimePtr(timeUnix(100))},
				{ID: 91, Media: scrobMedia{Type: "series", TMDBID: 1399, TVDBID: 121361, Title: "GoT"}},
				{ID: 92, Media: scrobMedia{Type: "series", TMDBID: 1399, SeasonNumber: &season}},
				{ID: 93, Media: scrobMedia{Type: "person", TMDBID: 5}},
			},
		},
	}
	client, done := f.serve(t)
	defer done()

	resp, err := NewServer(nil).listWatchlist(context.Background(), client)
	if err != nil || resp.GetFault() != nil {
		t.Fatalf("listWatchlist() = %v, %v", resp, err)
	}
	if !resp.GetCompleteSnapshot() {
		t.Errorf("CompleteSnapshot = false, want true once the list exists")
	}
	items := resp.GetItems()
	if len(items) != 2 {
		t.Fatalf("items = %d, want the movie and the whole series", len(items))
	}
	if items[0].GetWatchlist().GetListedAt().AsTime() != timeUnix(100) || items[1].GetMedia().GetMediaType() != seriesType {
		t.Errorf("items = %+v", items)
	}
}

// The first add creates the plugin's list; another list with a similar
// purpose, such as MDBList's, is left alone.
func TestWriteWatchlistCreatesTheListOnTheFirstAdd(t *testing.T) {
	f := &fakeLists{lists: []scrobList{{ID: 7, Name: "MDBList - Watchlist"}}, nextID: 20}
	client, done := f.serve(t)
	defer done()

	r := writeOne(t, client, watchlistEvent(opAddToWatchlist, movieType, map[string]string{"tmdb": "42"}), false)
	want := []string{"create " + watchlistName, "add /lists/21/items movie"}
	if strings.Join(f.writes, ",") != strings.Join(want, ",") {
		t.Errorf("writes = %v, want %v", f.writes, want)
	}
	if r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Errorf("status = %v, want APPLIED", r.GetStatus())
	}
}

// Scrob answers 409 for an item already on the list.
func TestWriteWatchlistTreatsAnItemAlreadyListedAsNoChange(t *testing.T) {
	f := &fakeLists{lists: []scrobList{{ID: 9, Name: watchlistName}}, addStatus: http.StatusConflict}
	client, done := f.serve(t)
	defer done()

	r := writeOne(t, client, watchlistEvent(opAddToWatchlist, seriesType, map[string]string{"tvdb": "121361"}), false)
	if r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE {
		t.Errorf("status = %v, want NO_CHANGE", r.GetStatus())
	}
}

// A removal deletes only the matching row of the plugin's list.
func TestWriteWatchlistRemovesTheMatchingItem(t *testing.T) {
	f := &fakeLists{
		lists: []scrobList{{ID: 9, Name: watchlistName}},
		items: map[int][]scrobListItem{9: {
			{ID: 90, Media: scrobMedia{Type: "movie", TMDBID: 42}},
			{ID: 91, Media: scrobMedia{Type: "series", TMDBID: 42}},
		}},
	}
	client, done := f.serve(t)
	defer done()

	r := writeOne(t, client, watchlistEvent(opRemoveFromWatchlist, movieType, map[string]string{"tmdb": "42"}), true)
	if want := []string{"delete /lists/9/items/90"}; strings.Join(f.writes, ",") != strings.Join(want, ",") {
		t.Errorf("writes = %v, want %v", f.writes, want)
	}
	if r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Errorf("status = %v, want APPLIED", r.GetStatus())
	}
}

// Removing from a list that does not exist yet changes nothing and creates
// nothing.
func TestWriteWatchlistRemovalWithoutTheListIsNoChange(t *testing.T) {
	f := &fakeLists{}
	client, done := f.serve(t)
	defer done()

	r := writeOne(t, client, watchlistEvent(opRemoveFromWatchlist, movieType, map[string]string{"tmdb": "42"}), true)
	if r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE || len(f.writes) != 0 {
		t.Errorf("status = %v, writes %v; want NO_CHANGE and no writes", r.GetStatus(), f.writes)
	}
}

// Episodes have no place on the list, and a title needs an id Scrob can use.
func TestWriteWatchlistRejectsWhatItCannotList(t *testing.T) {
	f := &fakeLists{}
	client, done := f.serve(t)
	defer done()

	for _, event := range []*pluginv1.WatchSyncEvent{
		watchlistEvent(opAddToWatchlist, pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE, map[string]string{"tmdb": "1"}),
		watchlistEvent(opAddToWatchlist, movieType, map[string]string{"imdb": "tt0113277"}),
	} {
		if r := writeOne(t, client, event, false); r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
			t.Errorf("status = %v, want REJECTED", r.GetStatus())
		}
	}
	if len(f.writes) != 0 {
		t.Errorf("writes = %v, want none", f.writes)
	}
}

func timeUnix(sec int64) time.Time { return time.Unix(sec, 0).UTC() }
