package provider

import (
	"context"
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

// scrobblePlayback reports one playback transition per event. Scrob keys a
// session by the id the webhook carries, so start, pause and stop for one
// playback must repeat the same `playback_session_id`.
func scrobblePlayback(ctx context.Context, client *apiClient, events []*pluginv1.WatchSyncEvent, results *resultSet, notification string) *pluginv1.WatchSyncFault {
	query := url.Values{"api_key": {client.apiKey}}
	for _, event := range events {
		body, ok := jellyfinWebhookBody(event, notification)
		if !ok {
			results.reject(event, errScrobbleNeedsExternalID)
			continue
		}
		if fault := client.postQuery(ctx, jellyfinWebhookPath, query, body, nil); fault != nil {
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

func jellyfinWebhookBody(event *pluginv1.WatchSyncEvent, notification string) (jellyfinWebhook, bool) {
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
			ID:        scrobbleSessionID(event),
			PlayState: jellyfinPlayState{PositionTicks: int64(event.GetPositionSeconds() * ticksPerSecond)},
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
