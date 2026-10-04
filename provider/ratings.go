package provider

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"strconv"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// listRatings reads GET /ratings, which Scrob returns as a single unpaginated
// list - every rating the account has, movies, shows, seasons, and episodes
// together - so the read is always one complete snapshot with no page token.
//
// The endpoint only ever reports tmdb_id on the rated item, not imdb_id or
// tvdb_id even though Scrob stores both, so a TVDB-only rated show cannot be
// matched here: the plugin reports the rating as absent rather than
// importing it under the wrong identity, and the host never learns it is
// there to begin with.
func (s *Server) listRatings(ctx context.Context, client *apiClient) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
	var payload scrobRatingsResponse
	if fault := client.get(ctx, "/ratings", nil, &payload); fault != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
	}
	response := &pluginv1.WatchSyncListRemoteStateResponse{CompleteSnapshot: true}
	for _, entry := range payload.Results {
		item, ok := ratingStateFromEntry(entry)
		if !ok {
			continue
		}
		response.Items = append(response.Items, item)
	}
	return response, nil
}

// ratingStateFromEntry maps one rated Scrob title: a movie, a whole series, or
// a single episode. It reports false for a season rating, an unrated entry (a
// null or zero rating), and a title the plugin cannot identify.
//
// A season rating is stored against the same series Media row as a
// whole-series rating in Scrob, distinguished only by this entry's
// season_number, which sits beside media, not inside it - media.type stays
// "series" either way. Importing it under the series' key would collide with
// and potentially overwrite a real whole-series rating.
func ratingStateFromEntry(entry scrobRatingEntry) (*pluginv1.WatchSyncRemoteState, bool) {
	value := providerRating(entry.Rating)
	if value == 0 {
		return nil, false
	}
	// A season rating rides on the series' own media row, told apart only by
	// this field, so it would otherwise import over a whole-series rating.
	if entry.Media.Type == "series" && entry.SeasonNumber != nil {
		return nil, false
	}
	key, media, ok := stateMediaFromScrob(entry.Media)
	if !ok {
		return nil, false
	}
	rating := &pluginv1.WatchSyncRemoteRatingState{Rating: value}
	if at := entry.RatedAt.Time(); !at.IsZero() {
		rating.RatedAt = timestamppb.New(at)
	}
	return &pluginv1.WatchSyncRemoteState{ProviderItemKey: key, Media: media, Rating: rating}, true
}

// providerRating rounds a rating to the integer scale; 0 means unrated.
func providerRating(rating float64) int32 {
	value := math.Round(rating)
	if value == 0 || math.IsNaN(value) {
		return 0
	}
	return int32(min(max(value, 1), 10))
}

const errRatingNeedsExternalID = "Scrob rating sync requires a movie, series or episode with a TMDB or TVDB id"

// errRatingEpisodeUnknown is Scrob refusing to rate an episode it does not
// track yet: it will not create a media row for an episode from a rating
// alone. Nothing is wrong with the request, so the event is rejected with an
// explanation rather than failed and retried forever.
const errRatingEpisodeUnknown = "Scrob does not track this episode yet, so it cannot be rated there"

// writeRatings sets or clears movie and series ratings with one POST or
// DELETE /ratings per event: Scrob has no bulk ratings-write endpoint.
// Scrob replaces an existing rating, so resending one is harmless, and a
// miss on removal (Scrob already has no rating for the title) is reported
// NO_CHANGE rather than a failure.
func writeRatings(ctx context.Context, client *apiClient, events []*pluginv1.WatchSyncEvent, results *resultSet, removing bool) *pluginv1.WatchSyncFault {
	for _, event := range events {
		media := event.GetMedia()
		kind := mediaType(media.GetMediaType())
		if kind != "movie" && kind != "series" && kind != "episode" {
			results.reject(event, errRatingNeedsExternalID)
			continue
		}
		// An episode is addressed by its own id: Scrob's rating endpoint takes
		// no series or season fields for one, and finds the episode's media row
		// by tmdb_id, tvdb_id or media_id.
		ids := idsFromExternal(media.GetExternalIds())
		if ids.TMDB <= 0 && ids.TVDB <= 0 {
			results.reject(event, errRatingNeedsExternalID)
			continue
		}
		if removing {
			if fault := removeRating(ctx, client, kind, ids, results, event); fault != nil {
				return fault
			}
			continue
		}
		rating := event.GetRating()
		if rating < 1 || rating > 10 {
			results.reject(event, "Scrob ratings must be from 1 to 10")
			continue
		}
		body := scrobRatingIn{TMDBID: ids.TMDB, TVDBID: ids.TVDB, MediaType: kind, Rating: float64(rating)}
		status, fault := client.postStatus(ctx, "/ratings", body, nil)
		switch {
		case fault == nil:
			results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED)
		case kind == "episode" && status == http.StatusBadRequest:
			results.reject(event, errRatingEpisodeUnknown)
		case connectionWide(fault):
			return fault
		default:
			results.fail(event, fault)
		}
	}
	return nil
}

func removeRating(ctx context.Context, client *apiClient, kind string, ids scrobIDs, results *resultSet, event *pluginv1.WatchSyncEvent) *pluginv1.WatchSyncFault {
	query := url.Values{"media_type": {kind}}
	if ids.TMDB > 0 {
		query.Set("tmdb_id", strconv.Itoa(ids.TMDB))
	} else if ids.TVDB > 0 {
		query.Set("tvdb_id", strconv.Itoa(ids.TVDB))
	}
	status, fault := client.delete(ctx, "/ratings", query)
	switch {
	case fault == nil:
		results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED)
	case status == http.StatusNotFound:
		results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE)
	case connectionWide(fault):
		return fault
	default:
		results.fail(event, fault)
	}
	return nil
}
