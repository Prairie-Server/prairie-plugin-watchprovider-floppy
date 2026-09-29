package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

var (
	watchedKinds  = []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED}
	progressKinds = []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_PROGRESS}
)

func movieHistoryEntry(mediaID, status string, playedAt string) map[string]any {
	return map[string]any{
		"media_type": "movie", "title": "Movie " + mediaID, "status": status, "played_at_local": playedAt, "instance_id": nil,
		"item": map[string]any{"media_type": "movie", "media_id": mediaID, "source": "tmdb", "title": "Movie " + mediaID},
	}
}

func listState(t *testing.T, upstreamURL string, client *http.Client, req *pluginv1.WatchSyncListRemoteStateRequest) *pluginv1.WatchSyncListRemoteStateResponse {
	t.Helper()
	req.Context = authenticatedContext(upstreamURL)
	response, err := NewServer(client).ListRemoteState(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestListWatchedAppliesCursorBoundsLocally(t *testing.T) {
	t.Parallel()
	cursor := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	var query url.Values
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		writeJSON(t, w, map[string]any{
			"pagination": map[string]any{"total": 5, "limit": 50, "offset": 0},
			"results": []any{map[string]any{"date": "2026-08-05", "entries": []any{
				movieHistoryEntry("1", "Completed", cursor.Add(-time.Minute).Format(time.RFC3339Nano)),
				movieHistoryEntry("2", "In progress", cursor.Add(time.Hour).Format(time.RFC3339Nano)),
				movieHistoryEntry("3", "Completed", "not a time"),
				movieHistoryEntry("4", "Completed", cursor.Add(time.Minute).Format(time.RFC3339Nano)),
				map[string]any{"media_type": "tv", "status": "Completed", "played_at_local": cursor.Add(2 * time.Minute).Format(time.RFC3339Nano),
					"item": map[string]any{"media_type": "tv", "media_id": "9", "source": "tmdb"}},
			}}},
		})
	}))
	defer upstream.Close()

	response := listState(t, upstream.URL, upstream.Client(), &pluginv1.WatchSyncListRemoteStateRequest{
		StateKinds: watchedKinds, Cursor: cursor.Format(time.RFC3339Nano),
	})
	if response.GetFault() != nil || response.GetCompleteSnapshot() || response.GetNextPageToken() != "" {
		t.Fatalf("response = %#v", response)
	}
	if query.Get("start_date") != "2026-08-04" || query.Get("logging_style") != "sessions" || query.Get("limit") != "50" {
		t.Fatalf("query = %v", query)
	}
	if len(response.GetItems()) != 1 || response.GetItems()[0].GetMedia().GetExternalIds()["tmdb"] != "4" ||
		response.GetItems()[0].GetMedia().GetTitle() != "Movie 4" {
		t.Fatalf("items = %#v", response.GetItems())
	}
	// The newest history row sets the high-water mark, even one that is not
	// itself returned (here the in-progress row).
	if want := cursor.Add(time.Hour - providerCursorOverlap).Format(time.RFC3339Nano); response.GetNextCursor() != want {
		t.Fatalf("next cursor = %q, want %q", response.GetNextCursor(), want)
	}
}

func TestListWatchedReportsFaults(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		status int
		body   map[string]any
		req    *pluginv1.WatchSyncListRemoteStateRequest
		want   string
		code   pluginv1.WatchSyncFaultCode
	}{
		{name: "cursor", req: &pluginv1.WatchSyncListRemoteStateRequest{Cursor: "bad"}, want: "Floppy cursor is invalid", code: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST},
		{name: "page token", req: &pluginv1.WatchSyncListRemoteStateRequest{PageToken: "!!"}, want: "Floppy page token is invalid", code: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST},
		{name: "upstream", status: http.StatusInternalServerError, req: &pluginv1.WatchSyncListRemoteStateRequest{}, want: "Floppy is temporarily unavailable", code: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY},
		{name: "pagination", body: map[string]any{"pagination": map[string]any{"total": 1, "limit": 50, "offset": 7}, "results": []any{}},
			req: &pluginv1.WatchSyncListRemoteStateRequest{}, want: "Floppy returned invalid pagination", code: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY},
		{name: "no timestamp", body: map[string]any{"pagination": map[string]any{"total": 100, "limit": 50, "offset": 0}, "results": []any{}},
			req: &pluginv1.WatchSyncListRemoteStateRequest{}, want: "Floppy returned history pages without a usable timestamp", code: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.status != 0 {
					w.WriteHeader(test.status)
					return
				}
				if test.body == nil {
					t.Errorf("upstream called for a locally invalid request")
				}
				writeJSON(t, w, test.body)
			}))
			defer upstream.Close()
			test.req.StateKinds = watchedKinds
			response := listState(t, upstream.URL, upstream.Client(), test.req)
			if response.GetFault().GetCode() != test.code || response.GetFault().GetSafeMessage() != test.want || len(response.GetItems()) != 0 {
				t.Fatalf("response = %#v, want %q", response, test.want)
			}
		})
	}
}

