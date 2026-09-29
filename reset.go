package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Upstream and management endpoints (D14, D16, D17, D19).
const (
	codexBaseURL             = "https://chatgpt.com/backend-api"
	resetCreditsPath         = "/wham/rate-limit-reset-credits"
	resetConsumePath         = "/wham/rate-limit-reset-credits/consume"
	managementResetQuotaPath = "/v0/management/reset-quota"

	codexUserAgent  = "codex_cli_rs/0.156.1"
	codexOriginator = "codex_cli_rs"

	creditStatusAvailable = "available"

	// creditsListAttempts bounds the in-flow retries of the credit listing, the one
	// read-only call a transient fault should not cost the flow.
	creditsListAttempts = 3

	// Consume result codes. The upstream enum is closed; anything else is treated as
	// a failure that consumed nothing (D17).
	consumeCodeReset           = "reset"
	consumeCodeAlreadyRedeemed = "already_redeemed"

	// Account id placeholders that must never be sent as a header (D14).
	placeholderAccountPrefixEmail = "email_"
	placeholderAccountPrefixLocal = "local_"
)

// Reset-flow log messages, one per outcome, so credit consumption stays auditable
// after the fact (spec story 17).
const (
	reasonAuthUnreadable     = "auto-reset: cannot read credential: no reset"
	reasonNoAccessToken      = "auto-reset: credential carries no access token: no reset"
	reasonUUIDFailed         = "auto-reset: cannot generate an idempotency key: no reset"
	reasonCreditsListFailed  = "auto-reset: cannot list reset credits: no reset"
	reasonNoAvailableCredit  = "auto-reset: no available reset credit: no reset"
	reasonConsumeFailed      = "auto-reset: reset credit consume failed: no reset"
	reasonResetSucceeded     = "auto-reset: reset credit consumed"
	reasonCooldownCleared    = "auto-reset: credential cooldown cleared"
	reasonCooldownFailed     = "auto-reset: credential cooldown clear failed"
	reasonManagementKeyEmpty = "auto-reset: management_key is not configured: no reset"
	reasonNoAuthIndex        = "auto-reset: record carries no auth index: no reset"
)

// resetCreditsResponse is the GET payload. Unknown fields (the real response carries
// more than the vendor's own struct declares) are ignored, and a null expires_at
// simply leaves the field empty (D16).
type resetCreditsResponse struct {
	Credits []resetCredit `json:"credits"`
}

type resetCredit struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expires_at"`
}

type consumeRequest struct {
	RedeemRequestID string `json:"redeem_request_id"`
	CreditID        string `json:"credit_id"`
}

type consumeResponse struct {
	Code string `json:"code"`
}

type managementResetQuotaRequest struct {
	AuthIndex string `json:"auth_index"`
}

// creditsListRetryDelay is the pause before each listing retry. ponytail: fixed
// delay, add backoff if upstream starts rate-limiting the retries. Tests zero it.
var creditsListRetryDelay = time.Second

// resetFlow is one run of the auto-reset flow for one usage record: the host it talks
// through, the configuration it started with, and the record that triggered it.
type resetFlow struct {
	h      host
	cfg    pluginConfig
	record pluginapi.UsageRecord
}

// log writes one flow line carrying the credential identity (spec story 17).
func (f resetFlow) log(level, reason string, extra map[string]any) {
	f.h.log(level, reason, recordFields(f.record, extra))
}

