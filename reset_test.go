package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// errFakeTransport stands in for a host.http.do transport failure.
var errFakeTransport = errors.New("fake transport failure")

const (
	authIndex   = "c1f0a9"
	authFile    = "codex-a@example.com.json"
	accessToken = "tok-123"
	accountID   = "acct-123"

	creditsURL = codexBaseURL + resetCreditsPath
	consumeURL = codexBaseURL + resetConsumePath
	quotaURL   = defaultManagementBaseURL + managementResetQuotaPath

	managementConfigYAML = "enabled: true\nmanagement_key: secret-key\ninclude_credentials:\n  - " + authFile + "\n"
	// includedConfigYAML enables the plugin and includes the hit credential, but carries
	// no management key: a hit reaches the pre-flight and stops there. Cases that only
	// need the classification gate use this; reset-flow cases use managementConfigYAML.
	includedConfigYAML = "enabled: true\ninclude_credentials:\n  - " + authFile + "\n"
)

// aUsableCredit is the credit listing most cases start from: it expires well after
// testClock, so it is never skipped as expired.
const aUsableCredit = `{"id":"credit-1","status":"available","expires_at":"2026-07-17T00:00:00Z"}`

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func assertHeader(t *testing.T, request httpRequest, name, want string) {
	t.Helper()
	if got := request.Headers.Get(name); got != want {
		t.Errorf("%s header = %q, want %q", name, got, want)
	}
}

// creditsBody renders an upstream credits listing; unknown top-level fields are part
// of the real payload and must be ignored.
func creditsBody(credits string) string {
	return `{"credits":[` + credits + `],"available_count":9,"total_earned_count":9}`
}

// scriptedResetHost is a fake host wired for one credential and a flow that can
// succeed: an available credit, a consume answer and a cooldown-clear answer.
func scriptedResetHost(credits string, consumeStatus int, consumeCode string) *fakeHost {
	return newFakeHost().
		withCredential(authIndex, `{"access_token":"`+accessToken+`","account_id":"`+accountID+`"}`).
		script(http.MethodGet, creditsURL, http.StatusOK, creditsBody(credits)).
		script(http.MethodPost, consumeURL, consumeStatus, `{"code":"`+consumeCode+`"}`).
		script(http.MethodPost, quotaURL, http.StatusOK, `{"status":"ok"}`)
}

// driveHit registers the configuration, sends one exhaustioned signal, and waits for
// the log line that ends the flow. The flow runs on its own goroutine (D11), so this
// is what makes the assertions after it deterministic.
func driveHit(t *testing.T, fake *fakeHost, configYAML, lastLog string, record pluginapi.UsageRecord) {
	t.Helper()
	registerConfig(t, fake, configYAML)
	sendUsage(t, fake, record)
	waitForLog(t, fake, lastLog)
}

// consumedCredit decodes the single consume request a flow sent and returns its body.
func consumedCredit(t *testing.T, fake *fakeHost) consumeRequest {
	t.Helper()
	requests := fake.requestsFor(http.MethodPost, consumeURL)
	if len(requests) != 1 {
		t.Fatalf("consume requests = %d, want exactly one", len(requests))
	}
	var redeem consumeRequest
	if errUnmarshal := json.Unmarshal(requests[0].Body, &redeem); errUnmarshal != nil {
		t.Fatalf("decode consume body %s: %v", requests[0].Body, errUnmarshal)
	}
	if !uuidV4Pattern.MatchString(redeem.RedeemRequestID) {
		t.Errorf("redeem_request_id = %q, want a UUID v4", redeem.RedeemRequestID)
	}
	return redeem
}

// waitForSuppressed probes the credential until the debounce reports the suppression
// window (spec story 14). Probes are dropped without any host call, and a drop
// reported as in-flight means the flow has not released its slot yet — so this is also
// how a test observes that a credited flow finished.
func waitForSuppressed(t *testing.T, fake *fakeHost, record pluginapi.UsageRecord) {
	t.Helper()
	waitFor(t, func() bool {
		if len(logsWithMessage(fake.logged(), reasonResetSuppressed)) > 0 {
			return true
		}
		sendUsage(t, fake, record)
		return false
	})
}

