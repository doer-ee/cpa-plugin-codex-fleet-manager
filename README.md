# Codex Fleet Manager

[简体中文](README.zh-CN.md) | English

## TL;DR

Codex Fleet Manager is a Codex account scheduling and model failover plugin
for CLIProxyAPI. It automatically selects the right Codex account based
on quota and reset time to keep account usage balanced, and retries requests
against a preconfigured fallback model chain when a model is at capacity,
overloaded, or an upstream request fails.


---------

`codex-fleet-manager` is a dynamic library plugin for CLIProxyAPI (CPA). It
provides quota-aware scheduling, account-health monitoring, reset-window
activation, and account annotations for Codex accounts.

Codex Fleet Manager is derived from Codex Quota Scheduler by Jeffery Zhang and
is independently improved and maintained. It is distributed under the MIT
License and preserves the original project's copyright and license notices. It
is not an official successor or endorsed release of the original project.

## v0.1.0 Highlights

- Independent plugin identity: dedicated CPA API routes, browser storage,
  state directory, dynamic-library names, and release archives.
- Optimized Fill First scheduling based on real account availability and quota
  pressure rather than a static account order.
- Optional reset-window activation and deadline-driven quota refresh.
- Bilingual Management UI, account annotations, priorities, and JSON backup.

## v0.3.14 Highlights

- Added the Model Retry Chain page with Live, Shadow, and Always Retry modes.
- Added ordered, independently numbered fallbacks and automatic routable-model
  catalog loading with duplicate model IDs removed.
- Added bilingual retry scheduler logs and a separate Settings page layout.
- Added CPA `v7.3.4+` version checking and an inline, user-confirmed comparison
  table for repairing the four stream/retry prerequisites; differences are
  highlighted in red and the repair uses CPA hot reload.

## Model Retry Chain

The Management UI includes an optional retry chain for upstream capacity and
transport failures. When enabled, a request that fails before content reaches
the client can move through configured fallback models. Retryable failures
include HTTP 429, 502, and 503, capacity/overload failures, and equivalent
transport failures. A stream that returns HTTP 200 but exposes an upstream
failure before the response is committed can also be handled by the retry
runtime.

Open **Retry Chain** from the Fleet Manager navigation. Each requested model
can have ordered model-only fallbacks; CPA resolves the optional provider.
Fallbacks are numbered independently for each requested model as `Fallback`,
`Fallback 2`, `Fallback 3`, and so on.

Retry modes are **Live**, **Shadow** (record only, never retry), and **Always
retry all models**. Always Retry also covers models without a configured chain;
when no fallback exists, it retries the same model. Shadow and Always Retry are
mutually exclusive. The page also configures maximum attempts (including the
first attempt), silence/hold/chain timeouts, frame and byte buffers, and whether
encrypted reasoning is removed when switching models.

The page can check and repair the CPA prerequisites, but CPA must be `v7.3.4`
or newer. If values are wrong, it shows an inline comparison table with current
and recommended values; differences are marked in red. Nothing changes until
**Apply recommended settings** is selected. The repair hot-reloads:

```yaml
request-retry: 3
codex:
  stream-bootstrap-buffering: true
  stream-bootstrap-timeout: "0"
streaming:
  bootstrap-retries: 1
```

No CPA container restart is required. Retry scheduler events are persisted in
plugin logs and localized in English and Chinese.

## Included scheduler capabilities

- Availability now comes before plugin priority: an unusable high-priority
  account can no longer outrank a usable account.
- Unavailable accounts are displayed by expected recovery time, earliest first,
  with unknown recovery times last.
- The five-hour quota window is optional. Accounts remain schedulable when
  OpenAI omits it, provided a valid weekly or monthly long window is present.
- A missing or invalid long window remains Unknown and unavailable, allowing CPA
  fallback instead of making an unsafe selection.
- The active Codex account list is synchronized from CPA's authoritative auth
  roster and restricted to the highest confirmed CPA auth-priority tier.
- Reset-window activation uses a persisted, single-flight sequence that verifies
  the result after one small Codex request. It remains opt-in.