// startAutoReset dispatches the reset flow for one hit signal and returns immediately
// (D11); a signal arriving while a flow is running or inside the suppression window
// is dropped with a debug log rather than queued (D12).
func startAutoReset(guard *debounce, h host, cfg pluginConfig, record pluginapi.UsageRecord) {
	flow := resetFlow{h: h, cfg: cfg, record: record}
	// Pre-flight: without a management key the cooldown can never be cleared, so a
	// consumed credit would leave the credential locked until the old reset time and
	// buy nothing. Stop before spending anything (spec story 16).
	if strings.TrimSpace(cfg.ManagementKey) == "" {
		flow.log(levelWarn, reasonManagementKeyEmpty, nil)
		return
	}
	// host.auth.get only resolves an auth index, and the debounce is keyed by it: a
	// record without one can never be reset and must not share a debounce entry.
	if record.AuthIndex == "" {
		flow.log(levelWarn, reasonNoAuthIndex, nil)
		return
	}
	if claimed, reason := guard.begin(record.AuthIndex); !claimed {
		flow.log(levelDebug, reason, nil)
		return
	}
	// The goroutine captures this guard, so a later reconfigure or test cannot redirect
	// the flow's bookkeeping at another debounce.
	go func() {
		guard.finish(record.AuthIndex, flow.run())
	}()
}

// run performs the whole flow: read the credential, list the account's reset credits,
// consume the earliest-expiring available one, then clear CPA's cooldown. Every
// failure stops the flow without compensating, leaving CPA's default cooldown in place
// (spec stories 6-16).
//
// It reports whether the flow settled: whether its outcome is one a repeat within the
// suppression window could not change. A spent credit, no credit to spend, a dead
// token and a consume already sent are settled, so the stale 429 records still
// arriving for this credential do not repeat the upstream calls. A transient fault
// (the host, the listing after its retries) is not, and the next signal tries again.
func (f resetFlow) run() bool {
	auth, errAuth := f.h.authGet(f.record.AuthIndex)
	if errAuth != nil {
		f.log(levelWarn, reasonAuthUnreadable, map[string]any{"error": errAuth.Error()})
		return false
	}
	creds := parseAuthCredentials(auth.JSON)
	if creds.accessToken == "" {
		f.log(levelWarn, reasonNoAccessToken, nil)
		return true
	}

	credits, okList, tokenRejected := f.listCredits(creds)
	if !okList {
		return tokenRejected
	}
	credit, okCredit := pickCredit(credits)
	if !okCredit {
		f.log(levelInfo, reasonNoAvailableCredit, map[string]any{"credits": len(credits)})
		return true
	}

	// One idempotency key per flow, generated once and outside any retry, so a
	// replayed consume can never burn a second credit (D18, spec story 8).
	redeemID, errUUID := newUUIDv4()
	if errUUID != nil {
		f.log(levelWarn, reasonUUIDFailed, map[string]any{"error": errUUID.Error()})
		return false
	}

	// Once a consume is sent the credit may be gone whatever came back, so every
	// consume outcome settles the flow: a second flow could spend a second credit.
	code, okConsume := f.consume(creds, credit.ID, redeemID)
	if !okConsume {
		return true
	}
	f.log(levelInfo, reasonResetSucceeded, map[string]any{
		"credit_id":         credit.ID,
		"code":              code,
		"redeem_request_id": redeemID,
	})

	f.clearCooldown()
	return true
}

// listCredits fetches the account's reset credits. A 401 or 403 means the stored
// token is rejected, which no retry fixes (D21), so it fails at once with
// tokenRejected set; any other failure is retried up to creditsListAttempts times.
// Either way a failed listing is logged once and no consume is sent.
func (f resetFlow) listCredits(creds authCredentials) (credits []resetCredit, ok, tokenRejected bool) {
	var failure map[string]any
	for attempt := 1; attempt <= creditsListAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(creditsListRetryDelay)
		}
		response, errDo := f.h.httpDo(httpRequest{
			Method:  http.MethodGet,
			URL:     codexBaseURL + resetCreditsPath,
			Headers: upstreamHeaders(creds, false),
		})
		if errDo != nil {
			failure = map[string]any{"error": errDo.Error()}
			continue
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			f.log(levelWarn, reasonCreditsListFailed, map[string]any{"status_code": response.StatusCode, "attempts": attempt})
			return nil, false, true
		}
		if !isSuccessStatus(response.StatusCode) {
			failure = map[string]any{"status_code": response.StatusCode}
			continue
		}
		var listing resetCreditsResponse
		if errUnmarshal := json.Unmarshal(response.Body, &listing); errUnmarshal != nil {
			failure = map[string]any{"error": errUnmarshal.Error()}
			continue
		}
		return listing.Credits, true, false
	}
	failure["attempts"] = creditsListAttempts
	f.log(levelWarn, reasonCreditsListFailed, failure)
	return nil, false, false
}