func TestProviderNextCursor(t *testing.T) {
	t.Parallel()
	highWater := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	if got := providerNextCursor(time.Time{}, time.Time{}); got != "" {
		t.Fatalf("zero high water = %q", got)
	}
	if got := providerNextCursor(time.Time{}, highWater); got != "2026-08-05T11:59:58Z" {
		t.Fatalf("first cursor = %q", got)
	}
	previous := highWater.Add(-time.Second)
	if got := providerNextCursor(previous, highWater); got != "2026-08-05T11:59:59Z" {
		t.Fatalf("cursor moved backwards past the previous cursor: %q", got)
	}
	if got := providerNextCursor(highWater.Add(-time.Hour), highWater); got != "2026-08-05T11:59:58Z" {
		t.Fatalf("incremental cursor = %q", got)
	}
}

func TestListProgressFetchesEverySnapshotPage(t *testing.T) {
	t.Parallel()
	cursor := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var queries []url.Values
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		if r.URL.Query().Get("offset") == "0" {
			writeJSON(t, w, map[string]any{
				"pagination": map[string]any{"total": 3, "limit": 200, "offset": 0, "next": "https://floppy.example.com/api/v1/playback/progress/?offset=200"},
				"results": []any{
					progressPayload("1", cursor.Add(-time.Minute)),
					progressPayload("2", cursor.Add(time.Minute)),
				},
			})
			return
		}
		bad := progressPayload("4", cursor.Add(3*time.Minute))
		bad["updated_at"] = "not a time"
		done := progressPayload("5", cursor.Add(4*time.Minute))
		done["completed"] = true
		writeJSON(t, w, map[string]any{
			"pagination": map[string]any{"total": 3, "limit": 200, "offset": 200},
			"results":    []any{progressPayload("3", cursor.Add(2*time.Minute)), bad, done},
		})
	}))
	defer upstream.Close()

	response := listState(t, upstream.URL, upstream.Client(), &pluginv1.WatchSyncListRemoteStateRequest{
		StateKinds: progressKinds, Cursor: cursor.Format(time.RFC3339Nano),
	})
	if response.GetFault() != nil || response.GetCompleteSnapshot() || response.GetNextPageToken() != "" {
		t.Fatalf("response = %#v", response)
	}
	var got []string
	for _, item := range response.GetItems() {
		got = append(got, item.GetMedia().GetExternalIds()["tmdb"])
	}
	if strings.Join(got, ",") != "2,3" {
		t.Fatalf("items = %v, want 2,3", got)
	}
	if want := cursor.Add(2*time.Minute - providerCursorOverlap).Format(time.RFC3339Nano); response.GetNextCursor() != want {
		t.Fatalf("next cursor = %q, want %q", response.GetNextCursor(), want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(queries) != 2 || queries[0].Get("offset") != "0" || queries[1].Get("offset") != "200" ||
		queries[0].Get("updated_since") != cursor.Format(time.RFC3339Nano) || queries[1].Get("media_type") != "movie,episode" {
		t.Fatalf("queries = %v", queries)
	}
}

func TestListProgressReportsFaults(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		status int
		body   map[string]any
		req    *pluginv1.WatchSyncListRemoteStateRequest
		want   string
	}{
		{name: "cursor", req: &pluginv1.WatchSyncListRemoteStateRequest{Cursor: "bad"}, want: "Floppy cursor is invalid"},
		{name: "page token", req: &pluginv1.WatchSyncListRemoteStateRequest{PageToken: "!!"}, want: "Floppy page token is invalid"},
		{name: "upstream", status: http.StatusUnauthorized, req: &pluginv1.WatchSyncListRemoteStateRequest{}, want: "Floppy rejected the API token"},
		{name: "pagination", body: map[string]any{"pagination": map[string]any{"total": 400, "limit": 200, "offset": 0, "next": "https://x/?offset=0"}, "results": []any{}},
			req: &pluginv1.WatchSyncListRemoteStateRequest{}, want: "Floppy returned invalid pagination"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.status != 0 {
					w.WriteHeader(test.status)
					return
				}
				if test.body == nil {
					t.Errorf("upstream called for a locally invalid request")
				}
				writeJSON(t, w, test.body)
			}))
			defer upstream.Close()
			test.req.StateKinds = progressKinds
			response := listState(t, upstream.URL, upstream.Client(), test.req)
			if response.GetFault().GetSafeMessage() != test.want || len(response.GetItems()) != 0 {
				t.Fatalf("response = %#v, want %q", response, test.want)
			}
		})
	}
}

