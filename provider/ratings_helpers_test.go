package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

func TestRatingListingKey(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		entry ratedMediaEntry
		want  string
	}{
		{name: "item id", entry: ratedMediaEntry{ID: json.RawMessage(`17`), Item: &ratedMediaItem{MediaID: json.RawMessage(`"603"`)}}, want: "item:17"},
		{name: "media fallback", entry: ratedMediaEntry{ID: json.RawMessage(`null`), Item: &ratedMediaItem{
			MediaType: "tv", Source: "tmdb", LibraryMediaType: "anime", MediaID: json.RawMessage(`"1668"`),
		}}, want: "media:tv:tmdb:anime:1668"},
		{name: "no item", entry: ratedMediaEntry{}, want: ""},
		{name: "item without media id", entry: ratedMediaEntry{Item: &ratedMediaItem{MediaType: "movie"}}, want: ""},
	} {
		if got := ratingListingKey(test.entry); got != test.want {
			t.Errorf("%s: key = %q, want %q", test.name, got, test.want)
		}
	}
}

func TestReleaseYear(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]int32{
		"1999-03-31T00:00:00Z":      1999,
		"2003-11-05T09:30:00+09:00": 2003,
		" 1994-09-22 ":              1994,
		"2024":                      2024,
		"0000-01-01":                0,
		"abcd-01-01":                0,
		"199":                       0,
		"":                          0,
	} {
		if got := releaseYear(value); got != want {
			t.Errorf("releaseYear(%q) = %d, want %d", value, got, want)
		}
	}
}

func setRatingEvent(id string, rating int32) *pluginv1.WatchSyncEvent {
	return &pluginv1.WatchSyncEvent{
		EventId: id, Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SET_RATING, Rating: rating,
		Media: &pluginv1.WatchSyncMedia{MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE, ExternalIds: map[string]string{"tmdb": "603"}},
	}
}

func removeRatingEvent(id string) *pluginv1.WatchSyncEvent {
	event := setRatingEvent(id, 0)
	event.Operation = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_RATING
	return event
}

func TestPerPlayRatingWriteHandlesTitleRouteFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		event       *pluginv1.WatchSyncEvent
		patchStatus int
		wantStatus  pluginv1.WatchSyncApplyStatus
		wantMessage string
	}{
		{name: "removal of a vanished title", event: removeRatingEvent("r1"), patchStatus: http.StatusNotFound,
			wantStatus: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE},
		{name: "rating of a vanished title", event: setRatingEvent("s1", 8), patchStatus: http.StatusNotFound,
			wantStatus: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED, wantMessage: "Floppy is not tracking this title"},
		{name: "server error", event: setRatingEvent("s2", 8), patchStatus: http.StatusBadGateway,
			wantStatus: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY, wantMessage: "Floppy is temporarily unavailable"},
		{name: "removal succeeds", event: removeRatingEvent("r2"),
			wantStatus: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			floppy := newFakeRatingFloppy(t, map[string][]*fakeRatingRow{
				"movie/603": {{id: 1, score: floatPointer(6), status: 3, created: "2024-01-01T00:00:00Z"}},
			})
			floppy.perPlay["movie/603"] = true
			floppy.patchStatus = test.patchStatus
			response := floppy.apply(t, test.event)
			if response.GetFault() != nil || len(response.GetResults()) != 1 {
				t.Fatalf("response = %#v", response)
			}
			result := response.GetResults()[0]
			if result.GetStatus() != test.wantStatus || result.GetFault().GetSafeMessage() != test.wantMessage {
				t.Fatalf("result = %#v, want %v %q", result, test.wantStatus, test.wantMessage)
			}
			patches := floppy.patches()
			wantBody := `{"score":8}`
			if test.event.GetOperation() == pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_RATING {
				wantBody = `{"score":null}`
			}
			if len(patches) != 1 || patches[0] != "PATCH /api/v1/media/movie/tmdb/603/ "+wantBody {
				t.Fatalf("patches = %q", patches)
			}
		})
	}
}

// consumptionServer serves one title's history pages from pages and answers
// consumption PATCHes with storedScore.
func consumptionServer(t *testing.T, pages func(offset int) map[string]any, storedScore any) (*apiClient, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var requests []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI()+" "+string(body))
		mu.Unlock()
		if r.Method == http.MethodPatch {
			writeJSON(t, w, historyRow(7, storedScore, 3, "2024-01-01T00:00:00Z", ""))
			return
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		writeJSON(t, w, pages(offset))
	}))
	t.Cleanup(upstream.Close)
	client, err := newAPIClient(upstream.URL, "token", upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client, &requests
}

