# Scrob watch-provider plugin for Silo

Connects Silo profiles to a self-hosted [Scrob](https://github.com/ellite/scrob) instance through Silo's `watch_sync_provider.v1` plugin contract.

## Capabilities

- Validates each profile's Scrob API key without persisting it in the plugin.
- Imports movie and episode watch history.
- Exports completed movie and episode watches, skipping a play Scrob already recorded. Plays from before the profile connected are not exported: they are history Silo already had, often with different timestamps than Scrob's copy. A connection made with an older plugin version starts this cutoff at its first sync after the update.
- An unwatch removes only the Scrob play at that watch time, never the title's other plays.
- Imports and exports movie and series ratings.
- Imports resume positions from Scrob's continue-watching list.
- Reports live playback, including pauses, to Scrob's Now Playing, which
  forwards it to the services connected to Scrob.

The plugin deliberately does not advertise favorites or watchlist sync. Scrob models a watchlist as arbitrary named lists rather than Silo's single list, which needs its own connection-config design.

## Live playback

Scrob has no provider-neutral endpoint for reporting playback. Live playback
reaches it through Scrob's generic player webhook (`/webhooks/kodi`), the one its
Kodi add-on uses, authenticated by the connection's own API key in a header. No
extra setup is needed in Scrob. Unlike Scrob's Jellyfin webhook, it has no
media-server connection behind it. It adds nothing to Scrob's collection, and the
settings of a Jellyfin connection in Scrob don't apply to it. An episode is found
by its series' TVDB or IMDb id and its numbers, so TVDB-only shows work too.

The webhook and the plugin's own history writes name their source `silo`, so
Scrob v2.33 and later show these plays as Silo, with Silo's icon, in Now Playing
and history. Older versions ignore the field and show the plays as Kodi or
Manual.

Scrob forwards Now Playing to the services connected to it, such as Trakt or
Simkl. If you connect one of those to both Silo and Scrob, it receives the play
twice: once scrobbled live through Scrob, and once from Silo's own connection.
Connect each service to one of the two, not both.

Scrob creates a title it has not seen before from the name the webhook
carries. Silo servers that predate titles on playback events send none, and
Scrob then answers every start, pause and stop for that title with a server
error. The plugin reports such an event as rejected, with a message saying so,
instead of retrying it. The next watched sync still records the play; updating
Silo fixes live playback for those titles.

## Server URL

Scrob is self-hosted, so each connection supplies its own server URL alongside the API key: different profiles can connect to different Scrob servers. A standard Scrob deployment (its `docker-compose.yaml`) publishes only the frontend's port; the plugin routes every request through `/api/proxy/*`, which the frontend forwards to the backend for a request carrying an API key (see `frontend/src/middleware.ts` and `frontend/src/pages/api/proxy/[...path].ts` in the Scrob repo). A bare `host:port` URL defaults to `http://`, since a self-hosted LAN address is overwhelmingly plain HTTP rather than TLS.

## Ratings

Silo syncs movie and series ratings. Scrob also rates episodes, and the plugin can read and write them, but Silo neither sends nor accepts episode ratings yet, so none are synced.

Scrob rates movies, series and episodes from 0 to 10 in increments of whatever precision its UI allows; Silo's plugin contract uses whole ratings from 1 to 10. Import rounds a Scrob score half up and clamps it to 1-10, and export writes the rating as the title's score; a removal clears it.

`GET /ratings` is a single unpaginated response covering every rating the account has - movies, shows, seasons, and episodes together - so every import is a complete snapshot. The endpoint reports `tmdb_id` on a rated item but not `imdb_id` or `tvdb_id`, even though Scrob stores both, so a TVDB-only rated show cannot be matched and is reported as absent rather than imported under the wrong identity.

A season rating is stored against the same series row as a whole-series rating in Scrob, distinguished only by a `season_number` field beside (not inside) the rated item. The plugin skips it: importing it under the series' key would silently overwrite a real whole-series rating.

## Watched history

`GET /history` pages individual watch events rather than a per-title summary. The plugin reads every page of one connection's history inside a single `ListRemoteState` call and aggregates it into one state per title (play count and the most recent watch time) before returning anything, since a per-event read spread across pages cannot otherwise be reconciled into the host's per-title state. A very large history can exceed the sync time budget; the sync retries on the next scheduled run when it does.

## Network access and external services

The plugin makes outbound requests only to the Scrob server URL each connection supplies, authenticated with that connection's own API key. It contacts no other host and sends no telemetry. Scrob itself may forward Now Playing to services connected to it (see Live playback above); that forwarding happens inside Scrob, not in the plugin.

The API key always travels in the `X-Api-Key` request header, never in a URL.

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
