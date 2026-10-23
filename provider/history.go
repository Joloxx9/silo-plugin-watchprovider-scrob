package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// historyPageSize is Scrob's largest GET /history page.
	historyPageSize = 100
	// Scrob has no "changed since" filter, and a play imported into Scrob from
	// another service can be dated in the past. An incremental import reads
	// the newest plays back to this far before the newest one it saw; a full
	// import, run at most once per fullImportInterval, catches older additions.
	incrementalOverlap = 48 * time.Hour
	fullImportInterval = 24 * time.Hour
)

// watchedCursor is the durable checkpoint the host keeps between imports.
type watchedCursor struct {
	// Newest is the newest play time any finished import saw.
	Newest time.Time `json:"newest"`
	// FullAt is when the last finished full import started.
	FullAt time.Time `json:"full_at"`
}

// watchedPage carries one import from page to page.
type watchedPage struct {
	Full      bool      `json:"full"`
	Page      int       `json:"page"`
	Newest    time.Time `json:"newest"`
	StopAt    time.Time `json:"stop_at,omitempty"`
	StartedAt time.Time `json:"started_at"`
	PrevFull  time.Time `json:"prev_full,omitempty"`
}

// listWatched returns one page of Scrob's history, newest first, per call. The
// host pages through with next_page_token, so a large history no longer has to
// fit in one call's time limit.
//
// After the first import, most imports are incremental: they stop at plays
// older than the cursor's newest play less incrementalOverlap. The host keeps
// the newest play per title and skips a play it already has, so neither the
// overlap nor a title appearing on several pages adds plays. Absent titles
// mean nothing, so no traversal is a complete snapshot.
func (s *Server) listWatched(ctx context.Context, client *apiClient, req *pluginv1.WatchSyncListRemoteStateRequest) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
	page, fault := s.watchedTraversal(req)
	if fault != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, syncRequestLimit(ctx))
	defer cancel()

	size := historyPageSize
	if requested := int(req.GetPageSize()); requested > 0 {
		size = min(size, requested)
	}
	query := url.Values{"page": {strconv.Itoa(page.Page)}, "page_size": {strconv.Itoa(size)}}
	var payload scrobHistoryResponse
	if fault := client.get(ctx, "/history", query, &payload); fault != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
	}

	type aggregate struct {
		item      *pluginv1.WatchSyncRemoteState
		playCount int32
	}
	aggregated := make(map[string]*aggregate)
	order := make([]string, 0, len(payload.Results))
	finished := len(payload.Results) < size || page.Page >= payload.TotalPages
	for _, event := range payload.Results {
		if event.WatchedAt == nil {
			// Plays without a time sort last and cannot be imported.
			finished = true
			break
		}
		at := event.WatchedAt.Time()
		if !page.Full && at.Before(page.StopAt) {
			finished = true
			break
		}
		if at.After(page.Newest) {
			page.Newest = at
		}
		item, key, ok := watchedStateFromEvent(event)
		if !ok {
			continue
		}
		// The page is newest first, so a title's first play here is its latest.
		if entry, exists := aggregated[key]; exists {
			entry.playCount++
			continue
		}
		aggregated[key] = &aggregate{item: item, playCount: 1}
		order = append(order, key)
	}

	response := &pluginv1.WatchSyncListRemoteStateResponse{}
	for _, key := range order {
		entry := aggregated[key]
		entry.item.Watched.PlayCount = entry.playCount
		response.Items = append(response.Items, entry.item)
	}
	if finished {
		next := watchedCursor{Newest: page.Newest, FullAt: page.PrevFull}
		if page.Full {
			next.FullAt = page.StartedAt
		}
		response.NextCursor = encodeToken(next)
		return response, nil
	}
	page.Page++
	response.NextPageToken = encodeToken(page)
	return response, nil
}

// watchedTraversal resumes the import a page token names, or starts one from
// the cursor: a full import when there is no usable cursor or the last full
// import is fullImportInterval old, an incremental one otherwise.
func (s *Server) watchedTraversal(req *pluginv1.WatchSyncListRemoteStateRequest) (watchedPage, *pluginv1.WatchSyncFault) {
	if strings.TrimSpace(req.GetPageToken()) != "" {
		var page watchedPage
		if !decodeToken(req.GetPageToken(), &page) || page.Page < 2 || page.StartedAt.IsZero() {
			return watchedPage{}, invalidRequestFault("Scrob watched history page token is invalid")
		}
		return page, nil
	}
	var cursor watchedCursor
	_ = decodeToken(req.GetCursor(), &cursor)
	now := s.clock().UTC()
	page := watchedPage{Page: 1, Newest: cursor.Newest, StartedAt: now, PrevFull: cursor.FullAt}
	page.Full = cursor.FullAt.IsZero() || cursor.Newest.IsZero() || now.Sub(cursor.FullAt) >= fullImportInterval
	if !page.Full {
		page.StopAt = cursor.Newest.Add(-incrementalOverlap)
	}
	return page, nil
}

