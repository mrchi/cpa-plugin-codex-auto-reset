# Auto-reset codex weekly limit

Status: implemented

## Problem Statement

I run CLIProxyAPI (CPA) with codex 订阅渠道 credentials. When a credential's 周用量窗口 is exhausted, CPA cools the credential down until the window's natural reset — up to a week of unavailability, and with a single credential the whole channel is dead for days. My accounts hold OpenAI-granted reset credits that could restore the quota immediately, but using them today means noticing the outage and redeeming manually.

## Solution

A CPA plugin that watches for codex credentials hitting weekly-limit exhaustion, and when the credential's account has reset credits available and natural recovery is more than a day away, automatically consumes one credit and clears CPA's local cooldown so the credential is schedulable again — without any manual intervention. Failures fall back silently to CPA's default behavior.

## User Stories

1. As a CPA operator, I want the plugin to detect when a codex credential receives a 429 `usage_limit_reached` from upstream, so that exhaustion is noticed without me watching logs.
2. As a CPA operator, I want exhaustion detection to work even when CPA successfully retries the request on another credential, so that a silently-cooled credential is still repaired.
3. As a CPA operator, I want the plugin to distinguish the 周用量窗口 from the 5 小时窗口，so that reset credits are only spent on weekly exhaustion.
4. As a CPA operator, I want a fallback window classification based on `resets_in_seconds` when `limit_window_minutes` is absent, so that older or variant error bodies still classify correctly.
5. As a CPA operator, I want no reset to happen when the weekly window naturally resets within one day, so that credits aren't burned on quota that would return by itself.
6. As a CPA operator, I want the plugin to check the account's reset credit balance before consuming, so that no consume call is made when no credit is available.
7. As a CPA operator, I want the plugin to select the unexpired credit with the earliest `expires_at`, so that credits closest to their 30-day expiry are spent first and an already-expired credit never costs the flow its chance.
8. As a CPA operator, I want each consume attempt to carry a client-generated idempotency key, so that a retried request cannot burn two credits.
9. As a CPA operator, I want an `already_redeemed` response treated as success, so that a retry after a network failure doesn't loop or report a false failure.
10. As a CPA operator, I want `nothing_to_reset` and `no_credit` responses treated as non-consuming failures, so that the plugin stops quietly instead of retrying.
11. As a CPA operator, I want the credential's CPA cooldown cleared after a successful consume, so that the restored quota is actually schedulable instead of staying locked until the old reset time.
12. As a CPA operator, I want the current failed request to still return its error to the client, so that the plugin stays simple and CPA's own retry behavior handles recovery.
13. As a CPA operator, I want concurrent exhaustion signals for the same credential to trigger only one reset flow, so that simultaneous requests don't burn multiple credits.
14. As a CPA operator, I want a credential whose reset flow just settled (a credit spent, none available, a rejected token, or a consume already sent) to be suppressed from re-triggering for a short period, so that in-flight stale failure records don't cause a second reset or repeat pointless upstream calls.
15. As a CPA operator, I want unlimited credit consumption per account (subject to the one-day guard), so that credits are used before their 30-day expiry rather than hoarded.
16. As a CPA operator, I want any failure in the reset flow (no credit, network error, API error, cooldown-clear failure) to fall back to CPA's default cooldown behavior, so that the plugin never makes things worse than it being absent.
17. As a CPA operator, I want every reset attempt and outcome written to the host log, so that I can audit credit consumption after the fact.
18. As a CPA operator, I want a global enable/disable switch in the plugin configuration, so that I can turn auto-reset off without unloading the plugin.
19. As a CPA operator, I want an explicit inclusion list in the configuration whose default is empty, so that only the accounts whose credits I want spent are touched and nothing is spent by accident (ADR-0004).
20. As a CPA operator, I want the plugin to use the credential's existing OAuth access token and account ID (falling back to `chatgpt_account_id` and the `id_token` claim) for upstream calls, so that no separate credentials need to be provisioned.
21. As a CPA operator, I want cooldown clearing to go through CPA's own host callback rather than the local management API, so that I need no management secret and no extra endpoint configuration (ADR-0006).
22. As a CPA operator, I want non-codex credentials and non-429 failures to be ignored entirely, so that the plugin never interferes with other channels.
23. As a CPA operator, I want the plugin packaged as a c-shared dynamic library loadable from CPA's plugins directory, so that deployment matches CPA's standard plugin mechanism.

## Implementation Decisions

