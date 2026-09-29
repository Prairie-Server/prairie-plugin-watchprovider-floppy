package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

// validateServer answers validate-token with the given status and body and
// counts the calls.
func validateServer(t *testing.T, status int, body map[string]any) (*httptest.Server, *int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/listenbrainz/1/validate-token" || r.Header.Get("Authorization") != "Token token" {
			t.Errorf("request = %s %s auth %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		mu.Lock()
		calls++
		mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		writeJSON(t, w, body)
	}))
	t.Cleanup(upstream.Close)
	return upstream, &calls
}

func TestRefreshCredentialsRevalidatesAndReturnsAnIndependentCopy(t *testing.T) {
	t.Parallel()
	upstream, calls := validateServer(t, http.StatusOK, map[string]any{"valid": true, "user_name": "quick"})
	auth := authenticatedContext(upstream.URL)
	expiresAt := timestamppb.New(time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC))
	auth.Credentials.RefreshToken = "refresh"
	auth.Credentials.ExpiresAt = expiresAt
	auth.Credentials.Scopes = []string{"watchlist:read", "watchlist:write"}

	response, err := NewServer(upstream.Client()).RefreshCredentials(context.Background(), &pluginv1.WatchSyncRefreshCredentialsRequest{Context: auth})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFault() != nil || *calls != 1 {
		t.Fatalf("fault = %v, validate calls = %d", response.GetFault(), *calls)
	}
	got := response.GetCredentials()
	if got.GetAccessToken() != "token" || got.GetRefreshToken() != "refresh" || got.GetTokenType() != "Bearer" ||
		!got.GetExpiresAt().AsTime().Equal(expiresAt.AsTime()) || len(got.GetScopes()) != 2 || got.GetScopes()[1] != "watchlist:write" ||
		got.GetSecretAttributes()[configBaseURL] != upstream.URL {
		t.Fatalf("credentials = %#v", got)
	}
	if response.GetAccount().GetUsername() != "quick" || response.GetAccount().GetDisplayName() != "quick" {
		t.Fatalf("account = %#v", response.GetAccount())
	}
	auth.Credentials.Scopes[0] = "mutated"
	auth.Credentials.SecretAttributes[configBaseURL] = "https://mutated.example.com"
	if got.GetScopes()[0] != "watchlist:read" || got.GetSecretAttributes()[configBaseURL] != upstream.URL {
		t.Fatalf("returned credentials alias the request: %#v", got)
	}
}

