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

func markWatched(ctx context.Context, client *apiClient, events []*pluginv1.WatchSyncEvent, results *resultSet) *pluginv1.WatchSyncFault {
	for _, event := range events {
		body, ok := watchEventBody(event)
		if !ok {
			results.reject(event, unsupportedPlayMessage)
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

func markUnwatched(ctx context.Context, client *apiClient, events []*pluginv1.WatchSyncEvent, results *resultSet) *pluginv1.WatchSyncFault {
	for _, event := range events {
		path, ok := unwatchItemPath(event.GetMedia())
		if !ok {
			results.reject(event, unsupportedPlayMessage)
			continue
		}
		status, fault := client.delete(ctx, path, nil)
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

func unwatchItemPath(media *pluginv1.WatchSyncMedia) (string, bool) {
	kind := mediaType(media.GetMediaType())
	if kind != "movie" && kind != "episode" {
		return "", false
	}
	ids := idsFromExternal(media.GetExternalIds())
	if ids.TMDB <= 0 && ids.TVDB <= 0 {
		return "", false
	}
	query := url.Values{"media_type": {kind}}
	if ids.TMDB > 0 {
		query.Set("tmdb_id", strconv.Itoa(ids.TMDB))
	}
	if ids.TVDB > 0 {
		query.Set("tvdb_id", strconv.Itoa(ids.TVDB))
	}
	return fmt.Sprintf("/history/item?%s", query.Encode()), true
}
