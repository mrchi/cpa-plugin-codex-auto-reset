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

	// Consume result codes. The upstream enum is closed; anything else is treated as
	// a failure that consumed nothing (D17).
	consumeCodeReset           = "reset"
	consumeCodeAlreadyRedeemed = "already_redeemed"

	// Account id placeholders that must never be sent as a header (D14).
	placeholderAccountPrefixEmail = "email_"
	placeholderAccountPrefixLocal = "local_"

	accountIDClaimKey = "https://api.openai.com/auth"
)

// Reset-flow log messages, one per outcome, so credit consumption stays auditable
// after the fact (spec story 17).
const (
	reasonResetInFlight      = "auto-reset: reset already in flight: signal dropped"
	reasonResetSuppressed    = "auto-reset: credential suppressed after a recent reset: signal dropped"
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

// startReset dispatches the reset flow for one hit signal and returns immediately
// (D11); a signal arriving while a flow is running or inside the suppression window
// is dropped with a debug log rather than queued (D12).
func startReset(state *resetState, h host, cfg pluginConfig, record pluginapi.UsageRecord) {
	// Pre-flight: without a management key the cooldown can never be cleared, so a
	// consumed credit would leave the credential locked until the old reset time and
	// buy nothing. Stop before spending anything (spec story 16).
	if strings.TrimSpace(cfg.ManagementKey) == "" {
		h.log("warn", reasonManagementKeyEmpty, logFields(resetFields(record, nil)))
		return
	}
	if claimed, reason := state.begin(record.AuthIndex); !claimed {
		h.log("debug", reason, logFields(resetFields(record, nil)))
		return
	}
	// The goroutine captures this state, so a later reconfigure or test cannot
	// redirect the flow's bookkeeping at another state object.
	go func() {
		state.finish(record.AuthIndex, runReset(h, cfg, record))
	}()
}

// runReset performs the whole flow: read the credential, list the account's reset
// credits, consume the earliest-expiring available one, then clear CPA's cooldown. It
// reports whether a credit was consumed. Every failure returns false without retrying
// or compensating, leaving CPA's default cooldown in place (spec stories 6-16).
func runReset(h host, cfg pluginConfig, record pluginapi.UsageRecord) bool {
	auth, errAuth := h.authGet(record.AuthIndex)
	if errAuth != nil {
		h.log("warn", reasonAuthUnreadable, logFields(resetFields(record, map[string]any{"error": errAuth.Error()})))
		return false
	}
	creds := parseAuthCredentials(auth.JSON)
	if creds.accessToken == "" {
		h.log("warn", reasonNoAccessToken, logFields(resetFields(record, nil)))
		return false
	}

	credit, okCredit := fetchResetCredit(h, creds, record)
	if !okCredit {
		return false
	}

	// One idempotency key per flow, generated once and outside any retry, so a
	// replayed consume can never burn a second credit (D18, spec story 8).
	redeemID, errUUID := newUUIDv4()
	if errUUID != nil {
		h.log("warn", reasonUUIDFailed, logFields(resetFields(record, map[string]any{"error": errUUID.Error()})))
		return false
	}

	code, okConsume := consumeResetCredit(h, creds, credit.ID, redeemID, record)
	if !okConsume {
		return false
	}
	h.log("info", reasonResetSucceeded, logFields(resetFields(record, map[string]any{
		"credit_id":         credit.ID,
		"code":              code,
		"redeem_request_id": redeemID,
	})))

	clearCooldown(h, cfg, record)
	return true
}

// fetchResetCredit lists the account's credits and picks the one to spend. A listing
// that cannot be read or holds nothing available aborts the flow; no consume is sent.
func fetchResetCredit(h host, creds authCredentials, record pluginapi.UsageRecord) (resetCredit, bool) {
	response, errDo := h.httpDo(httpRequest{
		Method:  http.MethodGet,
		URL:     codexBaseURL + resetCreditsPath,
		Headers: upstreamHeaders(creds, false),
	})
	if errDo != nil {
		h.log("warn", reasonCreditsListFailed, logFields(resetFields(record, map[string]any{"error": errDo.Error()})))
		return resetCredit{}, false
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		h.log("warn", reasonCreditsListFailed, logFields(resetFields(record, map[string]any{"status_code": response.StatusCode})))
		return resetCredit{}, false
	}
	var listing resetCreditsResponse
	if errUnmarshal := json.Unmarshal(response.Body, &listing); errUnmarshal != nil {
		h.log("warn", reasonCreditsListFailed, logFields(resetFields(record, map[string]any{"error": errUnmarshal.Error()})))
		return resetCredit{}, false
	}
	credit, okCredit := pickCredit(listing.Credits)
	if !okCredit {
		h.log("info", reasonNoAvailableCredit, logFields(resetFields(record, map[string]any{
			"credits": len(listing.Credits),
		})))
		return resetCredit{}, false
	}
	return credit, true
}

// consumeResetCredit redeems one credit by id. Only reset and already_redeemed mean
// the credit is gone; nothing_to_reset, no_credit (both HTTP 200), any other code and
// every non-2xx status are failures that consumed nothing (D17). Each branch adds its
// own detail to the local field set and returns, so the set is never shared.
func consumeResetCredit(h host, creds authCredentials, creditID, redeemID string, record pluginapi.UsageRecord) (string, bool) {
	fields := map[string]any{"credit_id": creditID, "redeem_request_id": redeemID}
	body, errMarshal := json.Marshal(consumeRequest{RedeemRequestID: redeemID, CreditID: creditID})
	if errMarshal != nil {
		fields["error"] = errMarshal.Error()
		h.log("warn", reasonConsumeFailed, logFields(resetFields(record, fields)))
		return "", false
	}
	response, errDo := h.httpDo(httpRequest{
		Method:  http.MethodPost,
		URL:     codexBaseURL + resetConsumePath,
		Headers: upstreamHeaders(creds, true),
		Body:    body,
	})
	if errDo != nil {
		fields["error"] = errDo.Error()
		h.log("warn", reasonConsumeFailed, logFields(resetFields(record, fields)))
		return "", false
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		fields["status_code"] = response.StatusCode
		h.log("warn", reasonConsumeFailed, logFields(resetFields(record, fields)))
		return "", false
	}
	var result consumeResponse
	if errUnmarshal := json.Unmarshal(response.Body, &result); errUnmarshal != nil {
		fields["error"] = errUnmarshal.Error()
		h.log("warn", reasonConsumeFailed, logFields(resetFields(record, fields)))
		return "", false
	}
	switch result.Code {
	case consumeCodeReset, consumeCodeAlreadyRedeemed:
		return result.Code, true
	default:
		fields["code"] = result.Code
		h.log("warn", reasonConsumeFailed, logFields(resetFields(record, fields)))
		return "", false
	}
}

// clearCooldown asks CPA's management API to drop the credential's cooldown, so the
// restored quota is schedulable again (D19, spec story 11). It runs only after a
// credited consume and never retries: a failure is logged and the flow ends. A blank
// key never reaches here — startReset stops the flow before it spends anything.
func clearCooldown(h host, cfg pluginConfig, record pluginapi.UsageRecord) {
	body, errMarshal := json.Marshal(managementResetQuotaRequest{AuthIndex: record.AuthIndex})
	if errMarshal != nil {
		h.log("warn", reasonCooldownFailed, logFields(resetFields(record, map[string]any{"error": errMarshal.Error()})))
		return
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+cfg.ManagementKey)
	headers.Set("Content-Type", "application/json")
	response, errDo := h.httpDo(httpRequest{
		Method:  http.MethodPost,
		URL:     strings.TrimRight(cfg.ManagementBaseURL, "/") + managementResetQuotaPath,
		Headers: headers,
		Body:    body,
	})
	if errDo != nil {
		h.log("warn", reasonCooldownFailed, logFields(resetFields(record, map[string]any{"error": errDo.Error()})))
		return
	}
	if response.StatusCode != http.StatusOK {
		h.log("warn", reasonCooldownFailed, logFields(resetFields(record, map[string]any{"status_code": response.StatusCode})))
		return
	}
	h.log("info", reasonCooldownCleared, logFields(resetFields(record, nil)))
}

// pickCredit returns the available credit closest to expiry. A credit that has already
// expired is skipped: consuming it would only earn a no_credit, which would abandon
// the flow while a usable credit sits next to it. A null (or unparsable) expires_at
// means "never expires" and always ranks last (D16). Credits are spent before their
// 30-day expiry rather than hoarded (spec story 7).
func pickCredit(credits []resetCredit) (resetCredit, bool) {
	at := now()
	var chosen resetCredit
	var chosenExpiry time.Time
	chosenDated, found := false, false
	for _, credit := range credits {
		if credit.Status != creditStatusAvailable || strings.TrimSpace(credit.ID) == "" {
			continue
		}
		expiry, errParse := time.Parse(time.RFC3339, credit.ExpiresAt)
		dated := errParse == nil
		if dated && !expiry.After(at) {
			continue
		}
		if !found || earlier(expiry, dated, chosenExpiry, chosenDated) {
			chosen, chosenExpiry, chosenDated, found = credit, expiry, dated, true
		}
	}
	return chosen, found
}

// earlier reports whether a candidate credit should replace the current choice: a
// dated credit always beats an undated one, and between two dated credits the
// earlier expiry wins. Two undated credits are equivalent, so the first one stands.
func earlier(candidate time.Time, candidateDated bool, chosen time.Time, chosenDated bool) bool {
	if candidateDated != chosenDated {
		return candidateDated
	}
	if !candidateDated {
		return false
	}
	return candidate.Before(chosen)
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

// resetFields carries the credential identity on every log line of the flow.
func resetFields(record pluginapi.UsageRecord, extra map[string]any) map[string]any {
	fields := map[string]any{"auth_id": record.AuthID, "auth_index": record.AuthIndex}
	for key, value := range extra {
		fields[key] = value
	}
	return fields
}