- The Management UI queue follows the same availability classes and ordering
  rules as production selection.

## How Scheduling Works

The scheduler applies four layers of decisions. Each layer narrows or orders the
accounts passed to the next one.

### 1. Admit the active CPA priority tier

- Only candidates whose provider is `codex` are considered. Other providers are
  ignored.
- A Codex account without an explicit CPA auth priority is treated as priority `0`.
- The plugin admits every Codex account in the highest confirmed CPA auth
  priority tier. Lower CPA tiers remain under CPA's own fallback behavior and
  are not loaded into the plugin queue.
- If all Codex accounts should participate together, give them the same CPA auth
  priority. Priority `0` is the simplest recommended configuration.

CPA auth priority and the plugin-owned account priority are separate settings.
The plugin never reads its account priority from CPA and never writes it back to
CPA.

### 2. Classify real availability

Admitted accounts are divided into three practical groups:

1. **Ready:** quota evidence is fresh or aging and the account is usable.
2. **Trial eligible:** evidence is unknown or stale but the account can be
   verified safely before use.
3. **Unavailable:** the long quota window is exhausted, authentication is
   blocked, the circuit is open, temporary exhaustion feedback is active, or
   the account cannot be verified safely.

Ready accounts are considered before trial-eligible accounts. Unavailable
accounts are not selected by the plugin.

A valid long window—weekly or monthly—is required. The five-hour window is optional:
if OpenAI temporarily omits it, a valid long window is enough for the
account to remain usable. If the long window is missing or invalid, the account
stays Unknown and unavailable so CPA fallback can take over.

When both long-window exhaustion and older temporary-exhaustion feedback are
present, the weekly or monthly exhaustion is authoritative and is shown as the
reason the account is unavailable.

### 3. Order selectable accounts

Plugin priority is applied only after availability classification, and only
inside the same selectable class. A higher plugin priority is considered first,
but it cannot move an unavailable account ahead of a usable one.

Within the same plugin priority:

1. If `monthly_mode` is `priority`, monthly accounts come before weekly
   accounts.
2. Accounts with higher quota pressure are preferred. Quota pressure is the
   remaining long-window percentage divided by the hours until that window
   resets, with a 30-minute minimum divisor.
3. Earlier expiry, then higher remaining quota, break ties when pressure is
   equal or unavailable.
4. The stable account ID order breaks the final tie.

With the default `monthly_mode: expiry_order`, weekly and monthly accounts share
the same combined remaining-quota/reset-pressure ordering. The alternative
`priority` mode explicitly prefers monthly accounts before applying pressure.

### 4. Order unavailable accounts

Plugin priority is ignored after an account becomes unavailable: priority
cannot make an exhausted account usable.

The Management queue orders unavailable accounts by expected recovery time from
earliest to latest. Accounts with no known recovery time appear last. This makes
the visible queue describe what can actually become usable next rather than
preserving an irrelevant priority order.

If neither the Ready nor trial-eligible group contains a safe choice, the plugin
returns no selection and allows CPA fallback.

### Example

Given four admitted Codex accounts, the visible and effective order is:

| Order | State | Plugin priority | Recovery | Why |
| --- | --- | ---: | --- | --- |
| 1 | Ready | 10 | — | Highest priority among Ready accounts |
| 2 | Ready | 0 | — | Still usable, so it stays ahead of every unavailable account |
| 3 | Weekly quota exhausted | 100 | 2 hours | Unavailable; high priority is ignored and this is the earliest recovery |
| 4 | Exhausted or Unknown | 1000 | Later or unknown | Unavailable and expected to recover last |

## Quota Refresh And Reset-Window Activation

### Quota refresh

Quota refresh reads the current Codex quota state from ChatGPT. It does not send
an ordinary model request and does not need the Management page to remain open.

During recent Codex activity, accounts are refreshed when their individual
deadlines become due; the worker does not repeatedly scan every account at a
fixed global interval. After the active window becomes idle, normal background
refresh sleeps until a Codex request, a management action, or a due reset-window
operation wakes it.