// jwtWithAccountID builds an unsigned id_token carrying the account id claim. The claim
// key is spelled here rather than shared with production: the fixture must fail if the
// namespace production reads ever drifts away from this one.
func jwtWithAccountID(t *testing.T, accountID string) string {
	t.Helper()
	payload, errMarshal := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": accountID},
	})
	if errMarshal != nil {
		t.Fatalf("marshal id_token claims: %v", errMarshal)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestResetFlowConsumesEarliestCreditAndClearsCooldown(t *testing.T) {
	useFakeClock(t, testClock)
	fake := scriptedResetHost(`
		{"id":"later","status":"available","expires_at":"2026-08-01T00:00:00Z"},
		{"id":"redeemed","status":"redeemed","expires_at":"2026-07-02T00:00:00Z"},
		{"id":"earliest","status":"available","expires_at":"2026-07-05T00:00:00Z"},
		{"id":"forever","status":"available","expires_at":null}`, http.StatusOK, consumeCodeReset)

	driveHit(t, fake, managementConfigYAML, reasonCooldownCleared, hitRecord())

	// The whole flow, in order: read the credential, list the credits, consume one,
	// clear CPA's cooldown.
	want := []string{
		"auth.get " + authIndex,
		"http.do GET " + creditsURL,
		"http.do POST " + consumeURL,
		"http.do POST " + quotaURL,
	}
	if trace := fake.callTrace(); !reflect.DeepEqual(trace, want) {
		t.Fatalf("call trace = %v, want %v", trace, want)
	}

	listing := fake.requestsFor(http.MethodGet, creditsURL)
	if len(listing) != 1 {
		t.Fatalf("listing requests = %d, want one", len(listing))
	}
	assertHeader(t, listing[0], "Authorization", "Bearer "+accessToken)
	assertHeader(t, listing[0], "ChatGPT-Account-ID", accountID)
	assertHeader(t, listing[0], "User-Agent", codexUserAgent)
	assertHeader(t, listing[0], "originator", codexOriginator)
	if contentType := listing[0].Headers.Get("Content-Type"); contentType != "" {
		t.Errorf("GET Content-Type = %q, want none", contentType)
	}

	redeem := consumedCredit(t, fake)
	if redeem.CreditID != "earliest" {
		t.Errorf("consume credit_id = %q, want the earliest-expiring available credit", redeem.CreditID)
	}

	quota := fake.requestsFor(http.MethodPost, quotaURL)
	if len(quota) != 1 {
		t.Fatalf("cooldown-clear requests = %d, want one", len(quota))
	}
	assertHeader(t, quota[0], "Authorization", "Bearer secret-key")
	assertHeader(t, quota[0], "Content-Type", "application/json")
	var resetQuota managementResetQuotaRequest
	if errUnmarshal := json.Unmarshal(quota[0].Body, &resetQuota); errUnmarshal != nil {
		t.Fatalf("decode reset-quota body %s: %v", quota[0].Body, errUnmarshal)
	}
	if resetQuota.AuthIndex != authIndex {
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

// TestResetCreditSelection pins which credit the flow spends. Expired and unusable
// entries must not cost the flow its only chance: a consume that comes back no_credit
// abandons the reset while a usable credit sits next to it.
func TestResetCreditSelection(t *testing.T) {
	tests := []struct {
		name       string
		credits    string
		wantCredit string // "" means no credit was available, so nothing is consumed
	}{
		{
			name:       "earliest expires_at wins",
			credits:    `{"id":"later","status":"available","expires_at":"2026-08-01T00:00:00Z"},{"id":"earliest","status":"available","expires_at":"2026-07-05T00:00:00Z"}`,
			wantCredit: "earliest",
		},
		{
			name:       "an expired credit is skipped",
			credits:    `{"id":"expired","status":"available","expires_at":"2026-06-01T00:00:00Z"},{"id":"usable","status":"available","expires_at":"2026-07-05T00:00:00Z"}`,
			wantCredit: "usable",
		},
		{
			name:    "only expired credits consume nothing",
			credits: `{"id":"expired-soonest","status":"available","expires_at":"2026-06-30T00:00:00Z"},{"id":"expired-later","status":"available","expires_at":"2026-07-01T11:59:59Z"}`,
		},
		{
			name:       "a credit expiring exactly now is skipped",
			credits:    `{"id":"boundary","status":"available","expires_at":"2026-07-01T12:00:00Z"},{"id":"usable","status":"available","expires_at":"2026-07-05T00:00:00Z"}`,
			wantCredit: "usable",
		},
		{
			name:       "an expired credit never wins over an undated one",
			credits:    `{"id":"expired","status":"available","expires_at":"2026-06-01T00:00:00Z"},{"id":"forever","status":"available","expires_at":null}`,
			wantCredit: "forever",
		},
		{
			name:       "null expires_at ranks last",
			credits:    `{"id":"forever","status":"available","expires_at":null},{"id":"dated","status":"available","expires_at":"2026-07-05T00:00:00Z"}`,
			wantCredit: "dated",
		},
		{
			name:       "an undated credit is used when it is the only one",
			credits:    `{"id":"forever","status":"available","expires_at":null}`,
			wantCredit: "forever",
		},
		{
			name:       "unparsable expiry ranks last",
			credits:    `{"id":"broken","status":"available","expires_at":"tomorrow"},{"id":"dated","status":"available","expires_at":"2026-07-05T00:00:00Z"}`,
			wantCredit: "dated",
		},
		{
			name:    "non-available credits are skipped",
			credits: `{"id":"redeeming","status":"redeeming","expires_at":"2026-07-05T00:00:00Z"},{"id":"redeemed","status":"redeemed","expires_at":"2026-07-05T00:00:00Z"}`,
		},
		{
			name:    "a credit without an id is skipped",
			credits: `{"status":"available","expires_at":"2026-07-05T00:00:00Z"}`,
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
			useFakeClock(t, testClock)
			fake := scriptedResetHost(test.credits, http.StatusOK, consumeCodeReset)

			lastLog := reasonNoAvailableCredit
			if test.wantCredit != "" {
				lastLog = reasonCooldownCleared
			}
			driveHit(t, fake, managementConfigYAML, lastLog, hitRecord())

			wantCalls := 1 // the listing
			if test.wantCredit != "" {
				wantCalls = 3 // plus the consume and the cooldown clear
			}
			if got := fake.requestCount(); got != wantCalls {
				t.Fatalf("http.do calls = %d, want %d", got, wantCalls)
			}
			if test.wantCredit == "" {
				if got := logsWithMessage(fake.logged(), reasonNoAvailableCredit); len(got) != 1 {
					t.Errorf("no-credit logs = %+v, want one", got)
				}
				return
			}
			if got := consumedCredit(t, fake).CreditID; got != test.wantCredit {
				t.Errorf("consumed credit = %q, want %q", got, test.wantCredit)
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
			useFakeClock(t, testClock)
			fake := scriptedResetHost(aUsableCredit, http.StatusOK, consumeCodeReset)
			fake.withCredential(authIndex, test.authJSON)

			lastLog := reasonNoAccessToken
			if test.wantAnyRequest {
				lastLog = reasonCooldownCleared
			}
			driveHit(t, fake, managementConfigYAML, lastLog, hitRecord())

			listing := fake.requestsFor(http.MethodGet, creditsURL)
			if !test.wantAnyRequest {
				if got := fake.requestCount(); got != 0 {
					t.Fatalf("http.do calls = %d, want none", got)
				}
				if got := logsWithMessage(fake.logged(), reasonNoAccessToken); len(got) != 1 {
					t.Errorf("no-access-token logs = %+v, want one", got)
				}
				return
			}
			if len(listing) != 1 {
				t.Fatalf("listing requests = %d, want one", len(listing))
			}
			assertHeader(t, listing[0], "Authorization", "Bearer "+accessToken)
			if got := listing[0].Headers.Get("ChatGPT-Account-ID"); got != test.wantAccount {
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
		{name: "an unparsable body is a failure", status: http.StatusOK, body: `not json`},
		{name: "a 500 is a failure", status: http.StatusInternalServerError, body: `{"error":"boom"}`},
		{name: "a 401 is a failure", status: http.StatusUnauthorized, body: ``},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			useFakeClock(t, testClock)
			fake := scriptedResetHost(aUsableCredit, test.status, "")
			fake.script(http.MethodPost, consumeURL, test.status, test.body)

			lastLog := reasonConsumeFailed
			if test.wantCooldown {
				lastLog = reasonCooldownCleared
			}
			driveHit(t, fake, managementConfigYAML, lastLog, hitRecord())

			wantCalls := 2 // the listing and the consume
			if test.wantCooldown {
				wantCalls = 3 // plus the cooldown clear
			}
			if got := fake.requestCount(); got != wantCalls {
				t.Fatalf("http.do calls = %d, want %d", got, wantCalls)
			}
			redeem := consumedCredit(t, fake)
			if redeem.CreditID != "credit-1" {
				t.Errorf("consume credit_id = %q, want credit-1", redeem.CreditID)
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

// TestResetListingFailures pins how a failed credit listing ends the flow: an expired
// or rejected token (401/403) is final, anything else is retried inside the flow a
// bounded number of times before giving up.
func TestResetListingFailures(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		transport bool
		wantCalls int
	}{
		{name: "a 401 is final", status: http.StatusUnauthorized, wantCalls: 1},
		{name: "a 403 is final", status: http.StatusForbidden, body: `{"error":"forbidden"}`, wantCalls: 1},
		{name: "a 500 is retried", status: http.StatusInternalServerError, wantCalls: creditsListAttempts},
		{name: "an unparsable listing is retried", status: http.StatusOK, body: `not json`, wantCalls: creditsListAttempts},
		{name: "a transport error is retried", transport: true, wantCalls: creditsListAttempts},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			useFakeClock(t, testClock)
			fake := scriptedResetHost(aUsableCredit, http.StatusOK, consumeCodeReset)
			if test.transport {
				fake.scriptFailure(http.MethodGet, creditsURL, errFakeTransport)
			} else {
				fake.script(http.MethodGet, creditsURL, test.status, test.body)
			}

			driveHit(t, fake, managementConfigYAML, reasonCreditsListFailed, hitRecord())

			if got := fake.requestCount(); got != test.wantCalls {
				t.Fatalf("http.do calls = %d, want %d listing attempts and nothing else", got, test.wantCalls)
			}
			if got := logsWithMessage(fake.logged(), reasonCreditsListFailed); len(got) != 1 {
				t.Errorf("listing-failure logs = %+v, want one", got)
			}
		})
	}
}

func TestResetListingRecoversOnRetry(t *testing.T) {
	useFakeClock(t, testClock)
	fake := scriptedResetHost(aUsableCredit, http.StatusOK, consumeCodeReset)
	var listings int
	fake.httpHandler = func(request httpRequest) (pluginapi.HTTPResponse, error) {
		if request.URL == creditsURL {
			fake.mu.Lock()
			listings++
			first := listings == 1
			fake.mu.Unlock()
			if first {
				return pluginapi.HTTPResponse{}, errFakeTransport
			}
		}
		return fake.answer(request)
	}

	driveHit(t, fake, managementConfigYAML, reasonCooldownCleared, hitRecord())

	if got := len(fake.requestsFor(http.MethodGet, creditsURL)); got != 2 {
		t.Errorf("listing requests = %d, want the failed one and one retry", got)
	}
	consumedCredit(t, fake)
}

// TestFlowOutcomeDecidesSuppression pins which finished flows silence the credential
// for the suppression window. A flow whose outcome a quick repeat cannot change (a
// credit spent, none to spend, a dead token, a consume already sent) is suppressed, so
// the stale 429 records that keep arriving do not repeat the upstream calls. A flow
// that stopped on a transient fault stays eligible for the next signal.
func TestFlowOutcomeDecidesSuppression(t *testing.T) {
	tests := []struct {
		name           string
		arrange        func(*fakeHost)
		lastLog        string
		wantSuppressed bool
	}{
		{
			name:           "no available credit",
			arrange:        func(f *fakeHost) { f.script(http.MethodGet, creditsURL, http.StatusOK, creditsBody("")) },
			lastLog:        reasonNoAvailableCredit,
			wantSuppressed: true,
		},
		{
			name:           "consume answers no_credit",
			arrange:        func(f *fakeHost) { f.script(http.MethodPost, consumeURL, http.StatusOK, `{"code":"no_credit"}`) },
			lastLog:        reasonConsumeFailed,
			wantSuppressed: true,
		},
		{
			name:           "consume outcome unknown",
			arrange:        func(f *fakeHost) { f.scriptFailure(http.MethodPost, consumeURL, errFakeTransport) },
			lastLog:        reasonConsumeFailed,
			wantSuppressed: true,
		},
		{
			name:           "credential has no access token",
			arrange:        func(f *fakeHost) { f.withCredential(authIndex, `{"account_id":"acct-1"}`) },
			lastLog:        reasonNoAccessToken,
			wantSuppressed: true,
		},
		{
			name:           "token rejected by the listing",
			arrange:        func(f *fakeHost) { f.script(http.MethodGet, creditsURL, http.StatusUnauthorized, "") },
			lastLog:        reasonCreditsListFailed,
			wantSuppressed: true,
		},
		{
			name:    "listing keeps failing",
			arrange: func(f *fakeHost) { f.scriptFailure(http.MethodGet, creditsURL, errFakeTransport) },
			lastLog: reasonCreditsListFailed,
		},
		{
			name:    "credential cannot be read",
			arrange: func(f *fakeHost) { f.auths = map[string]pluginapi.HostAuthGetResponse{} },
			lastLog: reasonAuthUnreadable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			useFakeClock(t, testClock)
			fake := scriptedResetHost(aUsableCredit, http.StatusOK, consumeCodeReset)
			test.arrange(fake)
			driveHit(t, fake, managementConfigYAML, test.lastLog, hitRecord())

			if test.wantSuppressed {
				waitForSuppressed(t, fake, hitRecord())
				if got := len(fake.authGetCalls()); got != 1 {
					t.Errorf("auth.get calls = %d, want the first flow only", got)
				}
				return
			}
			// Keep signalling until a second flow starts; a suppressed credential never would.
			waitFor(t, func() bool {
				if len(fake.authGetCalls()) >= 2 {
					return true
				}
				sendUsage(t, fake, hitRecord())
				return false
			})
			if got := logsWithMessage(fake.logged(), reasonResetSuppressed); len(got) != 0 {
				t.Errorf("suppression drops = %+v, want none after a transient failure", got)
			}
		})
	}
}

// TestBlankAuthIndexNeverStartsAFlow pins that a record without an auth index, which
// host.auth.get cannot resolve and the debounce cannot key, is dropped before any call.
func TestBlankAuthIndexNeverStartsAFlow(t *testing.T) {
	useFakeClock(t, testClock)
	fake := scriptedResetHost(aUsableCredit, http.StatusOK, consumeCodeReset)
	record := hitRecord()
	record.AuthIndex = ""

	driveHit(t, fake, managementConfigYAML, reasonNoAuthIndex, record)

	if got := len(fake.authGetCalls()); got != 0 {
		t.Errorf("auth.get calls = %d, want none", got)
	}
	if got := fake.requestCount(); got != 0 {
		t.Errorf("http.do calls = %d, want none", got)
	}
}

func TestResetWithoutACredentialAborts(t *testing.T) {
	useFakeClock(t, testClock)
	fake := newFakeHost().script(http.MethodGet, creditsURL, http.StatusOK, creditsBody(aUsableCredit))

	driveHit(t, fake, managementConfigYAML, reasonAuthUnreadable, hitRecord())

	if got := fake.requestCount(); got != 0 {
		t.Errorf("http.do calls = %d, want none", got)
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
			useFakeClock(t, testClock)
			fake := scriptedResetHost(aUsableCredit, http.StatusOK, consumeCodeReset)
			if test.transport {
				fake.scriptFailure(http.MethodPost, quotaURL, errFakeTransport)
			} else {
				fake.script(http.MethodPost, quotaURL, http.StatusInternalServerError, `{"error":"boom"}`)
			}

			driveHit(t, fake, managementConfigYAML, reasonCooldownFailed, hitRecord())

			if got := fake.requestCount(); got != 3 {
				t.Fatalf("http.do calls = %d, want exactly one cooldown attempt", got)
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

// TestMissingManagementKeyConsumesNothing pins the fail-safe: without a management key
// the cooldown can never be cleared, so spending a credit would leave the credential
// locked until the old reset time and burn the credit for nothing — worse than the
// plugin being absent (spec story 16). The flow must stop before touching upstream.
func TestMissingManagementKeyConsumesNothing(t *testing.T) {
	useFakeClock(t, testClock)
	fake := scriptedResetHost(aUsableCredit, http.StatusOK, consumeCodeReset)

	driveHit(t, fake, includedConfigYAML, reasonManagementKeyEmpty, hitRecord())

	if got := fake.requestCount(); got != 0 {
		t.Fatalf("http.do calls = %v, want none: the credit must not be spent", fake.callTrace())
	}
	if got := len(fake.authGetCalls()); got != 0 {
		t.Errorf("auth.get calls = %d, want none", got)
	}
	if got := logsWithMessage(fake.logged(), reasonManagementKeyEmpty); len(got) != 1 {
		t.Errorf("management-key logs = %+v, want one", got)
	}
}

// TestIdempotencyKeyIsGeneratedPerFlow pins D18: the key is generated once per flow,
// not once per process and not inside a retry loop, so a replayed consume can never
// burn a second credit.
func TestIdempotencyKeyIsGeneratedPerFlow(t *testing.T) {
	useFakeClock(t, testClock)

	keys := make([]string, 0, 2)
	for _, index := range []string{authIndex, "b2e1c8"} {
		fake := scriptedResetHost(aUsableCredit, http.StatusOK, consumeCodeReset)
		registerConfig(t, fake, managementConfigYAML)
		fake.withCredential(index, `{"access_token":"`+accessToken+`","account_id":"`+accountID+`"}`)
		record := hitRecord()
		record.AuthIndex = index
		sendUsage(t, fake, record)
		waitForLog(t, fake, reasonCooldownCleared)
		keys = append(keys, consumedCredit(t, fake).RedeemRequestID)
	}
	if keys[0] == keys[1] {
		t.Errorf("both flows used the idempotency key %q, want a fresh key per flow", keys[0])
	}
}

// TestUsageHandleDispatchesOnceAndReturnsImmediately drives the whole path from
// usage.handle: the reset flow must run off the usage goroutine (D11), and a second
// signal for the same credential while it is in flight must be dropped (D12).
func TestUsageHandleDispatchesOnceAndReturnsImmediately(t *testing.T) {
	useFakeClock(t, testClock)
	fake := scriptedResetHost(aUsableCredit, http.StatusOK, consumeCodeReset)
	released := make(chan struct{})
	// Park the flow inside the credit listing so every later signal arrives while the
	// first one is still running.
	fake.httpHandler = func(request httpRequest) (pluginapi.HTTPResponse, error) {
		if request.URL == creditsURL {
			<-released
		}
		return fake.answer(request)
	}
	registerConfig(t, fake, managementConfigYAML)

	record := hitRecord()
	payload := marshalRecord(t, record)
	returned := make(chan []byte, 1)
	go func() {
		raw, _ := handleMethod(fake, pluginabi.MethodUsageHandle, payload)
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
	sendUsage(t, fake, record)
	if got := len(fake.authGetCalls()); got != 1 {
		t.Fatalf("auth.get calls = %d, want 1: a concurrent signal must not start a second flow", got)
	}
	dropped := logsWithMessage(fake.logged(), reasonResetInFlight)
	if len(dropped) != 1 {
		t.Fatalf("in-flight drops = %+v, want one", dropped)
	}
	if dropped[0].Level != "debug" {
		t.Errorf("drop log level = %q, want debug", dropped[0].Level)
	}

	close(released)
	waitFor(t, func() bool { return fake.requestCount() == 3 })
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

// TestResetSuppressionBlocksRetriggerWithinFiveMinutes is the acceptance test for spec
// story 14: five minutes of silence after a credited reset, and eligibility again once
// the window has passed.
func TestResetSuppressionBlocksRetriggerWithinFiveMinutes(t *testing.T) {
	clock := useFakeClock(t, testClock)
	fake := scriptedResetHost(aUsableCredit, http.StatusOK, consumeCodeReset)
	registerConfig(t, fake, managementConfigYAML)

	sendUsage(t, fake, hitRecord())
	waitForLog(t, fake, reasonCooldownCleared)
	// Probe until the flow has released its slot and the suppression window applies.
	waitForSuppressed(t, fake, hitRecord())
	afterFirstFlow := fake.requestCount()
	if afterFirstFlow != 3 {
		t.Fatalf("http.do calls after the first flow = %d, want 3", afterFirstFlow)
	}

	clock.advance(time.Minute)
	sendUsage(t, fake, hitRecord())
	sendUsage(t, fake, hitRecord())
	if got := fake.requestCount(); got != afterFirstFlow {
		t.Errorf("http.do calls inside the window = %d, want %d", got, afterFirstFlow)
	}
	if got := len(fake.authGetCalls()); got != 1 {
		t.Errorf("auth.get calls inside the window = %d, want 1", got)
	}
	if got := logsWithMessage(fake.logged(), reasonResetSuppressed); len(got) < 2 {
		t.Errorf("suppression drops = %d, want the signals inside the window dropped", len(got))
	}

	// Past the window the credential is eligible again and spends a second credit.
	clock.advance(resetSuppressWindow)
	sendUsage(t, fake, hitRecord())
	waitFor(t, func() bool { return fake.requestCount() == 6 })
	if got := logsWithMessage(fake.logged(), reasonCooldownCleared); len(got) != 2 {
		t.Errorf("cooldown-clear logs = %d, want a second flow after the window", len(got))
	}
}

// TestConcurrentSignalsStartOneFlow pins spec story 13: many simultaneous signals for
// one credential produce exactly one flow.
func TestConcurrentSignalsStartOneFlow(t *testing.T) {
	useFakeClock(t, testClock)
	fake := scriptedResetHost(aUsableCredit, http.StatusOK, consumeCodeReset)
	released := make(chan struct{})
	fake.httpHandler = func(request httpRequest) (pluginapi.HTTPResponse, error) {
		if request.URL == creditsURL {
			<-released
		}
		return fake.answer(request)
	}
	registerConfig(t, fake, managementConfigYAML)

	const signals = 64
	payload := marshalRecord(t, hitRecord())
	var wait sync.WaitGroup
	for range signals {
		wait.Add(1)
		go func() {
			defer wait.Done()
			// Assertions stay on the test goroutine; this only drives the RPC.
			_, _ = handleMethod(fake, pluginabi.MethodUsageHandle, payload)
		}()
	}
	wait.Wait()

	// Every caller but the winner is dropped synchronously, so this count is exact.
	if got := len(logsWithMessage(fake.logged(), reasonResetInFlight)); got != signals-1 {
		t.Errorf("in-flight drops = %d, want %d", got, signals-1)
	}
	close(released)
	waitFor(t, func() bool { return fake.requestCount() == 3 })
	if got := len(fake.authGetCalls()); got != 1 {
		t.Errorf("auth.get calls = %d, want exactly one flow", got)
	}
}