func TestRefreshCredentialsReportsFaults(t *testing.T) {
	t.Parallel()
	response, err := NewServer(nil).RefreshCredentials(context.Background(), &pluginv1.WatchSyncRefreshCredentialsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST ||
		response.GetFault().GetSafeMessage() != "Floppy credentials are required" || response.GetCredentials() != nil {
		t.Fatalf("missing context response = %#v", response)
	}

	upstream, _ := validateServer(t, http.StatusOK, map[string]any{"valid": false, "user_name": "quick"})
	response, err = NewServer(upstream.Client()).RefreshCredentials(context.Background(), &pluginv1.WatchSyncRefreshCredentialsRequest{Context: authenticatedContext(upstream.URL)})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL ||
		response.GetFault().GetSafeMessage() != "Floppy did not validate the API token" || response.GetCredentials() != nil {
		t.Fatalf("invalid token response = %#v", response)
	}
}

func TestGetAccountReturnsValidatedAccountOrFault(t *testing.T) {
	t.Parallel()
	upstream, _ := validateServer(t, http.StatusOK, map[string]any{"valid": true, "user_name": "alice"})
	response, err := NewServer(upstream.Client()).GetAccount(context.Background(), &pluginv1.WatchSyncGetAccountRequest{Context: authenticatedContext(upstream.URL)})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFault() != nil || response.GetAccount().GetExternalSubject() != "alice" || response.GetAccount().GetUsername() != "alice" {
		t.Fatalf("response = %#v", response)
	}

	rejected, _ := validateServer(t, http.StatusUnauthorized, nil)
	response, err = NewServer(rejected.Client()).GetAccount(context.Background(), &pluginv1.WatchSyncGetAccountRequest{Context: authenticatedContext(rejected.URL)})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetAccount() != nil || response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL {
		t.Fatalf("rejected response = %#v", response)
	}

	blank, _ := validateServer(t, http.StatusOK, map[string]any{"valid": true, "user_name": "  "})
	response, err = NewServer(blank.Client()).GetAccount(context.Background(), &pluginv1.WatchSyncGetAccountRequest{Context: authenticatedContext(blank.URL)})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetAccount() != nil || response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL {
		t.Fatalf("blank username response = %#v", response)
	}

	response, err = NewServer(nil).GetAccount(context.Background(), &pluginv1.WatchSyncGetAccountRequest{
		Context: &pluginv1.WatchSyncAuthenticatedContext{CapabilityId: capabilityID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFault().GetSafeMessage() != "Floppy credentials are required" {
		t.Fatalf("no credentials response = %#v", response)
	}
}

func TestExchangeAPIKeyRejectsBadInputsBeforeCallingFloppy(t *testing.T) {
	t.Parallel()
	upstream, calls := validateServer(t, http.StatusOK, map[string]any{"valid": true, "user_name": "quick"})
	server := NewServer(upstream.Client())
	for _, test := range []struct {
		name string
		req  *pluginv1.WatchSyncExchangeAPIKeyRequest
		want string
	}{
		{name: "capability", req: &pluginv1.WatchSyncExchangeAPIKeyRequest{CapabilityId: "trakt", ProviderConfig: providerConfig(upstream.URL), ApiKey: "token"}, want: "Unknown Floppy capability"},
		{name: "token", req: &pluginv1.WatchSyncExchangeAPIKeyRequest{CapabilityId: capabilityID, ProviderConfig: providerConfig(upstream.URL), ApiKey: "   "}, want: "Floppy API token is required"},
		{name: "base URL", req: &pluginv1.WatchSyncExchangeAPIKeyRequest{CapabilityId: capabilityID, ApiKey: "token"}, want: "Floppy base URL must be an absolute http or https URL"},
	} {
		response, err := server.ExchangeAPIKey(context.Background(), test.req)
		if err != nil {
			t.Fatal(err)
		}
		if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST || response.GetFault().GetSafeMessage() != test.want || response.GetCredentials() != nil {
			t.Errorf("%s: response = %#v", test.name, response)
		}
	}
	if *calls != 0 {
		t.Fatalf("validate calls = %d, want 0", *calls)
	}
}

func TestExchangeAPIKeyReadsSecretConfigAndReportsValidationFaults(t *testing.T) {
	t.Parallel()
	upstream, calls := validateServer(t, http.StatusServiceUnavailable, nil)
	response, err := NewServer(upstream.Client()).ExchangeAPIKey(context.Background(), &pluginv1.WatchSyncExchangeAPIKeyRequest{
		CapabilityId:   capabilityID,
		ProviderConfig: &pluginv1.WatchSyncProviderConfig{SecretValues: map[string]string{configBaseURL: upstream.URL}},
		ApiKey:         "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatalf("validate calls = %d, want 1 (secret base URL was not used)", *calls)
	}
	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY || response.GetCredentials() != nil {
		t.Fatalf("response = %#v", response)
	}
}

func TestCloneHelpersHandleEmptyValues(t *testing.T) {
	t.Parallel()
	if cloneCredentials(nil) != nil {
		t.Fatal("cloneCredentials(nil) != nil")
	}
	if cloneMap(nil) != nil || cloneMap(map[string]string{}) != nil {
		t.Fatal("cloneMap of empty input should be nil")
	}
	clone := cloneCredentials(&pluginv1.WatchSyncCredentials{AccessToken: "a"})
	if clone.GetAccessToken() != "a" || clone.GetSecretAttributes() != nil || len(clone.GetScopes()) != 0 {
		t.Fatalf("clone = %#v", clone)
	}
}

func TestListRemoteStateRejectsUnsupportedRequests(t *testing.T) {
	t.Parallel()
	server := NewServer(nil)
	response, err := server.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFault().GetSafeMessage() != "Floppy credentials are required" {
		t.Fatalf("no context response = %#v", response)
	}
	for _, test := range []struct {
		kinds []pluginv1.WatchSyncRemoteStateKind
		want  string
	}{
		{
			kinds: []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED, pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_PROGRESS},
			want:  "Floppy accepts one state family per traversal",
		},
		{
			kinds: []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_FAVORITE},
			want:  "Floppy does not support the requested state family",
		},
		{
			kinds: []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST},
			want:  "Floppy does not support the requested state family",
		},
	} {
		response, err := server.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{
			Context: authenticatedContext("https://floppy.example.com"), StateKinds: test.kinds,
		})
		if err != nil {
			t.Fatal(err)
		}
		if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST || response.GetFault().GetSafeMessage() != test.want {
			t.Errorf("kinds %v response = %#v", test.kinds, response)
		}
	}
}

func TestListRemoteStateDefaultsToWatchedHistory(t *testing.T) {
	t.Parallel()
	var path string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		writeJSON(t, w, map[string]any{"pagination": map[string]any{"total": 0, "limit": 50, "offset": 0}, "results": []any{}})
	}))
	defer upstream.Close()
	response, err := NewServer(upstream.Client()).ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{Context: authenticatedContext(upstream.URL)})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/api/v1/history/" || response.GetFault() != nil || !response.GetCompleteSnapshot() || response.GetNextCursor() != "" || len(response.GetItems()) != 0 {
		t.Fatalf("path %q response = %#v", path, response)
	}
}

