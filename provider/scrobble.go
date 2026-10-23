package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

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

// scrobbleSource names the plugin in Scrob's Now Playing and history. Scrob
// reads the optional "source" on POST /history and on its Jellyfin webhook; a
// Scrob without the field ignores it and labels the play "Manual" or
// "Jellyfin".
const scrobbleSource = "silo"

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
	Source           string          `json:"source"`
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
		if notification == "PlaybackStop" && finishedPlayback(event) {
			if fault := recordFinishedPlayback(ctx, client, event, results); fault != nil {
				return fault
			}
			continue
		}
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
		Source:           scrobbleSource,
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

// scrobWatchedFraction is how far into a title Scrob's webhook counts a stop
// as a finished play.
const scrobWatchedFraction = 0.90

// finishedPlayback reports a stop that Scrob's webhook would record as a
// finished play: either Silo counted it, or it reached Scrob's own threshold.
func finishedPlayback(event *pluginv1.WatchSyncEvent) bool {
	if event.GetCompleted() {
		return true
	}
	duration := event.GetDurationSeconds()
	return duration > 0 && event.GetPositionSeconds()/duration >= scrobWatchedFraction
}

// recordFinishedPlayback ends a finished playback without the webhook stop.
//
// The webhook would stamp the play with the moment Scrob received the stop.
// Silo treats an imported play as its own only within the same second, so
// that play would come back into Silo as a second viewing on the next import.
// Silo also counts the stop as the play's export and sends no separate one.
// So a play Silo counted is written with POST /history at Silo's time, and the
// Now Playing session is dismissed rather than stopped, which would forward a
// second play to the services connected to Scrob. A stop Scrob would count but
// Silo did not is only dismissed.
func recordFinishedPlayback(ctx context.Context, client *apiClient, event *pluginv1.WatchSyncEvent, results *resultSet) *pluginv1.WatchSyncFault {
	status := pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE
	if event.GetCompleted() {
		body, ok := watchEventBody(event)
		if !ok {
			results.reject(event, unsupportedPlayMessage)
			return dismissSessionQuietly(ctx, client, event)
		}
		code, fault := client.postStatus(ctx, "/history", body, nil)
		switch {
		case fault == nil:
			status = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED
		case code == http.StatusConflict:
			// Scrob already holds a play within its duplicate window.
		case connectionWide(fault):
			return fault
		default:
			results.fail(event, fault)
			return nil
		}
	}
	if fault := dismissSession(ctx, client, event); fault != nil {
		if connectionWide(fault) {
			return fault
		}
		results.fail(event, fault)
		return nil
	}
	results.set(event, status)
	return nil
}

// dismissSessionQuietly ends the session of a play that cannot be recorded,
// reporting only a fault that ends the whole call.
func dismissSessionQuietly(ctx context.Context, client *apiClient, event *pluginv1.WatchSyncEvent) *pluginv1.WatchSyncFault {
	if fault := dismissSession(ctx, client, event); fault != nil && connectionWide(fault) {
		return fault
	}
	return nil
}

type scrobNowPlayingResponse struct {
	NowPlaying []struct {
		SessionKey string `json:"session_key"`
	} `json:"now_playing"`
}

// dismissSession removes this playback's Now Playing session without
// recording or forwarding anything. Scrob keys a Jellyfin webhook session as
// "jellyfin:<user id>:<session id>" and does not tell an API-key caller its
// user id, so the key is read back from the Now Playing list.
func dismissSession(ctx context.Context, client *apiClient, event *pluginv1.WatchSyncEvent) *pluginv1.WatchSyncFault {
	var nowPlaying scrobNowPlayingResponse
	if fault := client.get(ctx, "/history/now-playing", nil, &nowPlaying); fault != nil {
		return fault
	}
	suffix := ":" + scrobbleSessionID(event)
	for _, session := range nowPlaying.NowPlaying {
		if !strings.HasPrefix(session.SessionKey, "jellyfin:") || !strings.HasSuffix(session.SessionKey, suffix) {
			continue
		}
		// The request builder escapes the path, so the key goes in raw.
		code, fault := client.delete(ctx, "/history/session/"+session.SessionKey, nil)
		if fault != nil && code != http.StatusNotFound {
			return fault
		}
	}
	return nil
}
