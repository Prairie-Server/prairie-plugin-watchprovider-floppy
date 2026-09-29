package main

import (
	"bytes"
	"testing"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
	"github.com/prairie-server/prairie-plugin-sdk/pkg/pluginsdk/manifest"
	"github.com/prairie-server/prairie-plugin-sdk/pkg/pluginsdk/runtime"

	"github.com/prairie-server/prairie-plugin-watchprovider-floppy/provider"
)

func TestManifestDeclaresPerConnectionFloppyServer(t *testing.T) {
	t.Parallel()
	parsed, err := manifest.Load(manifestJSON)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	capabilities := parsed.GetCapabilities()
	if len(capabilities) != 1 {
		t.Fatalf("capabilities = %d, want 1", len(capabilities))
	}
	schemas := capabilities[0].GetConfigSchema()
	if len(schemas) != 1 || schemas[0].GetKey() != "floppy" || !schemas[0].GetRequired() {
		t.Fatalf("connection config schemas = %#v", schemas)
	}
	if len(parsed.GetGlobalConfigSchema()) != 0 {
		t.Fatalf("global config schemas = %#v, want none", parsed.GetGlobalConfigSchema())
	}
}

func TestManifestAdvertisesMovieAndSeriesRatings(t *testing.T) {
	t.Parallel()
	parsed, err := manifest.Load(manifestJSON)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	descriptor := parsed.GetCapabilities()[0].GetWatchSyncProvider()
	if !descriptor.GetImportRatings() || !descriptor.GetExportRatings() {
		t.Fatalf("ratings = import %t export %t, want both", descriptor.GetImportRatings(), descriptor.GetExportRatings())
	}
	if descriptor.GetImportFavorites() || descriptor.GetExportFavorites() || descriptor.GetImportWatchlist() || descriptor.GetExportWatchlist() {
		t.Fatalf("descriptor advertises favorites or watchlist: %v", descriptor)
	}
	media := map[pluginv1.WatchSyncMediaType]bool{}
	for _, mediaType := range descriptor.GetSupportedMediaTypes() {
		media[mediaType] = true
	}
	if !media[pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE] || !media[pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES] ||
		!media[pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE] {
		t.Fatalf("supported media types = %v", descriptor.GetSupportedMediaTypes())
	}
}

func TestMainServesWatchSyncProviderWithEmbeddedManifest(t *testing.T) {
	original, originalVersion := serveManifest, version
	t.Cleanup(func() { serveManifest, version = original, originalVersion })
	version = "1.2.3-test"
	calls := 0
	var gotManifest []byte
	var gotVersion string
	var gotServers runtime.CapabilityServers
	serveManifest = func(manifestBytes []byte, v string, servers runtime.CapabilityServers) {
		calls++
		gotManifest, gotVersion, gotServers = manifestBytes, v, servers
	}

	main()

	if calls != 1 {
		t.Fatalf("serveManifest calls = %d, want 1", calls)
	}
	if !bytes.Equal(gotManifest, manifestJSON) || len(gotManifest) == 0 {
		t.Fatalf("manifest bytes were not the embedded manifest")
	}
	if gotVersion != "1.2.3-test" {
		t.Fatalf("version = %q, want 1.2.3-test", gotVersion)
	}
	if _, ok := gotServers.WatchSyncProvider.(*provider.Server); !ok {
		t.Fatalf("WatchSyncProvider = %T, want *provider.Server", gotServers.WatchSyncProvider)
	}
}
