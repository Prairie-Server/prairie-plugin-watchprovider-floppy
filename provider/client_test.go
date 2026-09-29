package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

func TestFaultForHTTPResponseMapsStatusCodes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		status      int
		retryAfter  string
		wantCode    pluginv1.WatchSyncFaultCode
		wantMessage string
		wantRetry   time.Duration
	}{
		{status: http.StatusUnauthorized, wantCode: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL, wantMessage: "Floppy rejected the API token"},
		{status: http.StatusForbidden, wantCode: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL, wantMessage: "Floppy rejected the API token"},
		{status: http.StatusTooManyRequests, retryAfter: "45", wantCode: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED, wantMessage: "Floppy rate limit reached", wantRetry: 45 * time.Second},
		{status: http.StatusRequestTimeout, wantCode: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, wantMessage: "Floppy request timed out"},
		{status: http.StatusBadRequest, wantCode: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, wantMessage: "Floppy rejected the request (HTTP 400)"},
		{status: http.StatusNotFound, wantCode: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, wantMessage: "Floppy rejected the request (HTTP 404)"},
		{status: http.StatusConflict, wantCode: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, wantMessage: "Floppy rejected the request (HTTP 409)"},
		{status: http.StatusUnprocessableEntity, wantCode: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, wantMessage: "Floppy rejected the request (HTTP 422)"},
		{status: http.StatusInternalServerError, wantCode: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, wantMessage: "Floppy is temporarily unavailable"},
		{status: http.StatusServiceUnavailable, wantCode: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, wantMessage: "Floppy is temporarily unavailable"},
		{status: http.StatusTeapot, wantCode: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT, wantMessage: "Floppy request failed (HTTP 418)"},
		{status: http.StatusPermanentRedirect, wantCode: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT, wantMessage: "Floppy request failed (HTTP 308)"},
	} {
		header := make(http.Header)
		if test.retryAfter != "" {
			header.Set("Retry-After", test.retryAfter)
		}
		fault := faultForHTTPResponse(&http.Response{StatusCode: test.status, Header: header})
		if fault.GetCode() != test.wantCode || fault.GetSafeMessage() != test.wantMessage {
			t.Errorf("HTTP %d fault = %v %q, want %v %q", test.status, fault.GetCode(), fault.GetSafeMessage(), test.wantCode, test.wantMessage)
		}
		if got := fault.GetRetryAfter().AsDuration(); got != test.wantRetry {
			t.Errorf("HTTP %d retry after = %v, want %v", test.status, got, test.wantRetry)
		}
	}
}

func TestRetryAfterParsesSecondsAndHTTPDates(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{value: "30", want: 30 * time.Second},
		{value: " 7 ", want: 7 * time.Second},
		{value: "0", want: 0},
		{value: "-5", want: 0},
		{value: "", want: 0},
		{value: "soon", want: 0},
		{value: time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), want: 0},
	} {
		if got := retryAfter(test.value); got != test.want {
			t.Errorf("retryAfter(%q) = %v, want %v", test.value, got, test.want)
		}
	}
	future := retryAfter(time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
	if future <= 58*time.Minute || future > time.Hour {
		t.Fatalf("retryAfter(future date) = %v, want about one hour", future)
	}
}

func TestTemporaryAndPermanentFaults(t *testing.T) {
	t.Parallel()
	withRetry := temporaryFault("busy", 12*time.Second)
	if withRetry.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY || withRetry.GetSafeMessage() != "busy" ||
		withRetry.GetRetryAfter().AsDuration() != 12*time.Second {
		t.Fatalf("temporary fault with retry = %#v", withRetry)
	}
	if withoutRetry := temporaryFault("busy", 0); withoutRetry.GetRetryAfter() != nil {
		t.Fatalf("temporary fault without retry has retry_after %v", withoutRetry.GetRetryAfter())
	}
	permanent := permanentFault("broken")
	if permanent.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT || permanent.GetSafeMessage() != "broken" || permanent.GetRetryAfter() != nil {
		t.Fatalf("permanent fault = %#v", permanent)
	}
}