A `usage_limit_reached` response immediately marks the selected account
temporarily exhausted until its reported reset time, or for two minutes when no
reset time is provided. Quota exhaustion does not count as a circuit-breaker
failure. Repeated non-quota failures use the circuit breaker instead.

### Reset-window activation

OpenAI may report that a quota reset time has passed without creating the next
quota window until the account sends another Codex request. When automatic
reset-window activation is enabled, the plugin:

1. checks the quota again;
2. sends one tiny Codex request only if the new window is still missing;
3. verifies the quota afterward; and
4. persists the operation state so restart recovery verifies before retrying.

This feature is disabled by default because the activation request may consume
a small amount of quota. Concurrent triggers share one operation rather than
sending duplicate requests.

### When CPA cannot confirm the account list

Normal refresh and reset-window activation stop when CPA cannot confirm the
current Codex account list and priorities. The plugin can continue serving safe
management information while it retries roster synchronization.

The high-risk setting `probe_on_provisional_roster` permits reset-window
activation using the most recently saved account list during that condition.
Credentials are revalidated before each attempt, but the plugin still cannot
guarantee that an account was not removed or moved to another CPA priority tier.
Keep this setting disabled unless you understand and accept that risk.

## Features

- Optimized Fill First scheduling for CPA Codex accounts.
- Availability-first production selection and Management queue ordering.
- Weekly and monthly quota support with an optional five-hour window.
- Usage feedback handling for `usage_limit_reached` responses.
- Per-account failure circuit breaker.
- Deadline-driven quota refresh and opt-in reset-window activation.
- English and Chinese Management UI with browser-language detection.
- Account aliases, notes, tags, groups, and plugin priorities.
- JSON export and import for scheduler settings and annotations.
- Release packages for Linux, macOS, Windows, and FreeBSD.

## Installation

