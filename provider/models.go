package provider

import (
	"encoding/json"
	"fmt"
	"time"
)

// scrobTime decodes a Scrob timestamp. Scrob stores timestamps naive (no
// timezone) and serializes them with Python's datetime.isoformat(), which
// omits the offset entirely (e.g. "2026-10-01T12:06:08") rather than the
// "Z"-suffixed RFC 3339 encoding/json's default time.Time unmarshaler
// requires - every non-null timestamp fails to decode without this. Scrob
// writes these with datetime.utcnow(), so a naive value is treated as UTC.
type scrobTime time.Time

const scrobNaiveTimeLayout = "2006-01-02T15:04:05"

func (t *scrobTime) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw == "" {
		*t = scrobTime(time.Time{})
		return nil
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		*t = scrobTime(parsed)
		return nil
	}
	parsed, err := time.ParseInLocation(scrobNaiveTimeLayout, raw, time.UTC)
	if err != nil {
		return fmt.Errorf("parse scrob timestamp %q: %w", raw, err)
	}
	*t = scrobTime(parsed)
	return nil
}

func (t scrobTime) Time() time.Time { return time.Time(t) }

// MarshalJSON exists so scrobTime round-trips in tests against a fake Scrob
// server; the plugin never sends this type back to Scrob itself (outbound
// timestamps go through RFC 3339 strings built directly, see watchEventBody).
func (t scrobTime) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(t).UTC().Format(scrobNaiveTimeLayout))
}

type scrobMedia struct {
	ID            int    `json:"id"`
	TMDBID        int    `json:"tmdb_id"`
	TVDBID        int    `json:"tvdb_id"`
	IMDbID        string `json:"imdb_id"`
	Type          string `json:"type"`
	Title         string `json:"title"`
	SeasonNumber  *int   `json:"season_number"`
	EpisodeNumber *int   `json:"episode_number"`
	ShowTitle     string `json:"show_title"`
	ShowTMDBID    int    `json:"show_tmdb_id"`
	ShowTVDBID    int    `json:"show_tvdb_id"`
}

type scrobHistoryEvent struct {
	Media     scrobMedia `json:"media"`
	WatchedAt *scrobTime `json:"watched_at"`
}

type scrobHistoryResponse struct {
	Page       int                 `json:"page"`
	TotalPages int                 `json:"total_pages"`
	Results    []scrobHistoryEvent `json:"results"`
}

type scrobWatchEventCreate struct {
	TMDBID        int    `json:"tmdb_id,omitempty"`
	TVDBID        int    `json:"tvdb_id,omitempty"`
	MediaType     string `json:"media_type"`
	WatchedAt     string `json:"watched_at,omitempty"`
	Completed     bool   `json:"completed"`
	SeriesTMDBID  int    `json:"series_tmdb_id,omitempty"`
	SeriesTVDBID  int    `json:"series_tvdb_id,omitempty"`
	SeasonNumber  *int   `json:"season_number,omitempty"`
	EpisodeNumber *int   `json:"episode_number,omitempty"`
}

type scrobRatingEntry struct {
	Media        scrobMedia `json:"media"`
	Rating       float64    `json:"rating"`
	RatedAt      scrobTime  `json:"rated_at"`
	SeasonNumber *int       `json:"season_number"`
}

type scrobRatingsResponse struct {
	Results []scrobRatingEntry `json:"results"`
}

type scrobRatingIn struct {
	TMDBID    int     `json:"tmdb_id,omitempty"`
	TVDBID    int     `json:"tvdb_id,omitempty"`
	MediaType string  `json:"media_type"`
	Rating    float64 `json:"rating"`
}
