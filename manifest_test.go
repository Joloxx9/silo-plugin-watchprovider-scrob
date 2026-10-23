package main

import (
	"testing"

	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
)

// The embedded manifest must load and pass the SDK's own validation, which
// the host runs before it starts the plugin.
func TestManifestIsValid(t *testing.T) {
	loaded, err := manifest.Load(manifestJSON)
	if err != nil {
		t.Fatalf("manifest.Load() error: %v", err)
	}
	if err := manifest.Validate(loaded); err != nil {
		t.Fatalf("manifest.Validate() error: %v", err)
	}
	sync := loaded.GetCapabilities()[0].GetWatchSyncProvider()
	if !sync.GetImportWatchlist() || !sync.GetExportWatchlist() || !sync.GetRemoveWatchlist() || sync.GetProvidesWatchlistOrder() {
		t.Errorf("watchlist capabilities = %+v, want import, export and remove without order", sync)
	}
	settings := sync.GetConnectionSettings()
	if len(settings) != 1 || settings[0].GetKey() != "remove_watched_movies" || settings[0].GetDefaultValue().GetBoolValue() != true {
		t.Errorf("connection settings = %+v, want remove_watched_movies on by default", settings)
	}
}