Until Codex Fleet Manager is accepted by CPA's Plugin Store, download the
archive for your platform from the
[latest GitHub release](https://github.com/doer-ee/cpa-plugin-codex-fleet-manager/releases/latest):

```text
codex-fleet-manager_<version>_<goos>_<goarch>.zip
```

Extract the library from the archive root and place it in CPA's matching plugin
directory:

- macOS: `codex-fleet-manager.dylib`
- Linux and FreeBSD: `codex-fleet-manager.so`
- Windows: `codex-fleet-manager.dll`

Example:

```bash
mkdir -p /path/to/CLIProxyAPI/plugins/darwin/arm64
cp codex-fleet-manager.dylib /path/to/CLIProxyAPI/plugins/darwin/arm64/
```

## CPA Configuration

Enable plugins globally and enable this plugin:

```yaml
plugins:
  enabled: true
  configs:
    codex-fleet-manager:
      enabled: true
      priority: 1 # CPA plugin registration/load priority
```

The registration/load `priority` above is not CPA account priority and is not
the plugin-owned per-account scheduler priority. Account annotations and
scheduler settings are managed from the plugin page rather than CPA's generic
plugin form.

Default scheduler settings:

```yaml
handle_enabled: true
quota_refresh_interval: 30m
stale_after: 5h
refresh_active_window: 1h
refresh_after_reset_delay: 1m
refresh_retry_delays: 1m,5m,15m
refresh_on_startup: false
monthly_mode: expiry_order
fallback: fill-first
enable_usage_feedback: true
enable_reset_probe: false
probe_on_provisional_roster: false
max_refresh_concurrency: 1
quota_endpoint: https://chatgpt.com/backend-api/wham/usage
circuit_failure_threshold: 5
circuit_open_duration: 30m
circuit_half_open_success_threshold: 2
max_log_entries: 200
log_retention: 24h
```

`monthly_mode` accepts:

- `expiry_order`: use expiry-based ordering across weekly and monthly accounts.
- `priority`: prefer monthly accounts before weekly accounts within the same
  selectable class and plugin priority. Within those boundaries, remaining
  long-window quota and time to reset are combined as quota pressure.

`quota_endpoint` is restricted to the expected ChatGPT quota endpoint and cannot
be redirected to an arbitrary host.

## Management UI

Open **Codex Fleet Manager** from CPA Management Center, or visit:

```text
/v0/resource/plugins/codex-fleet-manager/status
```

The page provides:

- the production-ordered account queue and next-account preview;
- separate Account Queue, Settings, and Retry Chain pages rather than placing
  all settings in the middle column;
- separate CPA priority and plugin priority indicators;
- quota bars, reset times, availability reasons, and circuit state;
- quota bars use green at 60% or above, orange from 30% through below 60%, and red below 30% for both quota windows;
- scheduler settings with plain-language safety guidance;
- aliases, notes, tags, groups, and per-account plugin priority editing;
- quota refresh, log viewing/export, and configuration import/export;
- automatic loading and de-duplication of routable model IDs when opening the
  Retry Chain page; and
- English and Chinese interface switching.

When embedded in CPA Management Center, the plugin initially follows CPA's
current language: Chinese locales use Chinese, while every other locale defaults
to English. A language explicitly selected inside the plugin is remembered and
takes precedence on later visits. The registered CPA sidebar label is
**Codex Fleet Manager**.

Protected data and actions require the CPA Management key. By default, the key
remains only in the current browser page session. The optional **Remember
management key in this browser** setting saves it unencrypted in browser local
storage and automatically reloads protected data on later visits. Enable this
only on a trusted device. The key is never saved to plugin state, exports, or
logs; clearing the checkbox removes the browser-stored copy.

## Privacy And Data Disclosure

The plugin runs inside CPA. It uses CPA host callbacks and plugin-owned CPA
Management API routes; it does not run an external service and does not send
data to the plugin author.

It may send authenticated requests using the Codex credentials already
configured in CPA to:

```text
GET https://chatgpt.com/backend-api/wham/usage
GET https://chatgpt.com/backend-api/wham/rate-limit-reset-credits
```

When reset-window activation is enabled, the plugin may also send the small
Codex activation request described above.

Local plugin state can contain scheduler settings, quota snapshots, operation
state, logs, aliases, notes, tags, and group names. Do not put secrets in notes,
aliases, tags, or group annotations. The Management UI avoids rendering access
tokens, authorization headers, cookies, and other credential fields.

Resource routes serve UI assets only. Account data and privileged operations use
Management routes and require the CPA Management key.

## Build

Requirements:

- Go 1.26 or newer, as declared by `go.mod`.
- CGO support and a C compiler for `-buildmode=c-shared`.
- `make` for the cross-platform release workflow.

Run tests:

```bash
make test
```

Build the current platform library:

```bash
make build
```

Build release archives and checksums:

```bash
make package VERSION=0.1.0
make checksums VERSION=0.1.0
```

Windows users can build `dist/codex-fleet-manager.dll` with:

```powershell
.\build.ps1
```

## GitHub Releases

Pushing a dotted numeric tag such as `v0.1.0` runs the GitHub Actions release
workflow. It tests the repository and publishes platform archives plus
`checksums.txt`:

```bash
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

Release archives use this naming scheme:

```text
codex-fleet-manager_<version>_<goos>_<goarch>.zip
```

## Management API

The UI resource is served from:

```text
GET /v0/resource/plugins/codex-fleet-manager/status
```

Privileged operations require the CPA Management key:

```text
GET  /v0/management/plugins/codex-fleet-manager/status?format=json
GET  /v0/management/plugins/codex-fleet-manager/logs
GET  /v0/management/plugins/codex-fleet-manager/export
PUT  /v0/management/plugins/codex-fleet-manager/settings
POST /v0/management/plugins/codex-fleet-manager/refresh
POST /v0/management/plugins/codex-fleet-manager/refresh/account
POST /v0/management/plugins/codex-fleet-manager/import
PUT  /v0/management/plugins/codex-fleet-manager/annotations
PATCH /v0/management/plugins/codex-fleet-manager/annotations/account
PATCH /v0/management/plugins/codex-fleet-manager/annotations/group
```

## License

MIT License. See [LICENSE](LICENSE).