// consume redeems one credit by id. Only reset and already_redeemed mean the credit is
// gone; nothing_to_reset, no_credit (both HTTP 200), any other code and every non-2xx
// status are failures that consumed nothing (D17).
func (f resetFlow) consume(creds authCredentials, creditID, redeemID string) (string, bool) {
	fields := map[string]any{"credit_id": creditID, "redeem_request_id": redeemID}
	// fail adds the one detail that explains this attempt and writes the single line
	// the flow logs for it, so no branch can fail silently (spec story 17).
	fail := func(key string, detail any) (string, bool) {
		fields[key] = detail
		f.log(levelWarn, reasonConsumeFailed, fields)
		return "", false
	}
	body, errMarshal := json.Marshal(consumeRequest{RedeemRequestID: redeemID, CreditID: creditID})
	if errMarshal != nil {
		return fail("error", errMarshal.Error())
	}
	response, errDo := f.h.httpDo(httpRequest{
		Method:  http.MethodPost,
		URL:     codexBaseURL + resetConsumePath,
		Headers: upstreamHeaders(creds, true),
		Body:    body,
	})
	if errDo != nil {
		return fail("error", errDo.Error())
	}
	if !isSuccessStatus(response.StatusCode) {
		return fail("status_code", response.StatusCode)
	}
	var result consumeResponse
	if errUnmarshal := json.Unmarshal(response.Body, &result); errUnmarshal != nil {
		return fail("error", errUnmarshal.Error())
	}
	switch result.Code {
	case consumeCodeReset, consumeCodeAlreadyRedeemed:
		return result.Code, true
	default:
		return fail("code", result.Code)
	}
}

// isSuccessStatus reports whether an upstream call answered 2xx. CPA's management API is
// stricter than the upstream one: clearCooldown accepts exactly 200 (D19).
func isSuccessStatus(status int) bool {
	return status >= 200 && status < 300
}

// clearCooldown asks CPA's management API to drop the credential's cooldown, so the
// restored quota is schedulable again (D19, spec story 11). It runs only after a
// credited consume and never retries: a failure is logged and the flow ends. A blank
// key never reaches here — startAutoReset stops the flow before it spends anything.
func (f resetFlow) clearCooldown() {
	body, errMarshal := json.Marshal(managementResetQuotaRequest{AuthIndex: f.record.AuthIndex})
	if errMarshal != nil {
		f.log(levelWarn, reasonCooldownFailed, map[string]any{"error": errMarshal.Error()})
		return
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+f.cfg.ManagementKey)
	headers.Set("Content-Type", "application/json")
	response, errDo := f.h.httpDo(httpRequest{
		Method:  http.MethodPost,
		URL:     strings.TrimRight(f.cfg.ManagementBaseURL, "/") + managementResetQuotaPath,
		Headers: headers,
		Body:    body,
	})
	if errDo != nil {
		f.log(levelWarn, reasonCooldownFailed, map[string]any{"error": errDo.Error()})
		return
	}
	if response.StatusCode != http.StatusOK {
		f.log(levelWarn, reasonCooldownFailed, map[string]any{"status_code": response.StatusCode})
		return
	}
	f.log(levelInfo, reasonCooldownCleared, nil)
}

// creditExpiry is a credit's expires_at as parsed: Dated is false for a null or
// unparsable value, which means "never expires" (D16).
type creditExpiry struct {
	at    time.Time
	dated bool
}

