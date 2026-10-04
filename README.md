# Scrob watch-provider plugin for Silo

Connects Silo profiles to a self-hosted [Scrob](https://github.com/ellite/scrob) instance through Silo's `watch_sync_provider.v1` plugin contract.

## Capabilities

- Validates each profile's Scrob API key without persisting it in the plugin.
- Imports movie and episode watch history.
- Exports completed movie and episode watches, and unwatches.
- Imports and exports movie, series and episode ratings.
- Imports resume positions from Scrob's continue-watching list.
- Reports live playback to Scrob's Now Playing, which forwards it to the
  services connected to Scrob.

The plugin deliberately does not advertise progress, favorites, watchlist, or live scrobble sync. Scrob models a watchlist as arbitrary named lists rather than Silo's single list, which needs its own connection-config design; progress and scrobble are reasonable follow-ups once this lands.

## Live playback

Scrob has no provider-neutral endpoint for reporting playback. Live playback
reaches it only through a media-server webhook, so the plugin posts the
Jellyfin webhook shape to Scrob's connection-less webhook route, authenticated
by the connection's own API key. No extra setup is needed in Scrob.

Scrob forwards Now Playing to the services connected to it, such as Trakt or
Simkl. If you connect one of those to both Silo and Scrob, it receives the play
twice: once scrobbled live through Scrob, and once from Silo's own connection.
Connect each service to one of the two, not both.

## Server URL

Scrob is self-hosted, so each connection supplies its own server URL alongside the API key: different profiles can connect to different Scrob servers. A standard Scrob deployment (its `docker-compose.yaml`) publishes only the frontend's port; the plugin routes every request through `/api/proxy/*`, which the frontend forwards to the backend for a request carrying an API key (see `frontend/src/middleware.ts` and `frontend/src/pages/api/proxy/[...path].ts` in the Scrob repo). A bare `host:port` URL defaults to `http://`, since a self-hosted LAN address is overwhelmingly plain HTTP rather than TLS.

## Ratings

Scrob rates movies and series from 0 to 10 in increments of whatever precision its UI allows; Silo's plugin contract uses whole ratings from 1 to 10. Import rounds a Scrob score half up and clamps it to 1-10, and export writes the rating as the title's score; a removal clears it.

`GET /ratings` is a single unpaginated response covering every rating the account has - movies, shows, seasons, and episodes together - so every import is a complete snapshot. The endpoint reports `tmdb_id` on a rated item but not `imdb_id` or `tvdb_id`, even though Scrob stores both, so a TVDB-only rated show cannot be matched and is reported as absent rather than imported under the wrong identity.

A season rating is stored against the same series row as a whole-series rating in Scrob, distinguished only by a `season_number` field beside (not inside) the rated item. The plugin skips it: importing it under the series' key would silently overwrite a real whole-series rating.

## Watched history

`GET /history` pages individual watch events rather than a per-title summary. The plugin reads every page of one connection's history inside a single `ListRemoteState` call and aggregates it into one state per title (play count and the most recent watch time) before returning anything, since a per-event read spread across pages cannot otherwise be reconciled into the host's per-title state. A very large history can exceed the sync time budget; the sync retries on the next scheduled run when it does.

## Setup

1. Install the plugin.
2. In Silo's watch-provider settings, connect each profile with its Scrob server URL and that Scrob user's API key (from Scrob's account settings).

The server URL and API key are profile-scoped connection data encrypted and owned by the Silo host.

## Development

The plugin builds against the `silo-plugin-sdk` watch-sync contract (v0.20.0).

```bash
make test
make build
./plugin manifest
```

`make build-all` produces static binaries for the platforms declared in `manifest.json`.

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request. Changes to authentication, reconciliation, idempotency, or the watch-sync contract should start as an issue.

## License

AGPL-3.0-only.