func TestTraversalRejectsMalformedPageTokens(t *testing.T) {
	t.Parallel()
	encode := func(raw string) string { return base64.RawURLEncoding.EncodeToString([]byte(raw)) }
	for _, token := range []string{
		"***not base64***",
		encode("not json"),
		encode(`{"offset":-1,"high_water":"2026-08-05T12:00:00Z"}`),
		encode(`{"offset":5}`),
	} {
		_, fault := traversal(&pluginv1.WatchSyncListRemoteStateRequest{PageToken: token})
		if fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST || fault.GetSafeMessage() != "Floppy page token is invalid" {
			t.Errorf("traversal(%q) fault = %#v", token, fault)
		}
	}
	highWater := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	token, fault := traversal(&pluginv1.WatchSyncListRemoteStateRequest{PageToken: encodePageToken(traversalToken{Offset: 3, HighWater: highWater})})
	if fault != nil || token.Offset != 3 || !token.HighWater.Equal(highWater) {
		t.Fatalf("round trip = %#v, %#v", token, fault)
	}
	token, fault = traversal(&pluginv1.WatchSyncListRemoteStateRequest{PageToken: "  "})
	if fault != nil || token.Offset != 0 || !token.HighWater.IsZero() {
		t.Fatalf("blank token = %#v, %#v", token, fault)
	}
}

func TestParseCursor(t *testing.T) {
	t.Parallel()
	if parsed, fault := parseCursor(" "); fault != nil || !parsed.IsZero() {
		t.Fatalf("blank cursor = %v, %#v", parsed, fault)
	}
	parsed, fault := parseCursor("2026-08-05T14:00:00.5+02:00")
	if fault != nil || !parsed.Equal(time.Date(2026, time.August, 5, 12, 0, 0, 500_000_000, time.UTC)) || parsed.Location() != time.UTC {
		t.Fatalf("cursor = %v, %#v", parsed, fault)
	}
	if _, fault := parseCursor("yesterday"); fault.GetSafeMessage() != "Floppy cursor is invalid" {
		t.Fatalf("invalid cursor fault = %#v", fault)
	}
}

func TestStringValueFormatsDecodedJSON(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		value any
		want  string
	}{
		{value: nil, want: ""},
		{value: "tt0133093", want: "tt0133093"},
		{value: float64(603), want: "603"},
		{value: 1.5, want: "1.5"},
		{value: json.Number("79168"), want: "79168"},
		{value: 42, want: "42"},
		{value: true, want: "true"},
	} {
		if got := stringValue(test.value); got != test.want {
			t.Errorf("stringValue(%#v) = %q, want %q", test.value, got, test.want)
		}
	}
	ids := normalizedIDs(map[string]any{"TMDB_ID": float64(603), "imdb": "tt0133093", "tvdb": "", "trakt": "1"})
	if len(ids) != 2 || ids["tmdb"] != "603" || ids["imdb"] != "tt0133093" {
		t.Fatalf("normalizedIDs = %#v", ids)
	}
}

func TestMergedExternalIDs(t *testing.T) {
	t.Parallel()
	movie := mergedExternalIDs(&pluginv1.WatchSyncMedia{
		MediaType:         pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
		ExternalIds:       map[string]string{"TMDB": " 603 ", "plex": "x"},
		SeriesExternalIds: map[string]string{"tmdb": "9", "imdb_id": "tt0133093"},
	})
	if len(movie) != 2 || movie["tmdb"] != "603" || movie["imdb"] != "tt0133093" {
		t.Fatalf("movie ids = %#v", movie)
	}
	episodeWithoutSeries := mergedExternalIDs(&pluginv1.WatchSyncMedia{
		MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
		ExternalIds: map[string]string{"tvdb": "123"},
	})
	if len(episodeWithoutSeries) != 1 || episodeWithoutSeries["tvdb"] != "123" {
		t.Fatalf("episode ids = %#v", episodeWithoutSeries)
	}
	onlySeries := mergedExternalIDs(&pluginv1.WatchSyncMedia{
		MediaType:         pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
		SeriesExternalIds: map[string]string{"tvdb": "5"},
	})
	if len(onlySeries) != 1 || onlySeries["tvdb"] != "5" {
		t.Fatalf("series-only ids = %#v", onlySeries)
	}
	if none := mergedExternalIDs(&pluginv1.WatchSyncMedia{MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE}); none == nil || len(none) != 0 {
		t.Fatalf("no ids = %#v, want empty non-nil map", none)
	}
}

