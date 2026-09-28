package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// errFakeTransport stands in for a host.http.do transport failure.
var errFakeTransport = errors.New("fake transport failure")

const (
	creditsURL = codexBaseURL + resetCreditsPath
	consumeURL = codexBaseURL + resetConsumePath
	quotaURL   = defaultManagementBaseURL + managementResetQuotaPath
)

// waitFor polls a condition until it holds. The reset flow runs on its own
// goroutine, so tests wait on an observable host-call effect rather than sleeping a
// fixed amount.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within 2s")
		}
		time.Sleep(time.Millisecond)
	}
}

func logsWithMessage(logs []fakeLog, message string) []fakeLog {
	var matched []fakeLog
	for _, entry := range logs {
		if entry.Message == message {
			matched = append(matched, entry)
		}
	}
	return matched
}

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func assertHeader(t *testing.T, request httpRequest, name, want string) {
	t.Helper()
	if got := request.Headers.Get(name); got != want {
		t.Errorf("%s header = %q, want %q", name, got, want)
	}
}

// hitBody is the weekly-exhaustion error body that makes classify() report a hit.
func hitBody() string {
	return quotaBody(true, `"type":"usage_limit_reached","limit_window_minutes":10080,"resets_in_seconds":200000`)
}

func resetConfig(managementKey string) pluginConfig {
	cfg := defaultConfig()
	cfg.ManagementKey = managementKey
	return cfg
}

// creditsBody renders an upstream credits listing; unknown top-level fields are part
// of the real payload and must be ignored.
func creditsBody(credits string) string {
	return `{"credits":[` + credits + `],"available_count":9,"total_earned_count":9}`
}

// scriptedResetHost returns a fake with the credential, credit listing, consume and
// cooldown-clear responses a happy flow needs; callers override individual routes.
func scriptedResetHost(credits string, consumeStatus int, consumeCode string) *fakeHost {
	fake := newFakeHost(
		fakeHTTPRoute{Method: http.MethodGet, URL: creditsURL, Status: 200, Body: creditsBody(credits)},
		fakeHTTPRoute{Method: http.MethodPost, URL: consumeURL, Status: consumeStatus, Body: `{"code":"` + consumeCode + `"}`},
		fakeHTTPRoute{Method: http.MethodPost, URL: quotaURL, Status: 200, Body: `{"status":"ok"}`},
	)
	fake.auths["c1f0a9"] = pluginapi.HostAuthGetResponse{JSON: json.RawMessage(`{"access_token":"tok-123","account_id":"acct-123"}`)}
	return fake
}

