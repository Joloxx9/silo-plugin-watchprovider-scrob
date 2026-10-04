package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const historyPageSize = 100

// listWatched reads every page of Scrob's GET /history and aggregates it into
// one state per title: Scrob returns individual watch events, potentially
// several per title across many pages, and the host's WATCHED state is a
// per-title play_count and last_watched_at rather than a per-event log, so
// the aggregation has to happen before any of it is returned. This traversal
// is therefore always a single, complete snapshot: there is no durable
// cursor and no next_page_token, bounded by the same time box as an
// ApplyEvents call.
func (s *Server) listWatched(ctx context.Context, client *apiClient, req *pluginv1.WatchSyncListRemoteStateRequest) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
	if strings.TrimSpace(req.GetPageToken()) != "" {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: invalidRequestFault("Scrob watched history has no continuation token")}, nil
	}
	budget := syncTimeBox(ctx)
	ctx, cancel := context.WithTimeout(ctx, syncRequestLimit(ctx))
	defer cancel()
	startedAt := s.clock()

	type aggregate struct {
		item      *pluginv1.WatchSyncRemoteState
		playCount int32
		lastAt    *timestamppb.Timestamp
	}
	aggregated := make(map[string]*aggregate)
	order := make([]string, 0)

	for page := 1; ; page++ {
		if s.clock().Sub(startedAt) >= budget {
			return &pluginv1.WatchSyncListRemoteStateResponse{Fault: temporaryFault("Scrob watched history is large; the sync will retry")}, nil
		}
		query := url.Values{"page": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(historyPageSize)}}
		var payload scrobHistoryResponse
		if fault := client.get(ctx, "/history", query, &payload); fault != nil {
			return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
		}
		for _, event := range payload.Results {
			item, key, ok := watchedStateFromEvent(event)
			if !ok {
				continue
			}
			entry, exists := aggregated[key]
			if !exists {
				aggregated[key] = &aggregate{item: item, playCount: 1, lastAt: item.GetWatched().GetLastWatchedAt()}
				order = append(order, key)
				continue
			}
			entry.playCount++
			if at := item.GetWatched().GetLastWatchedAt(); at != nil && (entry.lastAt == nil || at.AsTime().After(entry.lastAt.AsTime())) {
				entry.lastAt = at
			}
		}
		if page >= payload.TotalPages || len(payload.Results) == 0 {
			break
		}
	}

	response := &pluginv1.WatchSyncListRemoteStateResponse{CompleteSnapshot: true}
	for _, key := range order {
		entry := aggregated[key]
		entry.item.Watched = &pluginv1.WatchSyncRemoteWatchedState{PlayCount: entry.playCount, LastWatchedAt: entry.lastAt}
		response.Items = append(response.Items, entry.item)
	}
	return response, nil
}

// watchedStateFromEvent maps one completed history event to a WATCHED state
// with play_count 1; listWatched folds repeats across events into one entry
// per title. It reports false for an event Scrob did not finish, or whose
// title the plugin cannot identify.
func watchedStateFromEvent(event scrobHistoryEvent) (*pluginv1.WatchSyncRemoteState, string, bool) {
	if event.WatchedAt == nil {
		return nil, "", false
	}
	media := event.Media
	watchedAt := timestamppb.New(event.WatchedAt.Time())
	switch media.Type {
	case "movie":
		ids := scrobIDs{TMDB: media.TMDBID, TVDB: media.TVDBID, IMDb: media.IMDbID}
		key := movieKey(ids)
		if key == "" {
			return nil, "", false
		}
		return &pluginv1.WatchSyncRemoteState{
			ProviderItemKey: key,
			Media: &pluginv1.WatchSyncMedia{
				MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
				Title:       media.Title,
				ExternalIds: externalIDsFromScrob(ids),
			},
			Watched: &pluginv1.WatchSyncRemoteWatchedState{PlayCount: 1, LastWatchedAt: watchedAt},
		}, "movie\x00" + key, true
	case "episode":
		if media.SeasonNumber == nil || media.EpisodeNumber == nil {
			return nil, "", false
		}
		showIDs := scrobIDs{TMDB: media.ShowTMDBID, TVDB: media.ShowTVDBID}
		episodeIDs := scrobIDs{TMDB: media.TMDBID, TVDB: media.TVDBID, IMDb: media.IMDbID}
		key := episodeKey(showIDs, *media.SeasonNumber, *media.EpisodeNumber, episodeIDs)
		if key == "" {
			return nil, "", false
		}
		return &pluginv1.WatchSyncRemoteState{
			ProviderItemKey: key,
			Media: &pluginv1.WatchSyncMedia{
				MediaType:         pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
				Title:             media.Title,
				ExternalIds:       externalIDsFromScrob(episodeIDs),
				SeriesTitle:       media.ShowTitle,
				SeriesExternalIds: externalIDsFromScrob(showIDs),
				SeasonNumber:      int32(*media.SeasonNumber),
				EpisodeNumber:     int32(*media.EpisodeNumber),
			},
			Watched: &pluginv1.WatchSyncRemoteWatchedState{PlayCount: 1, LastWatchedAt: watchedAt},
		}, "episode\x00" + key, true
	default:
		return nil, "", false
	}
}