func TestNewAPIClientValidatesBaseURL(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "floppy.example.com", "/relative", "ftp://floppy.example.com", "https://"} {
		if _, err := newAPIClient(raw, "token", nil); err == nil || !strings.Contains(err.Error(), "absolute http or https") {
			t.Errorf("newAPIClient(%q) err = %v, want absolute URL error", raw, err)
		}
	}
	for _, raw := range []string{"https://floppy.example.com/?debug=1", "https://floppy.example.com/#top"} {
		if _, err := newAPIClient(raw, "token", nil); err == nil || !strings.Contains(err.Error(), "query, or a fragment") {
			t.Errorf("newAPIClient(%q) err = %v, want query/fragment error", raw, err)
		}
	}
	client, err := newAPIClient("  https://floppy.example.com/base/  ", " token ", nil)
	if err != nil {
		t.Fatal(err)
	}
	if client.token != "token" {
		t.Fatalf("token = %q, want trimmed", client.token)
	}
	if got := client.endpoint("/api/v1/history/", nil); got != "https://floppy.example.com/base/api/v1/history/" {
		t.Fatalf("endpoint = %q", got)
	}
}

func TestRequestReportsLocalAndTransportFailures(t *testing.T) {
	t.Parallel()
	client, err := newAPIClient("https://floppy.example.com", "token", nil)
	if err != nil {
		t.Fatal(err)
	}
	status, fault := client.request(context.Background(), http.MethodPost, "/x", nil, make(chan int), nil, "Bearer")
	if status != 0 || fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT || fault.GetSafeMessage() != "Floppy request could not be encoded" {
		t.Fatalf("unencodable payload = %d %#v", status, fault)
	}
	status, fault = client.request(context.Background(), "BAD METHOD", "/x", nil, nil, nil, "Bearer")
	if status != 0 || fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT || fault.GetSafeMessage() != "Floppy request could not be created" {
		t.Fatalf("invalid method = %d %#v", status, fault)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	unreachable, err := newAPIClient(closedURL, "token", nil)
	if err != nil {
		t.Fatal(err)
	}
	status, fault = unreachable.request(context.Background(), http.MethodGet, "/x", nil, nil, nil, "Bearer")
	if status != 0 || fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY || fault.GetSafeMessage() != "Floppy is temporarily unreachable" {
		t.Fatalf("unreachable = %d %#v", status, fault)
	}
}

func TestRequestHandlesResponseBodies(t *testing.T) {
	t.Parallel()
	var authorization []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = append(authorization, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/garbage":
			_, _ = w.Write([]byte("<html>not json</html>"))
		case "/empty":
			w.WriteHeader(http.StatusNoContent)
		case "/teapot":
			w.WriteHeader(http.StatusTeapot)
		default:
			writeJSON(t, w, map[string]any{"valid": true, "user_name": "quick"})
		}
	}))
	defer upstream.Close()
	client, err := newAPIClient(upstream.URL, "", upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	var output validateTokenResponse
	status, fault := client.request(context.Background(), http.MethodGet, "/garbage", nil, nil, &output, "Bearer")
	if status != http.StatusOK || fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY || fault.GetSafeMessage() != "Floppy returned an unreadable response" {
		t.Fatalf("garbage body = %d %#v", status, fault)
	}
	status, fault = client.request(context.Background(), http.MethodGet, "/empty", nil, nil, &output, "Bearer")
	if status != http.StatusNoContent || fault != nil || output.Valid {
		t.Fatalf("no content = %d %#v output %#v", status, fault, output)
	}
	status, fault = client.request(context.Background(), http.MethodGet, "/teapot", nil, nil, &output, "Bearer")
	if status != http.StatusTeapot || fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT {
		t.Fatalf("teapot = %d %#v", status, fault)
	}
	status, fault = client.request(context.Background(), http.MethodGet, "/ok", nil, nil, &output, "Bearer")
	if status != http.StatusOK || fault != nil || !output.Valid || output.Username != "quick" {
		t.Fatalf("ok = %d %#v output %#v", status, fault, output)
	}
	for _, header := range authorization {
		if header != "" {
			t.Fatalf("Authorization sent without a token: %q", header)
		}
	}
}
