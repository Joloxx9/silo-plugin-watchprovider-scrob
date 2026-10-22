package main

import (
	_ "embed"

	"github.com/Joloxx9/silo-plugin-watchprovider-scrob/provider"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

// version is set at build time with -ldflags "-X main.version=...".
var version string

//go:embed manifest.json
var manifestJSON []byte

func main() {
	runtime.ServeManifest(manifestJSON, version, runtime.CapabilityServers{
		WatchSyncProvider: provider.NewServer(nil),
	})
}
