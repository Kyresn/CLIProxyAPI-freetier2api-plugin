# FreeTier 2API Plugin

[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![CLIProxyAPI](https://img.shields.io/badge/CLIProxyAPI-Plugin-2496ED?style=flat)](https://github.com/router-for-me/CLIProxyAPI)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

A native dynamic library plugin that connects multiple free-tier AI providers to [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI), reusing the host's OpenAI / Anthropic / Responses compatible routing, authentication, proxy, and load balancing.

[中文](README.md) | English

If this project helps you, a Star would mean a lot.

> **⚠️ Important Compliance Notice**: This project is **strictly for personal learning and technical research only**. Commercial sale, API resale, paid account farming, public/paid shared pools, or any use violating upstream Terms of Service or applicable laws is strictly prohibited. Each provider is hard-limited to a maximum of **10 accounts**. By using this software, you agree to the [Disclaimer](#disclaimer) below.

## Features

- Native integration with CLIProxyAPI's Anthropic `/v1/messages`, OpenAI `/v1/chat/completions`, and OpenAI `/v1/responses` endpoints without running a separate gateway service
- Supports 15 free-tier provider instances across 8 vendor families (AutoClaw CN/Global and Raccoon included) with strict per-vendor credential isolation (capped at 10 accounts per provider)
- Dynamic model catalog discovery, custom model aliases, and cross-provider load balancing / failover
- Built-in single-page Web console for OAuth device login, localhost callback paste, API Key import, account aliases and batch enable/disable/delete, quota tracking, check-in history, model toggling, and live logs
- Automated daily check-in, automated daily tasks, and token keepalive refresh
- Pure Go standard library implementation with zero third-party runtime dependencies

## Supported Providers

| Vendor ID | Display Name | Realm | Auth Mode | Credential File Prefix |
| --- | --- | --- | --- | --- |
| `workbuddycn` | WorkBuddy CN | `cn` | OAuth Device Flow | `workbuddycn-*.json` |
| `workbuddyglobal` | WorkBuddy Global | `global` | OAuth Device Flow | `workbuddyglobal-*.json` |
| `autoclawcn` | AutoClaw CN | `cn` | API Key | `autoclawcn-*.json` |
| `autoclawglobal` | AutoClaw Global | `global` | API Key | `autoclawglobal-*.json` |
| `qodercn` | Qoder CN | `cn` | OAuth Device Flow | `qodercn-*.json` |
| `qoderglobal` | Qoder Global | `global` | OAuth Device Flow | `qoderglobal-*.json` |
| `raccoon` | Raccoon (XiaoHuanXiong) | — | API Key | `raccoon-*.json` |
| `opencodezen` | OpenCode ZEN | `global` | API Key | `opencodezen-*.json` |
| `cline` | Cline | `global` | WorkOS Device Flow | `cline-*.json` |
| `zcodecn` | ZCode CN | `cn` | OAuth CLI Flow (BigModel) | `zcodecn-*.json` |
| `zcodeglobal` | ZCode Global | `global` | OAuth CLI Flow (z.ai) | `zcodeglobal-*.json` |
| `traecn` | Trae CN | `cn` | OAuth Flow | `traecn-*.json` |
| `traeglobal` | Trae Global | `global` | OAuth Flow | `traeglobal-*.json` |
| `codearts` | CodeArts | `cn` | OAuth Flow | `codearts-*.json` |
| `minimaxcodecn` | MiniMax Code CN | `cn` | OAuth Device Flow | `minimaxcodecn-*.json` |
| `minimaxcodeglobal` | MiniMax Code Global | `global` | OAuth Device Flow | `minimaxcodeglobal-*.json` |

> Older builds exposed a single `zcode` vendor with `zcode-*.json` files. Those files are still recognised after the split and default to `zcodeglobal` (only the global realm existed before), so no rename is needed; existing `zcode:<model>` entries in `disabled_models` and model aliases keep resolving to `zcodeglobal`.

## Quick Start

### Option 1: Install via CPA Plugin Store (Recommended)

This plugin is listed in the official CLIProxyAPI plugin registry:

1. Open the CLIProxyAPI Admin Panel → **Plugin Store**.
2. Search for **FreeTier 2API** and click Install.

### Option 2: Manual Installation from Releases

Download the archive for your platform from [Releases](https://github.com/jiandanc/CLIProxyAPI-freetier2api-plugin/releases), extract the shared library (`freetier2api.so` / `freetier2api.dylib` / `freetier2api.dll`) into your CPA plugins directory, and enable it in `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "/path/to/cpa/plugins"
  configs:
    freetier2api:
      enabled: true
      enabled_realms: "cn,global"
```

### Option 3: Build from Source

Requires Go 1.22+ and a C compiler (`CGO_ENABLED=1`), or use the bundled Docker build script (`golang:1.24` Debian glibc image):

```bash
git clone https://github.com/jiandanc/CLIProxyAPI-freetier2api-plugin.git
cd CLIProxyAPI-freetier2api-plugin
./scripts/build.sh
```

The compiled shared library is written to `dist/freetier2api.<so|dylib|dll>`.

## Usage

### 1. Open Console & Add Accounts

Once CLIProxyAPI is running, open the embedded console:

```text
http://<CPA-HOST>:8317/v0/resource/plugins/freetier2api/console
```

- Click **"添加账号 ▾" (Add Account)**, choose a provider, and complete the browser OAuth flow or enter an API Key (up to 10 accounts per provider).
- When running on a remote host, Docker, or WSL where `127.0.0.1` callbacks (such as Trae or CodeArts) cannot reach the container directly, expand **"回调没到插件？粘贴回调链接"** in the login box and paste the full localhost callback URL from your browser's address bar.

### 2. Call the API

Call standard CLIProxyAPI endpoints using bare model names or `<vendor>:<model>` prefixes:

```bash
# OpenAI Chat Completions
curl http://localhost:8317/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <CPA_API_KEY>" \
  -d '{"model":"GLM-5.3","messages":[{"role":"user","content":"Hello!"}]}'

# Anthropic Messages
curl http://localhost:8317/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -H "x-api-key: <CPA_API_KEY>" \
  -d '{"model":"GLM-5.3-Flash","max_tokens":1024,"messages":[{"role":"user","content":"Hello!"}]}'
```

### 3. Model Aliases & Cross-Provider Routing

In the Models table of the console, click any model ID or its `✎` icon to assign an external alias. Assigning the same alias to models across different providers merges them into a single logical model ID in CLIProxyAPI for automatic cross-provider load balancing and failover.

### 4. Account Aliases

The Accounts table shows whatever nickname or email the upstream returns, and some platforms only hand out a default nickname that cannot be changed — with several accounts it becomes impossible to tell them apart. Click the account name or its `✎` icon to set a custom alias; the editor is prefilled with the current name, and the display order becomes **alias → upstream nickname/email → file name → account ID**. Saving an empty value, or typing the original name back, means "no alias" — the record is cleared and the upstream name is used, so behavior is unchanged when no alias is set.

Aliases are stored in `state.json` under `account_aliases`, keyed by account identity (the credential file name, i.e. `core.Credential.FileID`) rather than by nickname or token, so they survive token refresh and re-login into the same credential file.

### 5. Account Batch Actions

The Accounts table has a selection column like the Models table. The toolbar reads **Add account / Disable selected / Enable selected / Select all / Clear / Token refresh / Query quota / Check in / Run tasks / Delete**, followed by a filter box that matches provider, account name, alias or file name (never credential contents):

- Nothing is selected by default. Delete is disabled while the selection is empty, and Token refresh, Query quota, Check in and Run tasks act on **all accounts**;
- With a selection, those actions only touch the selected accounts. The selection survives table refreshes, and accounts that disappear drop out of it automatically;
- Filtering only changes which rows are visible: Select all / Clear act on the visible rows, accounts hidden by the filter keep their selection, and batch actions still run against the full selection;
- Disabling is not deleting: it only writes the host's `disabled` flag into the credential JSON (token and file are kept, so it is reversible). Delete removes the credential file;
- Delete still takes two clicks: the first arms the button (auto-reset after 5 seconds), the second deletes.

## Configuration

Configure under `plugins.configs.freetier2api` in `config.yaml` (runtime settings can also be toggled live in the web console):

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `enabled_realms` | string | `cn,global` | Enabled regions (`cn` / `global`) |
| `auto_checkin` | bool | `true` | Enable automated daily check-in |
| `auto_checkin_at` | string | `10:00` | Daily check-in time (`HH:MM`, local time). Scheduling is hourly, so any minute above 0 rounds up to the next hour and the run never fires before the configured time |
| `auto_tasks` | bool | `true` | Enable automated daily tasks |
| `checkin_history_limit` | int | `30` | Check-in records kept per account (`<=0` falls back to 30; expandable in the console) |
| `prompt_mode` | string | `passthrough` | System prompt filter mode: `passthrough` / `custom` / `append` |
| `model_name_tag` | string | `off` | Channel tag on model IDs: `off` (default) / `prefix` / `suffix`. When enabled, every vendor model gets a vendor prefix/suffix in its public ID (e.g. `zcodeglobal-GLM-5.3`) so same-name models can be pinned to a channel; tags are per-vendor, not per-account. Once tagged, same-name models are no longer merged across vendors. Takes effect after the host rebuilds its model registry (restart or credential reload; the console shows a restart-pending hint) |
| `disabled_models` | string / list | empty | Disabled models (`vendor:model` or bare name) |
| `extra_models` | string | empty | Additional model IDs to register (comma-separated) |
| `state_dir` | string | `~/.freetier2api-plugin` | State and cache directory |
| `log_level` | string | `info` | Log level: `debug` / `info` / `error` |

## Project Architecture

```text
package main / cgo        C ABI entry (main.go / cgo.go)
  └─ internal/app         composition root and ABI adapter
       ├─ internal/admin    management HTTP API + config + model catalog + console
       ├─ internal/tasks    vendor-neutral task catalog/scheduler/queue/engine
       ├─ internal/auth     account enumeration/batch/quota formatting + login sessions
       ├─ internal/vendors/<name>   provider protocol and adapter layer (single package, closed filename whitelist)
       │    ├─ adapter.go           [required] core.Vendor implementation + Instances/Configure/Match + ConfigFields
       │    ├─ chat.go              [required] protocol endpoints, signing/headers, payload transforms, Chat/ChatStream, SSE
       │    ├─ credential.go        [required] credential parsing/ownership/storage, device identity, helpers
       │    ├─ models.go            [required] model catalog/probing + bundledModels() fallback
       │    ├─ errors.go            [required] upstream error classification and retryability
       │    ├─ login.go             [required] AuthMode + login flows + refresh (exactly one)
       │    ├─ tasks.go             [optional] quota/check-in/plans/tasks/daily rewards/account profile
       │    ├─ *_test.go            [as needed] beside the source; live tests use *_live_test.go
       │    └─ assets/              [as needed] go:embed resources
       └─ internal/core            vendor-neutral contracts
```

- Vendor packages may depend only on `core` + `hostbridge` + `logger`.
- `internal/core` depends only on the standard library; only `internal/app/vendors.go` may import concrete vendors.
- Every new internal package must declare its layer boundary (`layerRules` / `internalPackages`).
- Root-package (C ABI) tests live beside the source (`cgo_test.go`; host-buffer and read-only-page cases in `cgo_hostbuffer_test.go` on POSIX), not under `tests/`.

## Acknowledgments & Third-Party Notices

This project references or adapts protocol designs, ABI contracts, and metadata from the following open-source projects:

- **[router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)** (MIT): Host plugin ABI/API contracts (`cpasdk/`) and HTTP/SSE bridge design.
- **[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)** & **[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)** (MIT): WorkBuddy protocol adaptation, prompt sanitization, and task system reference.
- **[aimod-cc/Agent2API](https://github.com/aimod-cc/Agent2API)**: AutoClaw and Raccoon protocol adaptation (header set/signing, proxy channel, authorize login, model catalog, points & check-in semantics) reference.

## Contributing

Friendly discussion is welcome. If you run into issues, try asking Claude Code, Codex, or similar tools for help first — most problems can be solved that way. PRs are even better.

## Disclaimer

1. **Personal & Educational Use Only**: This project is provided strictly for personal learning, technical research, and protocol interoperability testing. Commercial sale, API resale, paid hosting, automated reward farming, public shared pools, or any illegal use is strictly prohibited.
2. **Non-Affiliation**: This project is an independent open-source work and is not affiliated with, endorsed by, or sponsored by Tencent (WorkBuddy / CodeBuddy), Alibaba (Qoder), Zhipu / Z.AI (ZCode), ByteDance (Trae), Huawei Cloud (CodeArts), MiniMax, Cline, OpenCode, or any other upstream provider.
3. **Compliance & Assumption of Risk**: Accessing services via unofficial clients or using automated check-in/task features may violate the Terms of Service (ToS) of upstream platforms and may result in account suspension, quota revocation, or other legal/compliance consequences. Users are solely responsible for ensuring their usage complies with all applicable Terms of Service and laws. The authors and contributors assume no liability for any direct or indirect damages arising from the use of this software.
4. **Takedown Contact**: If any upstream provider or rights holder believes any part of this repository infringes upon your legitimate rights, please open an Issue and the relevant adapter code will be promptly removed.

## License

[MIT](LICENSE)
