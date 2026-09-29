package main

import (
	_ "embed"

	"github.com/prairie-server/prairie-plugin-sdk/pkg/pluginsdk/runtime"

	"github.com/prairie-server/prairie-plugin-watchprovider-floppy/provider"
)

// version is set at build time with -ldflags "-X main.version=...".
var version string

//go:embed manifest.json
var manifestJSON []byte

// serveManifest is a test seam over runtime.ServeManifest.
var serveManifest = runtime.ServeManifest

func main() {
	serveManifest(manifestJSON, version, runtime.CapabilityServers{
		WatchSyncProvider: provider.NewServer(nil),
	})
}