// before reports whether this credit should be spent before the other: a dated credit
// always beats an undated one, and between two dated credits the earlier expiry wins.
// Two undated credits are equivalent, so the first one stands.
func (e creditExpiry) before(other creditExpiry) bool {
	if e.dated != other.dated {
		return e.dated
	}
	if !e.dated {
		return false
	}
	return e.at.Before(other.at)
}

// pickCredit returns the available credit closest to expiry. A credit that has already
// expired is skipped: consuming it would only earn a no_credit, which would abandon
// the flow while a usable credit sits next to it. Credits are spent before their 30-day
// expiry rather than hoarded (spec story 7).
func pickCredit(credits []resetCredit) (resetCredit, bool) {
	at := now()
	var chosen resetCredit
	var chosenExpiry creditExpiry
	found := false
	for _, credit := range credits {
		if credit.Status != creditStatusAvailable || strings.TrimSpace(credit.ID) == "" {
			continue
		}
		parsed, errParse := time.Parse(time.RFC3339, credit.ExpiresAt)
		candidate := creditExpiry{at: parsed, dated: errParse == nil}
		if candidate.dated && !candidate.at.After(at) {
			continue
		}
		if !found || candidate.before(chosenExpiry) {
			chosen, chosenExpiry, found = credit, candidate, true
		}
	}
	return chosen, found
}

// authCredentials is what upstream calls need from the stored auth file (D14).
type authCredentials struct {
	accessToken string
	accountID   string
}

// parseAuthCredentials reads the flat CPA auth file. A file that is not JSON simply
// yields empty credentials, which the caller treats as an unreadable credential.
func parseAuthCredentials(raw json.RawMessage) authCredentials {
	var file struct {
		AccessToken      string `json:"access_token"`
		AccountID        string `json:"account_id"`
		ChatGPTAccountID string `json:"chatgpt_account_id"`
		IDToken          string `json:"id_token"`
	}
	_ = json.Unmarshal(raw, &file)
	return authCredentials{
		accessToken: strings.TrimSpace(file.AccessToken),
		accountID: usableAccountID(
			file.AccountID,
			file.ChatGPTAccountID,
			idTokenAccountID(file.IDToken),
		),
	}
}

// usableAccountID returns the first candidate that is neither blank nor a placeholder.
// A placeholder (email_xxx / local_xxx) is never sent as a header; falling through to
// the next source is what keeps the header present when the id_token holds a real id.
func usableAccountID(candidates ...string) string {
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if strings.HasPrefix(candidate, placeholderAccountPrefixEmail) || strings.HasPrefix(candidate, placeholderAccountPrefixLocal) {
			continue
		}
		return candidate
	}
	return ""
}

// idTokenAccountID digs chatgpt_account_id out of the id_token JWT claim. A token
// that is absent, truncated or not decodable is simply no account id.
func idTokenAccountID(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if errDecode != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if errUnmarshal := json.Unmarshal(payload, &claims); errUnmarshal != nil {
		return ""
	}
	return claims.Auth.ChatGPTAccountID
}

// upstreamHeaders builds the header set the codex CLI sends (D15): the official
// client adds nothing else, and in particular no OpenAI-Beta.
func upstreamHeaders(creds authCredentials, jsonBody bool) http.Header {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+creds.accessToken)
	if creds.accountID != "" {
		headers.Set("ChatGPT-Account-ID", creds.accountID)
	}
	headers.Set("User-Agent", codexUserAgent)
	headers.Set("originator", codexOriginator)
	if jsonBody {
		headers.Set("Content-Type", "application/json")
	}
	return headers
}

// newUUIDv4 renders a random RFC 4122 version 4 UUID for use as the vendor's
// idempotency key. Only uniqueness is required, but the documented shape is cheap to
// honour, so the version and variant bits are set explicitly.
func newUUIDv4() (string, error) {
	var raw [16]byte
	if _, errRead := rand.Read(raw[:]); errRead != nil {
		return "", errRead
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}
