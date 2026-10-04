package provider

import (
	"context"
	"net/url"
	"strconv"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// progressPageLimit is the continue-watching page size. Scrob defaults to 20
// and the list is the viewer's in-progress titles, not their whole history, so
// one generous page is the whole snapshot in practice.
const progressPageLimit = 500

// listProgress reads GET /history/continue-watching, Scrob's list of titles
// with a stored resume position. Scrob returns it in one response ordered by
// most recently updated, so the read is a complete snapshot with no page
// token, like ratings.
func (s *Server) listProgress(ctx context.Context, client *apiClient) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
	query := url.Values{"limit": {strconv.Itoa(progressPageLimit)}}
	var payload scrobContinueWatchingResponse
	if fault := client.get(ctx, "/history/continue-watching", query, &payload); fault != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
	}
	response := &pluginv1.WatchSyncListRemoteStateResponse{CompleteSnapshot: true}
	for _, entry := range payload.ContinueWatching {
		item, ok := progressStateFromEntry(entry)
		if !ok {
			continue
		}
		response.Items = append(response.Items, item)
	}
	return response, nil
}

// progressStateFromEntry maps one continue-watching row. It reports false for a
// title the plugin cannot identify and for a position outside [0, 100).
//
// The contract reserves 100 for a completed play, which belongs in the watched
// family: importing it as progress would leave Silo showing a title as both
// finished and resumable.
func progressStateFromEntry(entry scrobProgressEntry) (*pluginv1.WatchSyncRemoteState, bool) {
	if entry.ProgressPercent <= 0 || entry.ProgressPercent >= 100 {
		return nil, false
	}
	key, media, ok := stateMediaFromScrob(entry.Media)
	if !ok {
		return nil, false
	}
	progress := &pluginv1.WatchSyncRemoteProgressState{ProgressPercent: entry.ProgressPercent}
	// Scrob reuses the event's updated_at as the display timestamp for a
	// progress row, which is when the position was last written.
	if entry.WatchedAt != nil {
		if at := entry.WatchedAt.Time(); !at.IsZero() {
			progress.PausedAt = timestamppb.New(at)
		}
	}
	return &pluginv1.WatchSyncRemoteState{ProviderItemKey: key, Media: media, Progress: progress}, true
}