func jwtWithAccountID(t *testing.T, accountID string) string {
	t.Helper()
	payload, errMarshal := json.Marshal(map[string]any{
		accountIDClaimKey: map[string]string{"chatgpt_account_id": accountID},
	})
	if errMarshal != nil {
		t.Fatalf("marshal id_token claims: %v", errMarshal)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestResetFlowConsumesEarliestCreditAndClearsCooldown(t *testing.T) {
	fake := scriptedResetHost(`
		{"id":"later","status":"available","expires_at":"2026-08-01T00:00:00Z"},
		{"id":"redeemed","status":"redeemed","expires_at":"2026-06-01T00:00:00Z"},
		{"id":"earliest","status":"available","expires_at":"2026-07-01T00:00:00Z"},
		{"id":"forever","status":"available","expires_at":null}`, http.StatusOK, consumeCodeReset)

	if !runReset(fake, resetConfig("secret-key"), usageRecord(hitBody())) {
		t.Fatalf("runReset = false, want a consumed credit")
	}

	want := []string{
		"auth.get c1f0a9",
		"http.do GET " + creditsURL,
		"http.do POST " + consumeURL,
		"http.do POST " + quotaURL,
	}
	if trace := fake.callTrace(); !reflect.DeepEqual(trace, want) {
		t.Fatalf("call trace = %v, want %v", trace, want)
	}

	requests := fake.requests()
	get, consume, quota := requests[0], requests[1], requests[2]
	assertHeader(t, get, "Authorization", "Bearer tok-123")
	assertHeader(t, get, "ChatGPT-Account-ID", "acct-123")
	assertHeader(t, get, "User-Agent", codexUserAgent)
	assertHeader(t, get, "originator", codexOriginator)
	if contentType := get.Headers.Get("Content-Type"); contentType != "" {
		t.Errorf("GET Content-Type = %q, want none", contentType)
	}

	assertHeader(t, consume, "Content-Type", "application/json")
	var redeem consumeRequest
	if errUnmarshal := json.Unmarshal(consume.Body, &redeem); errUnmarshal != nil {
		t.Fatalf("decode consume body %s: %v", consume.Body, errUnmarshal)
	}
	if redeem.CreditID != "earliest" {
		t.Errorf("consume credit_id = %q, want the earliest-expiring available credit", redeem.CreditID)
	}
	if !uuidV4Pattern.MatchString(redeem.RedeemRequestID) {
		t.Errorf("redeem_request_id = %q, want a UUID v4", redeem.RedeemRequestID)
	}

	assertHeader(t, quota, "Authorization", "Bearer secret-key")
	var resetQuota managementResetQuotaRequest
	if errUnmarshal := json.Unmarshal(quota.Body, &resetQuota); errUnmarshal != nil {
		t.Fatalf("decode reset-quota body %s: %v", quota.Body, errUnmarshal)
	}
	if resetQuota.AuthIndex != "c1f0a9" {
		t.Errorf("reset-quota auth_index = %q, want the runtime auth index", resetQuota.AuthIndex)
	}

	logs := fake.logged()
	if got := logsWithMessage(logs, reasonResetSucceeded); len(got) != 1 {
		t.Fatalf("success logs = %+v, want one in %+v", got, logs)
	} else {
		for _, field := range []string{"auth_id", "auth_index", "credit_id", "code", "redeem_request_id"} {
			if _, okField := got[0].Fields[field]; !okField {
				t.Errorf("field %q missing from the success log %v", field, got[0].Fields)
			}
		}
	}
	if got := logsWithMessage(logs, reasonCooldownCleared); len(got) != 1 {
		t.Errorf("cooldown logs = %+v, want one in %+v", got, logs)
	}
}

func TestResetCreditSelection(t *testing.T) {
	tests := []struct {
		name       string
		credits    string
		wantCredit string // "" means no credit was available, so nothing is consumed
	}{
		{
			name:       "earliest expires_at wins",
			credits:    `{"id":"later","status":"available","expires_at":"2026-08-01T00:00:00Z"},{"id":"earliest","status":"available","expires_at":"2026-07-01T00:00:00Z"}`,
			wantCredit: "earliest",
		},
		{
			name:       "null expires_at ranks last",
			credits:    `{"id":"forever","status":"available","expires_at":null},{"id":"dated","status":"available","expires_at":"2026-09-01T00:00:00Z"}`,
			wantCredit: "dated",
		},
		{
			name:       "an undated credit is used when it is the only one",
			credits:    `{"id":"forever","status":"available","expires_at":null}`,
			wantCredit: "forever",
		},
		{
			name:       "unparseable expiry ranks last",
			credits:    `{"id":"broken","status":"available","expires_at":"tomorrow"},{"id":"dated","status":"available","expires_at":"2026-09-01T00:00:00Z"}`,
			wantCredit: "dated",
		},
		{
			name:    "non-available credits are skipped",
			credits: `{"id":"redeeming","status":"redeeming","expires_at":"2026-07-01T00:00:00Z"},{"id":"redeemed","status":"redeemed","expires_at":"2026-07-01T00:00:00Z"}`,
		},
		{
			name:    "a credit without an id is skipped",
			credits: `{"status":"available","expires_at":"2026-07-01T00:00:00Z"}`,
		},
		{
			name: "an empty listing consumes nothing",
		},
		{
			name: "the official fixture shape is tolerated",
			credits: `{"id":"credit-1","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-06-17T00:00:00Z","expires_at":"2026-07-17T00:00:00Z","redeem_started_at":null,"redeemed_at":null,"profile_image_url":"https://example.test/avatar.png","profile_user_id":"@friend","title":"Full reset (Weekly + 5 hr)","description":"Ready to redeem"},` +
				`{"id":"credit-2","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-06-18T00:00:00Z","expires_at":null}`,
			wantCredit: "credit-1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := scriptedResetHost(test.credits, http.StatusOK, consumeCodeReset)

			consumed := runReset(fake, resetConfig("secret-key"), usageRecord(hitBody()))
			if consumed != (test.wantCredit != "") {
				t.Fatalf("runReset = %v, want %v", consumed, test.wantCredit != "")
			}

			var consumedCredit string
			for _, request := range fake.requests() {
				if request.Method == http.MethodPost && request.URL == consumeURL {
					var redeem consumeRequest
					if errUnmarshal := json.Unmarshal(request.Body, &redeem); errUnmarshal != nil {
						t.Fatalf("decode consume body %s: %v", request.Body, errUnmarshal)
					}
					consumedCredit = redeem.CreditID
				}
			}
			if consumedCredit != test.wantCredit {
				t.Errorf("consumed credit = %q, want %q", consumedCredit, test.wantCredit)
			}

			wantCalls := 1 // the listing
			if test.wantCredit != "" {
				wantCalls = 3
			}
			if calls := fake.requests(); len(calls) != wantCalls {
				t.Errorf("http.do calls = %d, want %d", len(calls), wantCalls)
			}
			if test.wantCredit == "" {
				if got := logsWithMessage(fake.logged(), reasonNoAvailableCredit); len(got) != 1 {
					t.Errorf("no-credit logs = %+v, want one", got)
				}
			}
		})
	}
}

func TestResetCredentialHeaderFallbacks(t *testing.T) {
	jwt := jwtWithAccountID(t, "acct-from-id-token")

	tests := []struct {
		name           string
		authJSON       string
		wantAccount    string // "" means the header must be absent
		wantAnyRequest bool
	}{
		{
			name:           "access_token and account_id are used as-is",
			authJSON:       `{"access_token":"tok-123","account_id":"acct-1"}`,
			wantAccount:    "acct-1",
			wantAnyRequest: true,
		},
		{
			name:           "chatgpt_account_id is the first fallback",
			authJSON:       `{"access_token":"tok-123","chatgpt_account_id":"acct-2"}`,
			wantAccount:    "acct-2",
			wantAnyRequest: true,
		},
		{
			name:           "id_token claim is the last fallback",
			authJSON:       `{"access_token":"tok-123","id_token":"` + jwt + `"}`,
			wantAccount:    "acct-from-id-token",
			wantAnyRequest: true,
		},
		{
			name:           "an email_ placeholder is never sent",
			authJSON:       `{"access_token":"tok-123","account_id":"email_abc"}`,
			wantAnyRequest: true,
		},
		{
			name:           "a local_ placeholder is never sent",
			authJSON:       `{"access_token":"tok-123","account_id":"local_abc"}`,
			wantAnyRequest: true,
		},
		{
			name:           "a placeholder falls through to the id_token claim",
			authJSON:       `{"access_token":"tok-123","account_id":"email_abc","id_token":"` + jwt + `"}`,
			wantAccount:    "acct-from-id-token",
			wantAnyRequest: true,
		},
		{
			name:           "a missing account id sends no header",
			authJSON:       `{"access_token":"tok-123"}`,
			wantAnyRequest: true,
		},
		{
			name:     "a credential without an access token aborts before any request",
			authJSON: `{"account_id":"acct-1"}`,
		},
		{
			name:     "an unreadable credential body aborts before any request",
			authJSON: `not a json document`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := scriptedResetHost(`{"id":"c","status":"available","expires_at":"2026-07-01T00:00:00Z"}`, http.StatusOK, consumeCodeReset)
			fake.auths["c1f0a9"] = pluginapi.HostAuthGetResponse{JSON: json.RawMessage(test.authJSON)}

			consumed := runReset(fake, resetConfig("secret-key"), usageRecord(hitBody()))
			if consumed != test.wantAnyRequest {
				t.Fatalf("runReset = %v, want %v", consumed, test.wantAnyRequest)
			}
			requests := fake.requests()
			if !test.wantAnyRequest {
				if len(requests) != 0 {
					t.Fatalf("http.do calls = %+v, want none", requests)
				}
				if got := logsWithMessage(fake.logged(), reasonNoAccessToken); len(got) != 1 {
					t.Errorf("no-access-token logs = %+v, want one", got)
				}
				return
			}
			if len(requests) == 0 {
				t.Fatalf("no request was made, want the credit listing")
			}
			assertHeader(t, requests[0], "Authorization", "Bearer tok-123")
			if got := requests[0].Headers.Get("ChatGPT-Account-ID"); got != test.wantAccount {
				t.Errorf("ChatGPT-Account-ID = %q, want %q", got, test.wantAccount)
			}
		})
	}
}

