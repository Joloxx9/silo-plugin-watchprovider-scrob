package provider

import "testing"

func TestIdsFromExternal(t *testing.T) {
	ids := idsFromExternal(map[string]string{"TMDB": "100", "tvdb": "200", "imdb": "tt1"})
	if ids.TMDB != 100 || ids.TVDB != 200 || ids.IMDb != "tt1" {
		t.Fatalf("idsFromExternal = %+v", ids)
	}
	if got := idsFromExternal(map[string]string{"tmdb": "not-a-number"}); got.TMDB != 0 {
		t.Fatalf("idsFromExternal with non-numeric tmdb = %+v, want zero", got)
	}
	if !(scrobIDs{}).empty() {
		t.Fatalf("empty() = false for zero value")
	}
}

func TestMovieKeyPrefersIMDbThenTMDBThenTVDB(t *testing.T) {
	if got := movieKey(scrobIDs{TMDB: 100, TVDB: 200, IMDb: "tt1"}); got != "imdb:tt1" {
		t.Fatalf("movieKey = %q, want imdb:tt1", got)
	}
	if got := movieKey(scrobIDs{TMDB: 100, TVDB: 200}); got != "tmdb:100" {
		t.Fatalf("movieKey = %q, want tmdb:100", got)
	}
	if got := movieKey(scrobIDs{TVDB: 200}); got != "tvdb:200" {
		t.Fatalf("movieKey = %q, want tvdb:200", got)
	}
	if got := movieKey(scrobIDs{}); got != "" {
		t.Fatalf("movieKey = %q, want empty", got)
	}
}

func TestShowKeyPrefersTVDBThenTMDBThenIMDb(t *testing.T) {
	if got := showKey(scrobIDs{TMDB: 100, TVDB: 200}); got != "tvdb:200" {
		t.Fatalf("showKey = %q, want tvdb:200", got)
	}
	if got := showKey(scrobIDs{TMDB: 100}); got != "tmdb:100" {
		t.Fatalf("showKey = %q, want tmdb:100", got)
	}
}

func TestEpisodeKeyFallsBackToShowContext(t *testing.T) {
	if got := episodeKey(scrobIDs{}, 1, 2, scrobIDs{TVDB: 555}); got != "tvdb:555" {
		t.Fatalf("episodeKey = %q, want tvdb:555", got)
	}
	if got := episodeKey(scrobIDs{TVDB: 999}, 1, 2, scrobIDs{}); got != "show:tvdb:999:s1:e2" {
		t.Fatalf("episodeKey = %q, want show:tvdb:999:s1:e2", got)
	}
	if got := episodeKey(scrobIDs{TMDB: 888}, 1, 2, scrobIDs{}); got != "show:tmdb:888:s1:e2" {
		t.Fatalf("episodeKey = %q, want show:tmdb:888:s1:e2", got)
	}
	if got := episodeKey(scrobIDs{}, 1, 2, scrobIDs{}); got != "" {
		t.Fatalf("episodeKey = %q, want empty", got)
	}
}

func TestExternalIDsFromScrob(t *testing.T) {
	got := externalIDsFromScrob(scrobIDs{TMDB: 1, TVDB: 2, IMDb: "tt1"})
	want := map[string]string{"tmdb": "1", "tvdb": "2", "imdb": "tt1"}
	if len(got) != len(want) {
		t.Fatalf("externalIDsFromScrob = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("externalIDsFromScrob[%q] = %q, want %q", k, got[k], v)
		}
	}
	if externalIDsFromScrob(scrobIDs{}) != nil {
		t.Fatalf("externalIDsFromScrob(empty) should be nil")
	}
}
