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

// ratingStateFromEntry maps one rated Scrob title. It reports false for a
// season or episode rating (Silo rates movies and series only), an unrated
// entry (a null or zero rating), and a title the plugin cannot identify.
//
// A season rating is stored against the same series Media row as a
// whole-series rating in Scrob, distinguished only by this entry's
// season_number, which sits beside media, not inside it - media.type stays
// "series" either way. Importing it under the series' key would collide with
// and potentially overwrite a real whole-series rating.
func ratingStateFromEntry(entry scrobRatingEntry) (*pluginv1.WatchSyncRemoteState, bool) {
	kind := entry.Media.Type
	if kind != "movie" && kind != "series" {
		return nil, false
	}
	if kind == "series" && entry.SeasonNumber != nil {
		return nil, false
	}
	value := providerRating(entry.Rating)
	if value == 0 {
		return nil, false
	}
	ids := scrobIDs{TMDB: entry.Media.TMDBID}
	var key string
	var mt pluginv1.WatchSyncMediaType
	if kind == "series" {
		key = showKey(ids)
		mt = pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES
	} else {
		key = movieKey(ids)
		mt = pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE
	}
	if key == "" {
		return nil, false
	}
	rating := &pluginv1.WatchSyncRemoteRatingState{Rating: value}
	if at := entry.RatedAt.Time(); !at.IsZero() {
		rating.RatedAt = timestamppb.New(at)
	}
	return &pluginv1.WatchSyncRemoteState{
		ProviderItemKey: key,
		Media: &pluginv1.WatchSyncMedia{
			MediaType:   mt,
			Title:       entry.Media.Title,
			ExternalIds: externalIDsFromScrob(ids),
		},
		Rating: rating,
	}, true
}

// providerRating rounds a rating to the integer scale; 0 means unrated.
func providerRating(rating float64) int32 {
	value := math.Round(rating)
	if value == 0 || math.IsNaN(value) {
		return 0
	}
	return int32(min(max(value, 1), 10))
}

const errRatingNeedsExternalID = "Scrob rating sync requires a movie or series with a TMDB id"

// writeRatings sets or clears movie and series ratings with one POST or
// DELETE /ratings per event: Scrob has no bulk ratings-write endpoint.
// Scrob replaces an existing rating, so resending one is harmless, and a
// miss on removal (Scrob already has no rating for the title) is reported
// NO_CHANGE rather than a failure.
func writeRatings(ctx context.Context, client *apiClient, events []*pluginv1.WatchSyncEvent, results *resultSet, removing bool) *pluginv1.WatchSyncFault {
	for _, event := range events {
		media := event.GetMedia()
		kind := mediaType(media.GetMediaType())
		if kind != "movie" && kind != "series" {
			results.reject(event, errRatingNeedsExternalID)
			continue
		}
		ids := idsFromExternal(media.GetExternalIds())
		if ids.TMDB <= 0 {
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
		body := scrobRatingIn{TMDBID: ids.TMDB, MediaType: kind, Rating: float64(rating)}
		if fault := client.post(ctx, "/ratings", body, nil); fault != nil {
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

func removeRating(ctx context.Context, client *apiClient, kind string, ids scrobIDs, results *resultSet, event *pluginv1.WatchSyncEvent) *pluginv1.WatchSyncFault {
	query := url.Values{"media_type": {kind}, "tmdb_id": {strconv.Itoa(ids.TMDB)}}
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
