package provider

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// Silo's dropped traversal is series-only: the contract states that a dropped
// write addresses a SERIES item and that a provider advertising sync_dropped
// must list SERIES among its media types. Scrob drops movies too, in a
// separate dropped_movies list, but Silo has nowhere to put one, so the movie
// half of Scrob's response is read and discarded rather than imported under a
// series key it does not belong to.
type scrobDroppedResponse struct {
	Shows  []scrobDroppedShow `json:"shows"`
	Movies []struct{}         `json:"movies"`
}

type scrobDroppedShow struct {
	ID     int    `json:"id"`
	TMDBID int    `json:"tmdb_id"`
	TVDBID int    `json:"tvdb_id"`
	Title  string `json:"title"`
}

// listDropped reads GET /history/dropped, which Scrob answers with every
// dropped show and movie in one unpaginated payload, so the read is always a
// complete snapshot with no page token. A series absent from a complete
// snapshot is not dropped, which is how an undrop made in Scrob reaches Silo
// without Scrob having to keep a tombstone.
//
// Scrob stores dropped shows as bare ids with no timestamps, so listed_at is
// left unset rather than invented from the response order.
func (s *Server) listDropped(ctx context.Context, client *apiClient) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
	var payload scrobDroppedResponse
	if fault := client.get(ctx, "/history/dropped", nil, &payload); fault != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
	}
	response := &pluginv1.WatchSyncListRemoteStateResponse{CompleteSnapshot: true}
	for _, show := range payload.Shows {
		key, media, ok := stateMediaFromScrob(scrobMedia{
			Type:   "series",
			TMDBID: show.TMDBID,
			TVDBID: show.TVDBID,
			Title:  show.Title,
		})
		if !ok {
			continue
		}
		response.Items = append(response.Items, &pluginv1.WatchSyncRemoteState{
			ProviderItemKey: key,
			Media:           media,
			Dropped:         &pluginv1.WatchSyncRemoteListState{},
		})
	}
	return response, nil
}

const errDroppedNeedsSeries = "Scrob only tracks a dropped show for a series"

const errDroppedNeedsExternalID = "Scrob dropped-show sync requires a series with a TMDB or TVDB id"

// errDroppedShowUnknown is Scrob having no show row for the series. Dropping
// addresses Scrob's own show id, and Scrob will not create a show from a drop
// alone, so the event is rejected with an explanation rather than failed and
// retried forever.
const errDroppedShowUnknown = "Scrob does not track this series yet, so it cannot be dropped there"

// writeDropped marks a series dropped or undropped with one write per event:
// Scrob has no bulk endpoint for either. Both directions are convergent, so
// Scrob already holding the desired state is reported NO_CHANGE rather than a
// failure.
func writeDropped(ctx context.Context, client *apiClient, events []*pluginv1.WatchSyncEvent, results *resultSet, removing bool) *pluginv1.WatchSyncFault {
	for _, event := range events {
		media := event.GetMedia()
		if mediaType(media.GetMediaType()) != "series" {
			results.reject(event, errDroppedNeedsSeries)
			continue
		}
		ids := idsFromExternal(media.GetExternalIds())
		if ids.TMDB <= 0 && ids.TVDB <= 0 {
			results.reject(event, errDroppedNeedsExternalID)
			continue
		}
		showID, fault := resolveShowID(ctx, client, ids)
		if fault != nil {
			if connectionWide(fault) {
				return fault
			}
			results.fail(event, fault)
			continue
		}
		if showID <= 0 {
			results.reject(event, errDroppedShowUnknown)
			continue
		}
		var status int
		if removing {
			status, fault = client.delete(ctx, "/history/drop/show", url.Values{"show_id": {strconv.Itoa(showID)}})
		} else {
			status, fault = client.postStatus(ctx, "/history/drop/show", scrobDropShowIn{ShowID: showID}, nil)
		}
		switch {
		case fault == nil:
			results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED)
		case status == http.StatusNotFound:
			// Scrob answers 404 when it has no settings row or no such show,
			// which for a convergent write means there is nothing to change.
			results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE)
		case connectionWide(fault):
			return fault
		default:
			results.fail(event, fault)
		}
	}
	return nil
}

type scrobDropShowIn struct {
	ShowID int `json:"show_id"`
}

// scrobShowLookup is the part of GET /shows/{tmdb_id} this plugin needs: the
// local show row's own id, which the drop endpoints address.
type scrobShowLookup struct {
	ID int `json:"id"`
}

// resolveShowID turns an external series identity into Scrob's own show id,
// which is the only thing /history/drop/show accepts.
//
// The lookup is keyed by TMDB id, so a TVDB-only series falls back to the
// dropped list, which carries both ids beside Scrob's id. That fallback can
// only answer for a series that is already dropped, which is exactly the case
// an undrop needs; a TVDB-only series that is not dropped yet cannot be
// resolved and its event is rejected rather than guessed at.
//
// A zero id with no fault means "not resolvable", not "error".
func resolveShowID(ctx context.Context, client *apiClient, ids scrobIDs) (int, *pluginv1.WatchSyncFault) {
	if ids.TMDB > 0 {
		var lookup scrobShowLookup
		fault := client.get(ctx, "/shows/"+strconv.Itoa(ids.TMDB), nil, &lookup)
		if fault == nil && lookup.ID > 0 {
			return lookup.ID, nil
		}
		if fault != nil && !lookupMiss(fault) {
			return 0, fault
		}
	}
	var dropped scrobDroppedResponse
	if fault := client.get(ctx, "/history/dropped", nil, &dropped); fault != nil {
		if !lookupMiss(fault) {
			return 0, fault
		}
		return 0, nil
	}
	for _, show := range dropped.Shows {
		if show.ID <= 0 {
			continue
		}
		if ids.TMDB > 0 && show.TMDBID == ids.TMDB {
			return show.ID, nil
		}
		if ids.TVDB > 0 && show.TVDBID == ids.TVDB {
			return show.ID, nil
		}
	}
	return 0, nil
}

// lookupMiss reports a lookup Scrob answered with a 4xx, meaning it has no
// such show. Anything else, such as Scrob being briefly unavailable, is
// returned so the event is retried rather than rejected as an unknown show.
func lookupMiss(fault *pluginv1.WatchSyncFault) bool {
	return fault.GetCode() == pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST
}