func TestResetConsumeOutcomes(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		body           string
		wantConsumed   bool
		wantCooldown   bool
		wantReasonCode string // logged code on a non-consuming failure
	}{
		{name: "reset consumes and clears the cooldown", status: http.StatusOK, body: `{"code":"reset","credit":{"id":"ignored-by-cli"},"windows_reset":2}`, wantConsumed: true, wantCooldown: true},
		{name: "already_redeemed counts as success", status: http.StatusOK, body: `{"code":"already_redeemed"}`, wantConsumed: true, wantCooldown: true},
		{name: "nothing_to_reset consumes nothing", status: http.StatusOK, body: `{"code":"nothing_to_reset"}`, wantReasonCode: "nothing_to_reset"},
		{name: "no_credit consumes nothing", status: http.StatusOK, body: `{"code":"no_credit"}`, wantReasonCode: "no_credit"},
		{name: "an unknown code is a failure", status: http.StatusOK, body: `{"code":"slow_down"}`, wantReasonCode: "slow_down"},
		{name: "an unparseable body is a failure", status: http.StatusOK, body: `not json`},
		{name: "a 500 is a failure", status: http.StatusInternalServerError, body: `{"error":"boom"}`},
		{name: "a 401 is a failure", status: http.StatusUnauthorized, body: ``},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := scriptedResetHost(`{"id":"credit-1","status":"available","expires_at":"2026-07-01T00:00:00Z"}`, test.status, "")
			fake.routes[1].Body = test.body

			if got := runReset(fake, resetConfig("secret-key"), usageRecord(hitBody())); got != test.wantConsumed {
				t.Fatalf("runReset = %v, want %v", got, test.wantConsumed)
			}

			requests := fake.requests()
			wantCalls := 2 // the listing and the consume
			if test.wantCooldown {
				wantCalls = 3 // plus the cooldown clear
			}
			if len(requests) != wantCalls {
				t.Fatalf("http.do calls = %d, want %d", len(requests), wantCalls)
			}
			var redeem consumeRequest
			if errUnmarshal := json.Unmarshal(requests[1].Body, &redeem); errUnmarshal != nil {
				t.Fatalf("decode consume body %s: %v", requests[1].Body, errUnmarshal)
			}
			if redeem.CreditID != "credit-1" || !uuidV4Pattern.MatchString(redeem.RedeemRequestID) {
				t.Errorf("consume body = %+v, want credit-1 and a UUID v4", redeem)
			}

			logs := fake.logged()
			cleared := logsWithMessage(logs, reasonCooldownCleared)
			if test.wantCooldown && len(cleared) != 1 {
				t.Errorf("cooldown logs = %+v, want one", cleared)
			}
			if !test.wantCooldown {
				if len(cleared) != 0 {
					t.Errorf("cooldown cleared for a failure: %+v", cleared)
				}
				if got := logsWithMessage(logs, reasonConsumeFailed); len(got) != 1 {
					t.Fatalf("consume-failure logs = %+v, want one", got)
				} else if test.wantReasonCode != "" && got[0].Fields["code"] != test.wantReasonCode {
					t.Errorf("logged code = %v, want %q", got[0].Fields["code"], test.wantReasonCode)
				}
			}
		})
	}
}