func TestPatchConsumptionScoreConfirmsTheStoredScore(t *testing.T) {
	t.Parallel()
	rating := int32(8)
	for _, test := range []struct {
		name   string
		rating *int32
		stored any
		ok     bool
	}{
		{name: "stored", rating: &rating, stored: 8, ok: true},
		{name: "cleared", rating: nil, stored: nil, ok: true},
		{name: "different score", rating: &rating, stored: 7},
		{name: "not cleared", rating: nil, stored: 5},
		{name: "not set", rating: &rating, stored: nil},
	} {
		client, requests := consumptionServer(t, nil, test.stored)
		status, fault := patchConsumptionScore(context.Background(), client, "/api/v1/media/movie/tmdb/603/", 7, test.rating)
		if status != 0 {
			t.Errorf("%s: status = %d", test.name, status)
		}
		if test.ok != (fault == nil) {
			t.Errorf("%s: fault = %#v, want ok %t", test.name, fault, test.ok)
		}
		if !test.ok && fault.GetSafeMessage() != "Floppy did not store the rating change; it will be retried" {
			t.Errorf("%s: fault message = %q", test.name, fault.GetSafeMessage())
		}
		if len(*requests) != 1 || !strings.HasPrefix((*requests)[0], "PATCH /api/v1/media/movie/tmdb/603/history/7/ {\"score\":") {
			t.Errorf("%s: requests = %q", test.name, *requests)
		}
	}
}

func TestLoadTitleConsumptionsFollowsPagesAndBoundsThem(t *testing.T) {
	t.Parallel()
	page := func(offset int, next bool, rows ...any) map[string]any {
		nextURL := ""
		if next {
			nextURL = "https://floppy.example.com/history/?offset=" + strconv.Itoa(offset+1)
		}
		return ratedPage(1, 0, offset, nextURL, rows...)
	}

	client, requests := consumptionServer(t, func(offset int) map[string]any {
		if offset == 0 {
			return page(0, true, historyRow(1, 6, 3, "2024-01-01T00:00:00Z", ""))
		}
		return page(1, false, historyRow(2, 9, 3, "2024-02-01T00:00:00Z", "2024-02-02T00:00:00Z"))
	}, nil)
	title, status, fault := loadTitleConsumptions(context.Background(), client, floppyMovie, "603")
	if fault != nil || status != 0 || title.perPlay || len(title.rows) != 2 || title.rows[1].id != 2 || *title.rows[1].score != 9 {
		t.Fatalf("title = %#v status %d fault %#v", title, status, fault)
	}
	if len(*requests) != 2 {
		t.Fatalf("requests = %q", *requests)
	}

	endless, requests := consumptionServer(t, func(offset int) map[string]any {
		return page(offset, true, historyRow(int64(offset+1), nil, 3, "2024-01-01T00:00:00Z", ""))
	}, nil)
	_, _, fault = loadTitleConsumptions(context.Background(), endless, floppyMovie, "603")
	if fault.GetSafeMessage() != "Floppy returned too much rating history for one title" || len(*requests) != ratingHistoryMaxPages {
		t.Fatalf("endless history fault = %#v after %d requests", fault, len(*requests))
	}

	unreadable, _ := consumptionServer(t, func(int) map[string]any {
		return page(0, false, historyRow(1, 6, 3, "yesterday", ""))
	}, nil)
	_, _, fault = loadTitleConsumptions(context.Background(), unreadable, floppyMovie, "603")
	if fault.GetSafeMessage() != "Floppy returned an unreadable rating history" {
		t.Fatalf("unreadable history fault = %#v", fault)
	}

	misPaged, _ := consumptionServer(t, func(int) map[string]any {
		return page(5, false, historyRow(1, 6, 3, "2024-01-01T00:00:00Z", ""))
	}, nil)
	_, _, fault = loadTitleConsumptions(context.Background(), misPaged, floppyMovie, "603")
	if fault.GetSafeMessage() != "Floppy returned invalid pagination" {
		t.Fatalf("mis-paged history fault = %#v", fault)
	}
}