func TestProgressStateSkipsUnusableEntries(t *testing.T) {
	t.Parallel()
	updatedAt := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	base := progressEntry{
		MediaType: "movie", Source: "tmdb", MediaID: json.RawMessage(`"603"`), Position: 600, Duration: 2400,
		UpdatedAt: updatedAt.Format(time.RFC3339Nano),
	}
	for name, mutate := range map[string]func(*progressEntry){
		"completed":         func(entry *progressEntry) { entry.Completed = true },
		"no duration":       func(entry *progressEntry) { entry.Duration = 0 },
		"negative position": func(entry *progressEntry) { entry.Position = -1 },
		"unknown type":      func(entry *progressEntry) { entry.MediaType = "book" },
		"no ids":            func(entry *progressEntry) { entry.Source = "manual" },
	} {
		entry := base
		mutate(&entry)
		if state := progressState(entry, updatedAt); state != nil {
			t.Errorf("%s: state = %#v, want nil", name, state)
		}
	}
	entry := base
	entry.Position = 2400
	state := progressState(entry, updatedAt)
	if state == nil || state.GetProgress().GetProgressPercent() != 99.999 || state.GetProviderItemKey() != "progress:movie:tmdb:603:0:0" ||
		!state.GetProgress().GetPausedAt().AsTime().Equal(updatedAt) {
		t.Fatalf("finished-position state = %#v", state)
	}
}