const unsupportedPlayMessage = "Scrob watched sync requires a movie or episode with a TMDB or TVDB id"

// recentWatchWindow is how far apart two plays of the same title may be and
// still be the same play. Scrob stores a webhook's play at the moment it ends
// and Silo stamps its own at the moment it recorded the watch, so the two
// never match to the second.
const recentWatchWindow = 10 * time.Minute

func markWatched(ctx context.Context, client *apiClient, events []*pluginv1.WatchSyncEvent, results *resultSet) *pluginv1.WatchSyncFault {
	for _, event := range events {
		body, ok := watchEventBody(event)
		if !ok {
			results.reject(event, unsupportedPlayMessage)
			continue
		}
		held, fault := scrobAlreadyHasPlay(ctx, client, event)
		if fault != nil && connectionWide(fault) {
			return fault
		}
		if held {
			results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE)
			continue
		}
		if fault := client.post(ctx, "/history", body, nil); fault != nil {
			if connectionWide(fault) {
				return fault
			}
			results.fail(event, fault)
			continue
		}
		results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED)
	}
	return nil
}

// markUnwatched removes the play Silo no longer holds.
//
// When the event says which play it was, only that one is deleted: Scrob keeps
// a row per viewing, and dropping the title's whole history because one
// rewatch was undone would destroy counts Silo never asked about. Without a
// timestamp there is nothing to single out, so the title's history is removed
// as before.
func markUnwatched(ctx context.Context, client *apiClient, events []*pluginv1.WatchSyncEvent, results *resultSet) *pluginv1.WatchSyncFault {
	for _, event := range events {
		path, itemQuery, ok := unwatchItemPath(event.GetMedia())
		if !ok {
			results.reject(event, unsupportedPlayMessage)
			continue
		}
		query := itemQuery
		if eventID, found, fault := scrobPlayAt(ctx, client, event); fault != nil && connectionWide(fault) {
			return fault
		} else if found {
			path, query = fmt.Sprintf("/history/event/%d", eventID), nil
		}
		status, fault := client.delete(ctx, path, query)
		switch {
		case fault == nil:
			results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED)
		case status == http.StatusNotFound:
			// Scrob has no watch event for this item: already the desired state.
			results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE)
		case connectionWide(fault):
			return fault
		default:
			results.fail(event, fault)
		}
	}
	return nil
}

func watchEventBody(event *pluginv1.WatchSyncEvent) (scrobWatchEventCreate, bool) {
	media := event.GetMedia()
	ids := idsFromExternal(media.GetExternalIds())
	kind := mediaType(media.GetMediaType())
	if kind != "movie" && kind != "episode" {
		return scrobWatchEventCreate{}, false
	}
	if ids.empty() {
		return scrobWatchEventCreate{}, false
	}
	body := scrobWatchEventCreate{TMDBID: ids.TMDB, TVDBID: ids.TVDB, MediaType: kind, Completed: true}
	if occurred := event.GetOccurredAt(); occurred != nil && occurred.CheckValid() == nil && !occurred.AsTime().IsZero() {
		body.WatchedAt = occurred.AsTime().UTC().Format(time.RFC3339)
	}
	if kind == "episode" {
		seriesIDs := idsFromExternal(media.GetSeriesExternalIds())
		if seriesIDs.TMDB <= 0 && seriesIDs.TVDB <= 0 {
			return scrobWatchEventCreate{}, false
		}
		body.SeriesTMDBID = seriesIDs.TMDB
		body.SeriesTVDBID = seriesIDs.TVDB
		season, episode := int(media.GetSeasonNumber()), int(media.GetEpisodeNumber())
		body.SeasonNumber = &season
		body.EpisodeNumber = &episode
	}
	return body, true
}