func TestResetListingFailuresAbortTheFlow(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		transport bool
	}{
		{name: "a non-2xx listing is a failure", status: http.StatusForbidden, body: `{"error":"forbidden"}`},
		{name: "an unparseable listing is a failure", status: http.StatusOK, body: `not json`},
		{name: "a transport error is a failure", transport: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := scriptedResetHost(`{"id":"credit-1","status":"available","expires_at":"2026-07-01T00:00:00Z"}`, http.StatusOK, consumeCodeReset)
			if test.transport {
				fake.httpHandler = func(httpRequest) (pluginapi.HTTPResponse, error) {
					return pluginapi.HTTPResponse{}, errFakeTransport
				}
			} else {
				fake.routes[0].Status, fake.routes[0].Body = test.status, test.body
			}

			if runReset(fake, resetConfig("secret-key"), usageRecord(hitBody())) {
				t.Fatalf("runReset = true, want the flow aborted")
			}
			if calls := fake.requests(); len(calls) != 1 {
				t.Fatalf("http.do calls = %d, want only the listing", len(calls))
			}
			if got := logsWithMessage(fake.logged(), reasonCreditsListFailed); len(got) != 1 {
				t.Errorf("listing-failure logs = %+v, want one", got)
			}
		})
	}
}

func TestResetWithoutACredentialAborts(t *testing.T) {
	fake := newFakeHost()

	if runReset(fake, resetConfig("secret-key"), usageRecord(hitBody())) {
		t.Fatalf("runReset = true, want the flow aborted")
	}
	if calls := fake.requests(); len(calls) != 0 {
		t.Errorf("http.do calls = %+v, want none", calls)
	}
	if got := logsWithMessage(fake.logged(), reasonAuthUnreadable); len(got) != 1 {
		t.Errorf("auth-failure logs = %+v, want one", got)
	}
}

