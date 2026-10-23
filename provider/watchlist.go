package provider

import (
	"context"
	"net/http"
	"strconv"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Scrob has no single watchlist of its own, only named lists. It mirrors
// another service's watchlist as a list of its own, such as "MDBList -
// Watchlist", and the plugin does the same: Silo's watchlist lives in a list
// named watchlistName, created on the first export. The user's other lists,
// including ones mirroring other services, are never touched.
const watchlistName = "Silo - Watchlist"

const watchlistDescription = "Silo's watchlist, kept in sync by the Silo Scrob plugin."

const (
	errWatchlistNeedsMedia    = "Scrob watchlist sync takes movies and series only"
	errWatchlistNeedsID       = "Scrob watchlist sync requires a movie or series with a TMDB or TVDB id"
	errWatchlistTitleNotFound = "Scrob could not find this title to add to its watchlist"
)

type scrobList struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type scrobListsResponse struct {
	Lists []scrobList `json:"lists"`
}

type scrobListItem struct {
	ID      int        `json:"id"`
	AddedAt *scrobTime `json:"added_at"`
	Media   scrobMedia `json:"media"`
}

type scrobListDetail struct {
	Items []scrobListItem `json:"items"`
}

type scrobListIn struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type scrobListItemIn struct {
	TMDBID    int    `json:"tmdb_id,omitempty"`
	TVDBID    int    `json:"tvdb_id,omitempty"`
	MediaType string `json:"media_type"`
}

// findWatchlist returns the id of the plugin's list, or zero when there is
// none yet. Should two lists carry the name, the oldest one is used.
func findWatchlist(ctx context.Context, client *apiClient) (int, *pluginv1.WatchSyncFault) {
	var payload scrobListsResponse
	if fault := client.get(ctx, "/lists", nil, &payload); fault != nil {
		return 0, fault
	}
	found := 0
	for _, list := range payload.Lists {
		if list.Name == watchlistName && list.ID > 0 && (found == 0 || list.ID < found) {
			found = list.ID
		}
	}
	return found, nil
}

func ensureWatchlist(ctx context.Context, client *apiClient) (int, *pluginv1.WatchSyncFault) {
	id, fault := findWatchlist(ctx, client)
	if fault != nil || id > 0 {
		return id, fault
	}
	var created scrobList
	if fault := client.post(ctx, "/lists", scrobListIn{Name: watchlistName, Description: watchlistDescription}, &created); fault != nil {
		return 0, fault
	}
	if created.ID <= 0 {
		return 0, temporaryFault("Scrob did not return the new watchlist")
	}
	return created.ID, nil
}

func watchlistItems(ctx context.Context, client *apiClient, listID int) ([]scrobListItem, *pluginv1.WatchSyncFault) {
	var detail scrobListDetail
	if fault := client.get(ctx, "/lists/"+strconv.Itoa(listID), nil, &detail); fault != nil {
		return nil, fault
	}
	return detail.Items, nil
}

// listWatchlist reports the plugin's list as a complete snapshot. Silo removes
// an item from its watchlist only when the list held it before and no longer
// does, and only with watchlist removals on. Before the first export creates
// the list there is nothing to report, so the traversal is an empty
// incremental one rather than a snapshot claiming the watchlist is empty.
func (s *Server) listWatchlist(ctx context.Context, client *apiClient) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
	listID, fault := findWatchlist(ctx, client)
	if fault != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
	}
	if listID == 0 {
		return &pluginv1.WatchSyncListRemoteStateResponse{}, nil
	}
	items, fault := watchlistItems(ctx, client, listID)
	if fault != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
	}
	response := &pluginv1.WatchSyncListRemoteStateResponse{CompleteSnapshot: true}
	for _, item := range items {
		if !watchlistMedia(item.Media) {
			continue
		}
		key, media, ok := stateMediaFromScrob(item.Media)
		if !ok {
			continue
		}
		listed := &pluginv1.WatchSyncRemoteListState{}
		if item.AddedAt != nil && !item.AddedAt.Time().IsZero() {
			listed.ListedAt = timestamppb.New(item.AddedAt.Time())
		}
		response.Items = append(response.Items, &pluginv1.WatchSyncRemoteState{
			ProviderItemKey: key,
			Media:           media,
			Watchlist:       listed,
		})
	}
	return response, nil
}