func TestTimestampParsesRFC3339(t *testing.T) {
	t.Parallel()
	if got := timestamp("2026-08-05T12:00:00Z"); got == nil || !got.AsTime().Equal(time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("timestamp = %v", got)
	}
	if got := timestamp("2026-08-05"); got != nil {
		t.Fatalf("timestamp(date only) = %v, want nil", got)
	}
}

func TestPayloadFromEventRejectsUnsupportedEvents(t *testing.T) {
	t.Parallel()
	movie := &pluginv1.WatchSyncMedia{MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE, ExternalIds: map[string]string{"tmdb": "603"}}
	for _, test := range []struct {
		name  string
		event *pluginv1.WatchSyncEvent
		want  string
	}{
		{name: "no media", event: &pluginv1.WatchSyncEvent{Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START}, want: "Watch event media is required"},
		{name: "episode without numbers", event: &pluginv1.WatchSyncEvent{
			Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START,
			Media:     &pluginv1.WatchSyncMedia{MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE, SeriesExternalIds: map[string]string{"tmdb": "1668"}, SeasonNumber: 1},
		}, want: "Episode events require season and episode numbers"},
		{name: "series", event: &pluginv1.WatchSyncEvent{
			Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED,
			Media:     &pluginv1.WatchSyncMedia{MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES, ExternalIds: map[string]string{"tmdb": "1668"}},
		}, want: "Floppy supports movie and episode events only"},
		{name: "no ids", event: &pluginv1.WatchSyncEvent{
			Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START,
			Media:     &pluginv1.WatchSyncMedia{MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE, ExternalIds: map[string]string{"plex": "1"}},
		}, want: "Watch event needs a TMDB, IMDb, or TVDB identifier"},
		{name: "unwatch", event: &pluginv1.WatchSyncEvent{Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_UNWATCHED, Media: movie}, want: "Floppy does not support this watch operation"},
	} {
		_, completed, fault := payloadFromEvent(test.event)
		if completed || fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST || fault.GetSafeMessage() != test.want {
			t.Errorf("%s: completed %t fault %#v, want %q", test.name, completed, fault, test.want)
		}
	}
}

func TestPayloadFromEventMapsPlaybackActions(t *testing.T) {
	t.Parallel()
	occurredAt := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.FixedZone("CEST", 2*60*60))
	for _, test := range []struct {
		operation pluginv1.WatchSyncOperation
		action    string
	}{
		{operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START, action: "start"},
		{operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_PAUSE, action: "pause"},
	} {
		payload, completed, fault := payloadFromEvent(&pluginv1.WatchSyncEvent{
			Operation: test.operation, PositionSeconds: 61.6, DurationSeconds: 5400.4, OccurredAt: timestamppb.New(occurredAt),
			Media: &pluginv1.WatchSyncMedia{
				MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE, Title: "The Matrix",
				ExternalIds: map[string]string{"imdb": "tt0133093"},
			},
		})
		if fault != nil || completed {
			t.Fatalf("%v: completed %t fault %#v", test.operation, completed, fault)
		}
		if payload.Action != test.action || payload.MediaType != "movie" || payload.Title != "The Matrix" || payload.IDs["imdb"] != "tt0133093" ||
			payload.Completed != nil || payload.PositionSeconds == nil || *payload.PositionSeconds != 62 ||
			payload.DurationSeconds == nil || *payload.DurationSeconds != 5400 || payload.PlayedAt != "2026-08-05T10:00:00Z" ||
			payload.SeasonNumber != nil || payload.EpisodeNumber != nil {
			t.Fatalf("%v: payload = %#v", test.operation, payload)
		}
	}
	payload, _, fault := payloadFromEvent(&pluginv1.WatchSyncEvent{
		Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START,
		Media:     &pluginv1.WatchSyncMedia{MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE, ExternalIds: map[string]string{"tmdb": "603"}},
	})
	if fault != nil || payload.PositionSeconds != nil || payload.DurationSeconds != nil || payload.PlayedAt != "" {
		t.Fatalf("payload without playback details = %#v", payload)
	}
}

