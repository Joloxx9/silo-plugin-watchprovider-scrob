package provider

import (
	"context"
	"fmt"
	"math"
	"net/http"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// Scrob has no provider-neutral "report playback" endpoint. Live playback
// reaches it through its generic player webhook, the one its Kodi add-on uses.
// That webhook has no media-server connection behind it, so it adds nothing
// to Scrob's collection and no media-server settings apply to it. It takes the
// key in the X-Api-Key header, resolves the title from external IDs, drives
// Now Playing, and forwards live scrobbles to the services connected to Scrob.
//
// This is deliberate and documented in the README: a viewer who also connects
// Trakt to both Silo and Scrob can receive the same play twice.
const playerWebhookPath = "/webhooks/kodi"

// scrobbleSource names the plugin in Scrob's Now Playing and history. Scrob
// reads the optional "source" on POST /history and on its player webhook; a
// Scrob without the field ignores it and labels the play "Manual" or "Kodi".
const scrobbleSource = "silo"

type playerItem struct {
	Type      string            `json:"type"`
	UniqueID  map[string]string `json:"uniqueid,omitempty"`
	Title     string            `json:"title,omitempty"`
	Year      int32             `json:"year,omitempty"`
	ShowTitle string            `json:"showtitle,omitempty"`
	Season    *int              `json:"season,omitempty"`
	Episode   *int              `json:"episode,omitempty"`
}

type playerWebhook struct {
	Source          string     `json:"source"`
	Event           string     `json:"event"`
	SessionID       string     `json:"session_id"`
	PositionSeconds int64      `json:"position_seconds"`
	TotalSeconds    int64      `json:"total_seconds"`
	Item            playerItem `json:"item"`
}

type playerResponse struct {
	Status string `json:"status"`
}

const (
	errScrobbleNeedsExternalID = "Scrob playback reporting requires a movie with a TMDB or IMDb id, or an episode with series ids and season and episode numbers"
	errScrobbleUnidentified    = "Scrob could not identify this title for live playback"
)

// errScrobbleServerError explains a webhook Scrob answered with a server
// error. The usual cause is a title Scrob has not seen yet arriving without a
// name, from a Silo that predates titles on playback events: Scrob cannot
// create the title and answers 500 for every start, pause and stop.
const errScrobbleServerError = "Scrob could not record live playback for this title (HTTP %d). If Scrob has not seen the title before, update Silo so playback events carry titles; the next watched sync still records the play."

// scrobblePlayback reports one playback transition per event. Scrob keys a
// session by the id the webhook carries, so start, pause and stop for one
// playback must repeat the same `playback_session_id`.
//
// A server error from the webhook is a rejection, not a temporary fault: a
// playback transition is only worth reporting while it is current, and the
// usual cause (see errScrobbleServerError) fails the same way on every retry.
func scrobblePlayback(ctx context.Context, client *apiClient, events []*pluginv1.WatchSyncEvent, results *resultSet, operation pluginv1.WatchSyncOperation) *pluginv1.WatchSyncFault {
	for _, event := range events {
		body, ok := playerWebhookBody(event, operation)
		if !ok {
			results.reject(event, errScrobbleNeedsExternalID)
			continue
		}
		var response playerResponse
		if status, fault := client.postStatus(ctx, playerWebhookPath, body, &response); fault != nil {
			if connectionWide(fault) {
				return fault
			}
			if status >= http.StatusInternalServerError {
				results.reject(event, fmt.Sprintf(errScrobbleServerError, status))
				continue
			}
			results.fail(event, fault)
			continue
		}
		if response.Status != "ok" {
			results.reject(event, errScrobbleUnidentified)
			continue
		}
		results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED)
	}
	return nil
}

// playerEvent names the transition. Silo also sends a start when a paused
// playback resumes; Scrob's resume keeps the session's position.
func playerEvent(event *pluginv1.WatchSyncEvent, operation pluginv1.WatchSyncOperation) string {
	switch operation {
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_PAUSE:
		return "playback_paused"
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP:
		return "playback_stopped"
	default:
		if event.GetPositionSeconds() > 0 {
			return "playback_resumed"
		}
		return "playback_started"
	}
}

// playerWebhookBody describes the playback the way the webhook reads it. For
// an episode the webhook reads the TVDB and IMDb ids as the series' and finds
// the episode by its numbers; a TMDB id is only a hint it checks against the
// series, so the episode's own TMDB id is sent, or the series' without one.
func playerWebhookBody(event *pluginv1.WatchSyncEvent, operation pluginv1.WatchSyncOperation) (playerWebhook, bool) {
	media := event.GetMedia()
	ids := idsFromExternal(media.GetExternalIds())
	var item playerItem
	switch mediaType(media.GetMediaType()) {
	case "movie":
		if ids.TMDB <= 0 && ids.IMDb == "" {
			return playerWebhook{}, false
		}
		item = playerItem{Type: "movie", UniqueID: uniqueIDs(ids), Title: media.GetTitle(), Year: media.GetYear()}
	case "episode":
		series := idsFromExternal(media.GetSeriesExternalIds())
		unique := scrobIDs{TMDB: ids.TMDB, TVDB: series.TVDB, IMDb: series.IMDb}
		if unique.TMDB <= 0 {
			unique.TMDB = series.TMDB
		}
		if unique.empty() || media.GetSeasonNumber() <= 0 || media.GetEpisodeNumber() <= 0 {
			return playerWebhook{}, false
		}
		season, episode := int(media.GetSeasonNumber()), int(media.GetEpisodeNumber())
		item = playerItem{
			Type: "episode", UniqueID: uniqueIDs(unique), Title: media.GetTitle(),
			ShowTitle: media.GetSeriesTitle(), Season: &season, Episode: &episode,
		}
	default:
		return playerWebhook{}, false
	}
	return playerWebhook{
		Source:          scrobbleSource,
		Event:           playerEvent(event, operation),
		SessionID:       scrobbleSessionID(event),
		PositionSeconds: int64(math.Round(max(event.GetPositionSeconds(), 0))),
		TotalSeconds:    int64(math.Round(max(event.GetDurationSeconds(), 0))),
		Item:            item,
	}, true
}

func uniqueIDs(ids scrobIDs) map[string]string {
	out := map[string]string{}
	if ids.TMDB > 0 {
		out["tmdb"] = intString(ids.TMDB)
	}
	if ids.TVDB > 0 {
		out["tvdb"] = intString(ids.TVDB)
	}
	if ids.IMDb != "" {
		out["imdb"] = ids.IMDb
	}
	return out
}

// scrobbleSessionID keys the Scrob session. The host's playback session id is
// authoritative; an event without one falls back to its own id so a stray
// event still reports rather than joining somebody else's session.
func scrobbleSessionID(event *pluginv1.WatchSyncEvent) string {
	if id := event.GetPlaybackSessionId(); id != "" {
		return id
	}
	return "silo-" + event.GetEventId()
}