// watchlistMedia reports a whole movie or series. A season of a series, or a
// person or episode the user put on the list in Scrob, has no place in Silo's
// watchlist.
func watchlistMedia(media scrobMedia) bool {
	switch media.Type {
	case "movie":
		return true
	case "series":
		return media.SeasonNumber == nil
	default:
		return false
	}
}

// writeWatchlist adds titles to the plugin's list, creating it on the first
// add, or removes them from it. Both are convergent: an item already there,
// or already gone, is NO_CHANGE.
func writeWatchlist(ctx context.Context, client *apiClient, events []*pluginv1.WatchSyncEvent, results *resultSet, removing bool) *pluginv1.WatchSyncFault {
	pending := make([]*pluginv1.WatchSyncEvent, 0, len(events))
	for _, event := range events {
		kind := mediaType(event.GetMedia().GetMediaType())
		ids := idsFromExternal(event.GetMedia().GetExternalIds())
		switch {
		case kind != "movie" && kind != "series":
			results.reject(event, errWatchlistNeedsMedia)
		case ids.TMDB <= 0 && ids.TVDB <= 0:
			results.reject(event, errWatchlistNeedsID)
		default:
			pending = append(pending, event)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	if removing {
		return removeFromWatchlist(ctx, client, pending, results)
	}
	listID, fault := ensureWatchlist(ctx, client)
	if fault != nil {
		return failAll(pending, results, fault)
	}
	for _, event := range pending {
		ids := idsFromExternal(event.GetMedia().GetExternalIds())
		body := scrobListItemIn{TMDBID: ids.TMDB, MediaType: mediaType(event.GetMedia().GetMediaType())}
		if ids.TMDB <= 0 {
			body.TVDBID = ids.TVDB
		}
		status, fault := client.postStatus(ctx, "/lists/"+strconv.Itoa(listID)+"/items", body, nil)
		switch {
		case fault == nil:
			results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED)
		case status == http.StatusConflict:
			results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE)
		case status == http.StatusNotFound:
			results.reject(event, errWatchlistTitleNotFound)
		case connectionWide(fault):
			return fault
		default:
			results.fail(event, fault)
		}
	}
	return nil
}

func removeFromWatchlist(ctx context.Context, client *apiClient, events []*pluginv1.WatchSyncEvent, results *resultSet) *pluginv1.WatchSyncFault {
	listID, fault := findWatchlist(ctx, client)
	if fault != nil {
		return failAll(events, results, fault)
	}
	if listID == 0 {
		for _, event := range events {
			results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE)
		}
		return nil
	}
	items, fault := watchlistItems(ctx, client, listID)
	if fault != nil {
		return failAll(events, results, fault)
	}
	for _, event := range events {
		status := pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE
		for _, item := range items {
			if !sameWatchlistTitle(item.Media, event.GetMedia()) {
				continue
			}
			code, fault := client.delete(ctx, "/lists/"+strconv.Itoa(listID)+"/items/"+strconv.Itoa(item.ID), nil)
			if fault != nil && code != http.StatusNotFound {
				if connectionWide(fault) {
					return fault
				}
				results.fail(event, fault)
				status = 0
				break
			}
			status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED
		}
		if status != 0 {
			results.set(event, status)
		}
	}
	return nil
}

// sameWatchlistTitle matches a list row to the title an event names, by type
// and by TMDB or TVDB id.
func sameWatchlistTitle(item scrobMedia, media *pluginv1.WatchSyncMedia) bool {
	if !watchlistMedia(item) || item.Type != mediaType(media.GetMediaType()) {
		return false
	}
	ids := idsFromExternal(media.GetExternalIds())
	return (ids.TMDB > 0 && item.TMDBID == ids.TMDB) || (ids.TVDB > 0 && item.TVDBID == ids.TVDB)
}

// failAll reports a fault for every event, or returns it when it ends the
// whole call.
func failAll(events []*pluginv1.WatchSyncEvent, results *resultSet, fault *pluginv1.WatchSyncFault) *pluginv1.WatchSyncFault {
	if connectionWide(fault) {
		return fault
	}
	for _, event := range events {
		results.fail(event, fault)
	}
	return nil
}