func TestApplyEventsRejectsInvalidEventsPerEvent(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	defer upstream.Close()
	response, err := NewServer(upstream.Client()).ApplyEvents(context.Background(), &pluginv1.WatchSyncApplyEventsRequest{
		Context: authenticatedContext(upstream.URL),
		Events: []*pluginv1.WatchSyncEvent{
			{EventId: "  "},
			{EventId: "no-media", Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START},
			{EventId: "no-time", Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED, Media: &pluginv1.WatchSyncMedia{
				MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE, ExternalIds: map[string]string{"tmdb": "603"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFault() != nil || len(response.GetResults()) != 3 {
		t.Fatalf("response = %#v", response)
	}
	for index, want := range []struct {
		eventID string
		message string
	}{
		{eventID: "", message: "Watch event ID is required"},
		{eventID: "no-media", message: "Watch event media is required"},
		{eventID: "no-time", message: "Completed watch events require a valid occurrence time"},
	} {
		result := response.GetResults()[index]
		if result.GetEventId() != want.eventID || result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED || result.GetFault().GetSafeMessage() != want.message {
			t.Errorf("result %d = %#v, want %q", index, result, want.message)
		}
	}

	response, err = NewServer(nil).ApplyEvents(context.Background(), &pluginv1.WatchSyncApplyEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFault().GetSafeMessage() != "Floppy credentials are required" {
		t.Fatalf("no context response = %#v", response)
	}
}

func TestApplyEventsMapsUpstreamFaults(t *testing.T) {
	t.Parallel()
	occurredAt := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	watched := func(id string) *pluginv1.WatchSyncEvent {
		return &pluginv1.WatchSyncEvent{
			EventId: id, Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED, OccurredAt: timestamppb.New(occurredAt),
			Media: &pluginv1.WatchSyncMedia{MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE, ExternalIds: map[string]string{"tmdb": "603"}},
		}
	}
	emptyHistory := func(t *testing.T, w http.ResponseWriter) {
		writeJSON(t, w, map[string]any{"pagination": map[string]any{"total": 0, "limit": 50, "offset": 0}, "results": []any{}})
	}
	for _, test := range []struct {
		name            string
		historyStatus   int
		scrobbleStatus  int
		wantConnection  pluginv1.WatchSyncFaultCode
		wantEventStatus pluginv1.WatchSyncApplyStatus
		wantEventCode   pluginv1.WatchSyncFaultCode
	}{
		{name: "history unauthorized", historyStatus: http.StatusUnauthorized, wantConnection: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL},
		{name: "history unavailable", historyStatus: http.StatusBadGateway,
			wantEventStatus: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY, wantEventCode: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY},
		{name: "scrobble rejected", scrobbleStatus: http.StatusUnprocessableEntity,
			wantEventStatus: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED, wantEventCode: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST},
		{name: "scrobble unauthorized", scrobbleStatus: http.StatusForbidden, wantConnection: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/history/":
					if test.historyStatus != 0 {
						w.WriteHeader(test.historyStatus)
						return
					}
					emptyHistory(t, w)
				case "/api/v1/scrobble/":
					if test.historyStatus != 0 {
						t.Errorf("scrobble sent after failed history lookup")
					}
					if test.scrobbleStatus != 0 {
						w.WriteHeader(test.scrobbleStatus)
						return
					}
					writeJSON(t, w, map[string]any{"detail": "accepted"})
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			response, err := NewServer(upstream.Client()).ApplyEvents(context.Background(), &pluginv1.WatchSyncApplyEventsRequest{
				Context: authenticatedContext(upstream.URL), Events: []*pluginv1.WatchSyncEvent{watched("e1")},
			})
			if err != nil {
				t.Fatal(err)
			}
			if test.wantConnection != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_UNSPECIFIED {
				if response.GetFault().GetCode() != test.wantConnection || len(response.GetResults()) != 0 {
					t.Fatalf("response = %#v, want connection fault %v", response, test.wantConnection)
				}
				return
			}
			if response.GetFault() != nil || len(response.GetResults()) != 1 {
				t.Fatalf("response = %#v", response)
			}
			result := response.GetResults()[0]
			if result.GetEventId() != "e1" || result.GetStatus() != test.wantEventStatus || result.GetFault().GetCode() != test.wantEventCode {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}