func encodeToken(value any) string {
	encoded, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeToken(value string, into any) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || len(decoded) == 0 {
		return false
	}
	return json.Unmarshal(decoded, into) == nil
}

// watchedStateFromEvent maps one completed history event to a WATCHED state
// with play_count 1; listWatched folds a title's repeats on one page into one
// entry. It reports false for an event Scrob did not finish, or whose
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

// markWatched exports the plays Silo's history reconciliation sends.
//
// That history is not only Silo's own playback: it also holds every play Silo
// imported from elsewhere, such as a media server's history, usually stamped
// with a different time than Scrob has for the same viewing. Scrob would take
// each as a new play and push it to every service connected to it. A play from
// before exportSince, set when the profile connected, is history Silo already
// had, so it is skipped. A later play Scrob already holds is skipped too.
func markWatched(ctx context.Context, client *apiClient, events []*pluginv1.WatchSyncEvent, results *resultSet, exportSince time.Time) *pluginv1.WatchSyncFault {
	for _, event := range events {
		body, ok := watchEventBody(event)
		if !ok {
			results.reject(event, unsupportedPlayMessage)
			continue
		}
		if occurred := event.GetOccurredAt(); occurred != nil && occurred.CheckValid() == nil &&
			occurred.AsTime().Before(exportSince) {
			results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE)
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
// Silo sends one unwatch per history row it removed, with that play's time, so
// only the Scrob play at that time is deleted. Scrob keeps a row per viewing,
// and a title's other plays, including ones Silo never had, must survive.
// When no Scrob play is close to that time there is nothing of Silo's to
// remove, so the event changes nothing.
func markUnwatched(ctx context.Context, client *apiClient, events []*pluginv1.WatchSyncEvent, results *resultSet) *pluginv1.WatchSyncFault {
	for _, event := range events {
		eventID, found, fault := scrobPlayAt(ctx, client, event)
		switch {
		case fault != nil && connectionWide(fault):
			return fault
		case fault != nil:
			results.fail(event, fault)
			continue
		case !found:
			results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE)
			continue
		}
		status, fault := client.delete(ctx, fmt.Sprintf("/history/event/%d", eventID), nil)
		switch {
		case fault == nil:
			results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED)
		case status == http.StatusNotFound:
			// The play is already gone: the desired state.
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
	body := scrobWatchEventCreate{TMDBID: ids.TMDB, TVDBID: ids.TVDB, MediaType: kind, Completed: true, Source: scrobbleSource}
	if occurred := event.GetOccurredAt(); occurred != nil && occurred.CheckValid() == nil && !occurred.AsTime().IsZero() {
		body.WatchedAt = occurred.AsTime().UTC().Format(time.RFC3339)
	}
	if kind == "episode" {
		// Scrob finds an episode by its show and numbers when it has them,
		// which also creates an episode it has not seen; without series ids it
		// looks the episode up by its own id.
		seriesIDs := idsFromExternal(media.GetSeriesExternalIds())
		if seriesIDs.TMDB <= 0 && seriesIDs.TVDB <= 0 {
			return body, true
		}
		body.SeriesTMDBID = seriesIDs.TMDB
		body.SeriesTVDBID = seriesIDs.TVDB
		season, episode := int(media.GetSeasonNumber()), int(media.GetEpisodeNumber())
		body.SeasonNumber = &season
		body.EpisodeNumber = &episode
	}
	return body, true
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
// lookup the duplicate check already relies on. An event without a time or a
// title Scrob can look up is rejected: there is no single play to name.
func scrobPlayAt(ctx context.Context, client *apiClient, event *pluginv1.WatchSyncEvent) (int, bool, *pluginv1.WatchSyncFault) {
	occurred := event.GetOccurredAt()
	if occurred == nil || occurred.CheckValid() != nil || occurred.AsTime().IsZero() {
		return 0, false, invalidRequestFault("Scrob unwatch needs the play's watch time")
	}
	query, ok := itemEventsQuery(event.GetMedia())
	if !ok {
		return 0, false, invalidRequestFault(unsupportedPlayMessage)
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