func TestCooldownClearFailureIsLoggedAndNeverRetried(t *testing.T) {
	tests := []struct {
		name      string
		transport bool
	}{
		{name: "a non-200 response"},
		{name: "a transport error", transport: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := scriptedResetHost(`{"id":"credit-1","status":"available","expires_at":"2026-07-01T00:00:00Z"}`, http.StatusOK, consumeCodeReset)
			if test.transport {
				// The listing and the consume succeed; only the cooldown call fails.
				fake.httpHandler = func(request httpRequest) (pluginapi.HTTPResponse, error) {
					switch {
					case request.Method == http.MethodGet:
						return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(creditsBody(`{"id":"credit-1","status":"available","expires_at":"2026-07-01T00:00:00Z"}`))}, nil
					case request.URL == consumeURL:
						return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":"reset"}`)}, nil
					}
					return pluginapi.HTTPResponse{}, errFakeTransport
				}
			} else {
				fake.routes[2].Status = http.StatusInternalServerError
			}

			if !runReset(fake, resetConfig("secret-key"), usageRecord(hitBody())) {
				t.Fatalf("runReset = false, want the consumed credit reported")
			}
			if calls := fake.requests(); len(calls) != 3 {
				t.Fatalf("http.do calls = %d, want exactly one cooldown attempt", len(calls))
			}
			logs := fake.logged()
			if got := logsWithMessage(logs, reasonResetSucceeded); len(got) != 1 {
				t.Errorf("success logs = %+v, want one", got)
			}
			if got := logsWithMessage(logs, reasonCooldownFailed); len(got) != 1 {
				t.Errorf("cooldown-failure logs = %+v, want one in %+v", got, logs)
			}
		})
	}
}

func TestCooldownClearIsSkippedWithoutAManagementKey(t *testing.T) {
	fake := scriptedResetHost(`{"id":"credit-1","status":"available","expires_at":"2026-07-01T00:00:00Z"}`, http.StatusOK, consumeCodeReset)

	if !runReset(fake, resetConfig(""), usageRecord(hitBody())) {
		t.Fatalf("runReset = false, want the consumed credit reported")
	}
	if calls := fake.requests(); len(calls) != 2 {
		t.Fatalf("http.do calls = %d, want the listing and the consume only", len(calls))
	}
	if got := logsWithMessage(fake.logged(), reasonManagementKeyEmpty); len(got) != 1 {
		t.Errorf("management-key logs = %+v, want one", got)
	}
}

