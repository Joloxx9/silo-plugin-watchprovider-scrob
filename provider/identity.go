package provider

import (
	"fmt"
	"strconv"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// scrobIDs is one title's external identifiers as Scrob's JSON API wants
// them: integer TMDB/TVDB ids, string IMDb id.
type scrobIDs struct {
	TMDB int
	TVDB int
	IMDb string
}

// idsFromExternal reads the host's generic external-id map (keys "tmdb",
// "imdb", "tvdb", case-insensitive) into Scrob's integer/string shape. A
// non-numeric TMDB or TVDB value is dropped rather than rejecting the whole
// event: Scrob's own ids are always numeric, so a non-numeric value could
// only have come from a provider namespace collision.
func idsFromExternal(external map[string]string) scrobIDs {
	var ids scrobIDs
	for key, value := range external {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "tmdb":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				ids.TMDB = n
			}
		case "tvdb":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				ids.TVDB = n
			}
		case "imdb":
			ids.IMDb = value
		}
	}
	return ids
}

func (ids scrobIDs) empty() bool {
	return ids.TMDB <= 0 && ids.TVDB <= 0 && ids.IMDb == ""
}

// movieKey and episodeKey mirror the id-priority convention Silo's other
// watch providers use for a provider item key: prefer IMDb, then TMDB, then
// TVDB for movies; TVDB-first for shows/episodes, since TheTVDB episode ids
// stay stable across TheTVDB's alternate orderings while (season, number) do
// not.
func movieKey(ids scrobIDs) string {
	switch {
	case ids.IMDb != "":
		return "imdb:" + ids.IMDb
	case ids.TMDB > 0:
		return "tmdb:" + strconv.Itoa(ids.TMDB)
	case ids.TVDB > 0:
		return "tvdb:" + strconv.Itoa(ids.TVDB)
	default:
		return ""
	}
}

func showKey(ids scrobIDs) string {
	switch {
	case ids.TVDB > 0:
		return "tvdb:" + strconv.Itoa(ids.TVDB)
	case ids.TMDB > 0:
		return "tmdb:" + strconv.Itoa(ids.TMDB)
	case ids.IMDb != "":
		return "imdb:" + ids.IMDb
	default:
		return ""
	}
}

func episodeKey(showIDs scrobIDs, season, episode int, episodeIDs scrobIDs) string {
	switch {
	case episodeIDs.TVDB > 0:
		return "tvdb:" + strconv.Itoa(episodeIDs.TVDB)
	case episodeIDs.TMDB > 0:
		return "tmdb:" + strconv.Itoa(episodeIDs.TMDB)
	case episodeIDs.IMDb != "":
		return "imdb:" + episodeIDs.IMDb
	case showIDs.TVDB > 0:
		return fmt.Sprintf("show:tvdb:%d:s%d:e%d", showIDs.TVDB, season, episode)
	case showIDs.TMDB > 0:
		return fmt.Sprintf("show:tmdb:%d:s%d:e%d", showIDs.TMDB, season, episode)
	default:
		return ""
	}
}

func intString(value int) string {
	if value <= 0 {
		return ""
	}
	return strconv.Itoa(value)
}

func externalIDsFromScrob(ids scrobIDs) map[string]string {
	out := make(map[string]string, 3)
	if ids.TMDB > 0 {
		out["tmdb"] = strconv.Itoa(ids.TMDB)
	}
	if ids.TVDB > 0 {
		out["tvdb"] = strconv.Itoa(ids.TVDB)
	}
	if ids.IMDb != "" {
		out["imdb"] = ids.IMDb
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mediaType(value pluginv1.WatchSyncMediaType) string {
	switch value {
	case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE:
		return "movie"
	case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE:
		return "episode"
	case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES:
		return "series"
	default:
		return ""
	}
}

// stateMediaFromScrob maps one Scrob media row to the provider item key and
// the media description every remote state shares, so a rating, a play and a
// resume position for the same title land on one key in Silo.
//
// A series row is accepted here; callers that must reject a season-scoped
// entry check that themselves, because the season number sits beside the media
// rather than inside it.
func stateMediaFromScrob(media scrobMedia) (string, *pluginv1.WatchSyncMedia, bool) {
	switch media.Type {
	case "movie":
		ids := scrobIDs{TMDB: media.TMDBID, TVDB: media.TVDBID, IMDb: media.IMDbID}
		key := movieKey(ids)
		if key == "" {
			return "", nil, false
		}
		return key, &pluginv1.WatchSyncMedia{
			MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
			Title:       media.Title,
			ExternalIds: externalIDsFromScrob(ids),
		}, true
	case "series":
		ids := scrobIDs{TMDB: media.TMDBID, TVDB: media.TVDBID, IMDb: media.IMDbID}
		key := showKey(ids)
		if key == "" {
			return "", nil, false
		}
		return key, &pluginv1.WatchSyncMedia{
			MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES,
			Title:       media.Title,
			ExternalIds: externalIDsFromScrob(ids),
		}, true
	case "episode":
		if media.SeasonNumber == nil || media.EpisodeNumber == nil {
			return "", nil, false
		}
		showIDs := scrobIDs{TMDB: media.ShowTMDBID, TVDB: media.ShowTVDBID}
		episodeIDs := scrobIDs{TMDB: media.TMDBID, TVDB: media.TVDBID, IMDb: media.IMDbID}
		key := episodeKey(showIDs, *media.SeasonNumber, *media.EpisodeNumber, episodeIDs)
		if key == "" {
			return "", nil, false
		}
		return key, &pluginv1.WatchSyncMedia{
			MediaType:         pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
			Title:             media.Title,
			ExternalIds:       externalIDsFromScrob(episodeIDs),
			SeriesTitle:       media.ShowTitle,
			SeriesExternalIds: externalIDsFromScrob(showIDs),
			SeasonNumber:      int32(*media.SeasonNumber),
			EpisodeNumber:     int32(*media.EpisodeNumber),
		}, true
	default:
		return "", nil, false
	}
}