// unwatchItemPath names the title to remove. The query is returned separately
// because the request builder escapes the path: a "?" folded into it becomes
// %3F, the route never matches, and Scrob's 404 reads as "already unwatched".
func unwatchItemPath(media *pluginv1.WatchSyncMedia) (string, url.Values, bool) {
	kind := mediaType(media.GetMediaType())
	if kind != "movie" && kind != "episode" {
		return "", nil, false
	}
	ids := idsFromExternal(media.GetExternalIds())
	if ids.TMDB <= 0 && ids.TVDB <= 0 {
		return "", nil, false
	}
	query := url.Values{"media_type": {kind}}
	if ids.TMDB > 0 {
		query.Set("tmdb_id", strconv.Itoa(ids.TMDB))
	}
	if ids.TVDB > 0 {
		query.Set("tvdb_id", strconv.Itoa(ids.TVDB))
	}
	return "/history/item", query, true
}

// scrobAlreadyHasPlay reports whether Scrob already holds this play.
//
// It asks for the title's own watch events rather than scanning recent
// history: a play Scrob recorded from a live scrobble is recent, but a play it
// holds from another source may be far down the log, and missing it would
// export a duplicate.
//
// A failed lookup loses the check rather than the export: refusing to export
// at all would be worse than risking a duplicate.
func scrobAlreadyHasPlay(ctx context.Context, client *apiClient, event *pluginv1.WatchSyncEvent) (bool, *pluginv1.WatchSyncFault) {
	occurred := event.GetOccurredAt()
	if occurred == nil || occurred.CheckValid() != nil || occurred.AsTime().IsZero() {
		return false, nil
	}
	query, ok := itemEventsQuery(event.GetMedia())
	if !ok {
		return false, nil
	}
	var payload scrobItemEventsResponse
	if fault := client.get(ctx, "/history/item-events", query, &payload); fault != nil {
		return false, fault
	}
	want := occurred.AsTime()
	for _, held := range payload.Events {
		if held.WatchedAt == nil {
			continue
		}
		at := held.WatchedAt.Time()
		if at.IsZero() {
			continue
		}
		if at.Sub(want) <= recentWatchWindow && want.Sub(at) <= recentWatchWindow {
			return true, nil
		}
	}
	return false, nil
}

// itemEventsQuery names the title the way Scrob's per-item lookup expects. A
// TVDB-only episode has no TMDB id, so either identifier will do.
func itemEventsQuery(media *pluginv1.WatchSyncMedia) (url.Values, bool) {
	kind := mediaType(media.GetMediaType())
	if kind != "movie" && kind != "episode" {
		return nil, false
	}
	ids := idsFromExternal(media.GetExternalIds())
	query := url.Values{"media_type": {kind}}
	switch {
	case ids.TMDB > 0:
		query.Set("tmdb_id", strconv.Itoa(ids.TMDB))
	case ids.TVDB > 0:
		query.Set("tvdb_id", strconv.Itoa(ids.TVDB))
	default:
		return nil, false
	}
	if kind == "episode" {
		seriesIDs := idsFromExternal(media.GetSeriesExternalIds())
		if seriesIDs.TMDB > 0 {
			query.Set("series_tmdb_id", strconv.Itoa(seriesIDs.TMDB))
		}
		if seriesIDs.TVDB > 0 {
			query.Set("series_tvdb_id", strconv.Itoa(seriesIDs.TVDB))
		}
	}
	return query, true
}

// scrobPlayAt finds the Scrob watch event for this play, so an unwatch removes
// one viewing rather than the title's whole history. It reuses the per-title
// lookup the duplicate check already relies on.
//
// A missing timestamp, an unidentifiable title or a failed lookup simply means
// no single play was found, and the caller falls back to removing the item.
func scrobPlayAt(ctx context.Context, client *apiClient, event *pluginv1.WatchSyncEvent) (int, bool, *pluginv1.WatchSyncFault) {
	occurred := event.GetOccurredAt()
	if occurred == nil || occurred.CheckValid() != nil || occurred.AsTime().IsZero() {
		return 0, false, nil
	}
	query, ok := itemEventsQuery(event.GetMedia())
	if !ok {
		return 0, false, nil
	}
	var payload scrobItemEventsResponse
	if fault := client.get(ctx, "/history/item-events", query, &payload); fault != nil {
		return 0, false, fault
	}
	want := occurred.AsTime()
	for _, held := range payload.Events {
		if held.WatchedAt == nil || held.ID == 0 {
			continue
		}
		at := held.WatchedAt.Time()
		if at.IsZero() {
			continue
		}
		if at.Sub(want) <= recentWatchWindow && want.Sub(at) <= recentWatchWindow {
			return held.ID, true, nil
		}
	}
	return 0, false, nil
}