func TestIdempotencyKeyIsGeneratedPerFlow(t *testing.T) {
	keys := make([]string, 0, 2)
	for range 2 {
		fake := scriptedResetHost(`{"id":"credit-1","status":"available","expires_at":"2026-07-01T00:00:00Z"}`, http.StatusOK, consumeCodeReset)
		if !runReset(fake, resetConfig("secret-key"), usageRecord(hitBody())) {
			t.Fatalf("runReset = false, want a consumed credit")
		}
		var redeem consumeRequest
		if errUnmarshal := json.Unmarshal(fake.requests()[1].Body, &redeem); errUnmarshal != nil {
			t.Fatalf("decode consume body: %v", errUnmarshal)
		}
		if !uuidV4Pattern.MatchString(redeem.RedeemRequestID) {
			t.Fatalf("redeem_request_id = %q, want a UUID v4", redeem.RedeemRequestID)
		}
		keys = append(keys, redeem.RedeemRequestID)
	}
	if keys[0] == keys[1] {
		t.Errorf("both flows used the idempotency key %q, want a fresh key per flow", keys[0])
	}
}

// TestUsageHandleDispatchesOnceAndReturnsImmediately drives the whole path from
// usage.handle: the reset flow must run off the usage goroutine (D11), and a second
// signal for the same credential while it is in flight must be dropped (D12).
func TestUsageHandleDispatchesOnceAndReturnsImmediately(t *testing.T) {
	fake := newFakeHost()
	fake.auths["c1f0a9"] = pluginapi.HostAuthGetResponse{JSON: json.RawMessage(`{"access_token":"tok-123","account_id":"acct-123"}`)}
	released := make(chan struct{})
	fake.httpHandler = func(request httpRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case request.Method == http.MethodGet:
			<-released
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(creditsBody(`{"id":"credit-1","status":"available","expires_at":"2026-07-01T00:00:00Z"}`))}, nil
		case request.URL == consumeURL:
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"code":"reset"}`)}, nil
		default:
			return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"status":"ok"}`)}, nil
		}
	}

	activeReset = newResetState()
	t.Cleanup(func() { activeReset = newResetState() })
	if _, errRegister := handleMethod(fake, pluginabi.MethodPluginRegister, lifecyclePayload(t, "enabled: true\nmanagement_key: secret-key\n")); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	fake.logs = nil
	record := marshalRecord(t, usageRecord(hitBody()))

	returned := make(chan []byte, 1)
	go func() {
		raw, _ := handleMethod(fake, pluginabi.MethodUsageHandle, record)
		returned <- raw
	}()
	select {
	case raw := <-returned:
		if result := string(decodeResult(t, raw)); result != "{}" {
			t.Errorf("usage.handle result = %s, want {}", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("usage.handle blocked while the reset flow was running")
	}

	// The flow is parked in the credit listing; the second signal must be dropped.
	waitFor(t, func() bool { return len(fake.authGetCalls()) == 1 })
	if _, errHandle := handleMethod(fake, pluginabi.MethodUsageHandle, record); errHandle != nil {
		t.Fatalf("second usage.handle: %v", errHandle)
	}
	if got := len(fake.authGetCalls()); got != 1 {
		t.Fatalf("auth.get calls = %d, want 1: a concurrent signal must not start a second flow", got)
	}
	if got := logsWithMessage(fake.logged(), reasonResetDropped); len(got) != 1 {
		t.Errorf("dropped-signal logs = %+v, want one", got)
	}

	close(released)
	waitFor(t, func() bool { return len(fake.requests()) == 3 })
	if got := len(fake.authGetCalls()); got != 1 {
		t.Errorf("auth.get calls = %d, want 1", got)
	}
	want := []string{
		"http.do GET " + creditsURL,
		"http.do POST " + consumeURL,
		"http.do POST " + quotaURL,
	}
	var trace []string
	for _, entry := range fake.callTrace() {
		if strings.HasPrefix(entry, "http.do") {
			trace = append(trace, entry)
		}
	}
	if !reflect.DeepEqual(trace, want) {
		t.Errorf("http call trace = %v, want %v", trace, want)
	}
}