func TestProtoMediaType(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]pluginv1.WatchSyncMediaType{
		"movie":     pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
		" Movie ":   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
		"EPISODE":   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
		"tv":        pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_UNSPECIFIED,
		"":          pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_UNSPECIFIED,
		"audiobook": pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_UNSPECIFIED,
	} {
		if got := protoMediaType(value); got != want {
			t.Errorf("protoMediaType(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestNextOffsetFromPagination(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		page     pagination
		current  int
		wantNext int
		wantMore bool
		wantErr  bool
	}{
		{name: "last page by total", page: pagination{Total: 10, Limit: 5, Offset: 5}, current: 5},
		{name: "no total", page: pagination{Limit: 5}},
		{name: "more by total", page: pagination{Total: 11, Limit: 5, Offset: 5}, current: 5, wantNext: 10, wantMore: true},
		{name: "zero limit steps by one", page: pagination{Total: 3}, wantNext: 1, wantMore: true},
		{name: "next link", page: pagination{Limit: 5, Next: "https://floppy.example.com/api/v1/history/?limit=5&offset=40"}, wantNext: 40, wantMore: true},
		{name: "negative offset", page: pagination{Offset: -1}, current: -1, wantErr: true},
		{name: "offset mismatch", page: pagination{Offset: 5}, current: 0, wantErr: true},
		{name: "unparseable next", page: pagination{Next: "http://[::1"}, wantErr: true},
		{name: "next without offset", page: pagination{Next: "https://floppy.example.com/?page=2"}, wantErr: true},
		{name: "next goes backwards", page: pagination{Offset: 10, Next: "https://floppy.example.com/?offset=10"}, current: 10, wantErr: true},
	} {
		next, more, fault := nextOffsetFromPagination(test.page, test.current)
		if test.wantErr {
			if fault.GetSafeMessage() != "Floppy returned invalid pagination" || more {
				t.Errorf("%s: fault = %#v more = %t, want invalid pagination", test.name, fault, more)
			}
			continue
		}
		if fault != nil || next != test.wantNext || more != test.wantMore {
			t.Errorf("%s: = %d %t %#v, want %d %t", test.name, next, more, fault, test.wantNext, test.wantMore)
		}
	}
}

func TestMediaMatchesHistory(t *testing.T) {
	t.Parallel()
	season, episode := int32(1), int32(2)
	episodeEntry := historyEntry{
		MediaType: "episode",
		Item: historyItem{
			Source: "tmdb", MediaID: json.RawMessage(`"1668"`), SeasonNumber: &season, EpisodeNumber: &episode,
			ProviderExternalID: map[string]any{"tvdb_id": "79168"},
		},
	}
	episodeMedia := func(season, episode int32, ids map[string]string) *pluginv1.WatchSyncMedia {
		return &pluginv1.WatchSyncMedia{
			MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE, SeasonNumber: season, EpisodeNumber: episode, SeriesExternalIds: ids,
		}
	}
	for _, test := range []struct {
		name  string
		media *pluginv1.WatchSyncMedia
		entry historyEntry
		want  bool
	}{
		{name: "nil media", entry: episodeEntry},
		{name: "type mismatch", media: &pluginv1.WatchSyncMedia{MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE, ExternalIds: map[string]string{"tmdb": "1668"}}, entry: episodeEntry},
		{name: "season mismatch", media: episodeMedia(2, 2, map[string]string{"tmdb": "1668"}), entry: episodeEntry},
		{name: "episode mismatch", media: episodeMedia(1, 3, map[string]string{"tmdb": "1668"}), entry: episodeEntry},
		{name: "different id", media: episodeMedia(1, 2, map[string]string{"tmdb": "1669"}), entry: episodeEntry},
		{name: "namespace absent on entry", media: episodeMedia(1, 2, map[string]string{"imdb": "tt0108778"}), entry: episodeEntry},
		{name: "source id", media: episodeMedia(1, 2, map[string]string{"tmdb": "1668"}), entry: episodeEntry, want: true},
		{name: "provider external id", media: episodeMedia(1, 2, map[string]string{"tvdb": "79168"}), entry: episodeEntry, want: true},
	} {
		if got := mediaMatchesHistory(test.media, test.entry); got != test.want {
			t.Errorf("%s: match = %t, want %t", test.name, got, test.want)
		}
	}
}

func TestHistoryContainsEventIgnoresNearMissesAndChecksPagination(t *testing.T) {
	t.Parallel()
	occurredAt := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	event := &pluginv1.WatchSyncEvent{
		OccurredAt: timestamppb.New(occurredAt),
		Media:      &pluginv1.WatchSyncMedia{MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE, ExternalIds: map[string]string{"tmdb": "603"}},
	}
	var mu sync.Mutex
	var pagination map[string]any
	var query url.Values
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		query = r.URL.Query()
		page := pagination
		mu.Unlock()
		writeJSON(t, w, map[string]any{
			"pagination": page,
			"results": []any{map[string]any{"date": "2026-08-05", "entries": []any{
				movieHistoryEntry("603", "In progress", occurredAt.Format(time.RFC3339Nano)),
				movieHistoryEntry("603", "Completed", "garbage"),
				movieHistoryEntry("603", "Completed", occurredAt.Add(3*time.Second).Format(time.RFC3339Nano)),
				movieHistoryEntry("604", "Completed", occurredAt.Add(time.Second).Format(time.RFC3339Nano)),
			}}},
		})
	}))
	defer upstream.Close()
	client, err := newAPIClient(upstream.URL, "token", upstream.Client())
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	pagination = map[string]any{"total": 4, "limit": 50, "offset": 0}
	mu.Unlock()
	found, fault := historyContainsEvent(context.Background(), client, event)
	if fault != nil || found {
		t.Fatalf("near misses = found %t fault %#v, want not found", found, fault)
	}
	mu.Lock()
	gotQuery := query
	mu.Unlock()
	if gotQuery.Get("start_date") != "2026-08-04" || gotQuery.Get("end_date") != "2026-08-06" || gotQuery.Get("offset") != "0" {
		t.Fatalf("query = %v", gotQuery)
	}

	mu.Lock()
	pagination = map[string]any{"total": 4, "limit": 50, "offset": 3}
	mu.Unlock()
	found, fault = historyContainsEvent(context.Background(), client, event)
	if found || fault.GetSafeMessage() != "Floppy returned invalid pagination" {
		t.Fatalf("bad pagination = found %t fault %#v", found, fault)
	}

	event.OccurredAt = &timestamppb.Timestamp{Seconds: 1, Nanos: -1}
	found, fault = historyContainsEvent(context.Background(), client, event)
	if found || fault.GetSafeMessage() != "Completed watch events require a valid occurrence time" {
		t.Fatalf("invalid time = found %t fault %#v", found, fault)
	}
}
