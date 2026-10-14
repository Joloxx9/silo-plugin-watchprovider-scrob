package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// Scrob has no provider-neutral "report playback" endpoint. Live playback
// reaches it only through a media-server webhook, so the plugin speaks the
// Jellyfin webhook shape, which Scrob accepts on the connection-less route
// authenticated by the user's own API key.
//
// This is deliberate and documented in the README: Scrob forwards Now Playing
// to the services connected to it, so a viewer who also connects Trakt to both
// Silo and Scrob can receive the same play twice.
const jellyfinWebhookPath = "/webhooks/jellyfin"

// ticksPerSecond is the Jellyfin tick, 100 nanoseconds.
const ticksPerSecond = 10_000_000

type jellyfinProviderIDs struct {
	Tmdb string `json:"Tmdb,omitempty"`
	Tvdb string `json:"Tvdb,omitempty"`
}

type jellyfinItem struct {
	Type              string              `json:"Type"`
	Name              string              `json:"Name,omitempty"`
	ProviderIds       jellyfinProviderIDs `json:"ProviderIds"`
	SeriesProviderIds jellyfinProviderIDs `json:"SeriesProviderIds,omitempty"`
	SeriesName        string              `json:"SeriesName,omitempty"`
	ParentIndexNumber *int                `json:"ParentIndexNumber,omitempty"`
	IndexNumber       *int                `json:"IndexNumber,omitempty"`
	RunTimeTicks      int64               `json:"RunTimeTicks,omitempty"`
}

type jellyfinPlayState struct {
	PositionTicks int64 `json:"PositionTicks"`
	// IsPaused tells Scrob a progress notification is a pause. Without it
	// Scrob shows the session as playing and forwards no pause.
	IsPaused bool `json:"IsPaused"`
}

type jellyfinSession struct {
	ID        string            `json:"Id,omitempty"`
	PlayState jellyfinPlayState `json:"PlayState"`
}

type jellyfinWebhook struct {
	NotificationType string          `json:"NotificationType"`
	Item             jellyfinItem    `json:"Item"`
	Session          jellyfinSession `json:"Session"`
}

const errScrobbleNeedsExternalID = "Scrob playback reporting requires a movie or episode with a TMDB id"

// errScrobbleServerError explains a webhook Scrob answered with a server
// error. The usual cause is a title Scrob has not seen yet arriving without a
// name, from a Silo that predates titles on playback events: Scrob cannot
// create the title and answers 500 for every start, progress and stop.
const errScrobbleServerError = "Scrob could not record live playback for this title (HTTP %d). If Scrob has not seen the title before, update Silo so playback events carry titles; the next watched sync still records the play."

// scrobblePlayback reports one playback transition per event. Scrob keys a
// session by the id the webhook carries, so start, pause and stop for one
// playback must repeat the same `playback_session_id`. A pause is a progress
// notification with paused set.
//
// A server error from the webhook is a rejection, not a temporary fault: a
// playback transition is only worth reporting while it is current, and the
// usual cause (see errScrobbleServerError) fails the same way on every retry.
func scrobblePlayback(ctx context.Context, client *apiClient, events []*pluginv1.WatchSyncEvent, results *resultSet, notification string, paused bool) *pluginv1.WatchSyncFault {
	// Scrob's webhook route accepts the key only as a query parameter, so
	// unlike every other request this one carries it in the URL.
	query := url.Values{"api_key": {client.apiKey}}
	for _, event := range events {
		body, ok := jellyfinWebhookBody(event, notification, paused)
		if !ok {
			results.reject(event, errScrobbleNeedsExternalID)
			continue
		}
		if status, fault := client.postQuery(ctx, jellyfinWebhookPath, query, body, nil); fault != nil {
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
		results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED)
	}
	return nil
}

func jellyfinWebhookBody(event *pluginv1.WatchSyncEvent, notification string, paused bool) (jellyfinWebhook, bool) {
	media := event.GetMedia()
	kind := mediaType(media.GetMediaType())
	if kind != "movie" && kind != "episode" {
		return jellyfinWebhook{}, false
	}
	ids := idsFromExternal(media.GetExternalIds())
	item := jellyfinItem{
		Name:         media.GetTitle(),
		RunTimeTicks: int64(event.GetDurationSeconds() * ticksPerSecond),
		ProviderIds:  jellyfinProviderIDs{Tmdb: intString(ids.TMDB), Tvdb: intString(ids.TVDB)},
	}
	switch kind {
	case "movie":
		if ids.TMDB <= 0 {
			return jellyfinWebhook{}, false
		}
		item.Type = "Movie"
	case "episode":
		seriesIDs := idsFromExternal(media.GetSeriesExternalIds())
		if seriesIDs.TMDB <= 0 {
			return jellyfinWebhook{}, false
		}
		season, episode := int(media.GetSeasonNumber()), int(media.GetEpisodeNumber())
		item.Type = "Episode"
		item.SeriesName = media.GetSeriesTitle()
		item.SeriesProviderIds = jellyfinProviderIDs{Tmdb: intString(seriesIDs.TMDB), Tvdb: intString(seriesIDs.TVDB)}
		item.ParentIndexNumber = &season
		item.IndexNumber = &episode
	}
	return jellyfinWebhook{
		NotificationType: notification,
		Item:             item,
		Session: jellyfinSession{
			ID: scrobbleSessionID(event),
			PlayState: jellyfinPlayState{
				PositionTicks: int64(event.GetPositionSeconds() * ticksPerSecond),
				IsPaused:      paused,
			},
		},
	}, true
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