- **One Go module** producing a c-shared plugin (`.dylib`/`.so`) exporting `cliproxy_plugin_init`, per CPA's plugin ABI (ADR-0001, ADR-0002, ADR-0003 govern the core choices; see `docs/adr/`).
- **Observation via `usage.handle` capability only** (ADR-0001). The handler receives per-attempt failure records carrying AuthID/AuthIndex, status code, and the upstream error body; it ignores records that are not codex 429 `usage_limit_reached`.
- **Trigger conditions, all required** (ADR-0002): status 429 with `type=usage_limit_reached`; weekly window (`limit_window_minutes==10080`, fallback `resets_in_seconds>18000` when the field is absent); `resets_in_seconds>86400` (one-day guard). When `resets_in_seconds` is absent the seconds are derived from `resets_at` (integer Unix seconds, legacy RFC3339 accepted), and the decision log names which field the timing came from (decisions.md D24). CPA does not parse `limit_window_minutes`; the plugin parses the raw error body itself.
- **Reset orchestration on trigger**: fetch the credential's stored auth JSON via `host.auth.get` to obtain access token and account ID → `GET /wham/rate-limit-reset-credits` to list credits (a 401/403 is final; any other listing failure is retried up to 3 times within the flow) → pick the unexpired available credit with earliest `expires_at` → `POST /wham/rate-limit-reset-credits/consume` with a fresh UUID v4 `redeem_request_id` and explicit `credit_id` → on `reset` or `already_redeemed`, clear CPA's local cooldown through the host callback `host.routing.reset_cooldown` with the record's `auth_index` (ADR-0006; requires CPA ≥ v8.0.12). Upstream base URL is `https://chatgpt.com/backend-api`.
- **Result semantics**: `reset`/`already_redeemed` = success; `nothing_to_reset`/`no_credit` = failure that consumed nothing; HTTP errors = failure. Any failure aborts the flow, logs, and leaves CPA's default cooldown in place.
- **Debounce**: an in-process per-credential mutex so concurrent triggers serialize; once a flow settles — a credit consumed, no credit available, the token rejected (no access token, or 401/403 on the listing), the listing still failing after its retries, or any consume sent — the credential is suppressed for a fixed short window (5 minutes) so stale failure records don't re-trigger. Only a flow stopped before it could reach upstream (the credential itself being unreadable) is not suppressed. A record without an auth index never starts a flow. No persistence — state lives in process memory and resets with CPA.
- **Idempotency within the flow**: `redeem_request_id` is generated once per flow; each flow sends at most one consume and never retries it (decisions.md D18). A new trigger (after suppression expiry) generates a new one.
- **Configuration** (via `plugins.configs.<id>`): `enabled` (bool), `include_credentials` (list of auth IDs, auth file names or auth indexes, matched against the usage record itself — no `host.auth.list` call, decisions.md D8), whose default is empty so nothing is spent until the operator opts credentials in (ADR-0004). No per-account credit mapping exists — credits are an account-side OpenAI resource. The former `management_key`/`management_base_url` keys are gone (ADR-0006).
- **Logging**: all trigger decisions (including negative ones at debug level), consume outcomes, and cooldown-clear results go through `host.log`.
- **No retry of the client request**: the current request fails through to the client; recovery happens on the next request after cooldown clearing (decided in grilling Q3).

## Testing Decisions

- **Good tests assert external behavior only**: given a usage record and scripted host responses, which host calls were made in what order, and what was logged. No assertions on internal state, parsing intermediates, or private functions.
- **Single seam: the host-callback boundary**. All side effects (`host.auth.get`, `host.http.do`, `host.routing.reset_cooldown`, `host.log`) are faked in-process; `host.http.do` is dispatched by URL to scripted responses for the credits list and the consume, and the cooldown clear is a scripted RPC whose call count and `auth_index` are asserted. Nothing requires a running CPA or a compiled c-shared binary; the ABI entrypoint is a thin untested forwarding layer.
- **Modules under test**: the trigger classifier (window classification, one-day guard, inclusion list, enabled flag), the reset orchestration (credit selection, one idempotency key per flow, listing retries, result-code mapping, cooldown clearing on success only), and the debounce (concurrent triggers serialize, a settled flow's suppression window blocks re-trigger, a transient failure does not).
- **Prior art**: none — the repo is new. Style follows the ponytail rule: plain `testing` package with table-driven cases, no frameworks or fixtures.

## Out of Scope

- 5 小时窗口 exhaustion: never triggers a consume.
- Exposing reset history or credit balances via a management API endpoint (log-only observability).
- Per-account burn-rate caps beyond the one-day guard.
- Retrying or replaying the failed client request after a successful reset.
- Handling the paid "instant reset" product or enterprise `/api/codex/...` endpoint variants.
- Token refresh: the plugin uses the stored access token as-is; a consume failing on an expired token is just a failure that falls back to default behavior.

## Further Notes

- The upstream endpoints are unofficial but verified in production by multiple open-source projects (openai/codex CLI source, openusage's recorded end-to-end redemption, aaamosh/codex-reset); see the conversation research for links.
- One credit consumption resets both the 5-hour and weekly windows upstream (`windows_reset: 2`), even though the trigger only watches the weekly window.
- Known CPA behavior motivating this plugin: upstream issue #5639 — a credential cooled by a weekly 429 stays locked for days even if quota signals later improve, which is exactly the state the cooldown-clearing step repairs.
