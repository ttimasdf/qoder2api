# qoder2api — Fork Findings & Progress

Status snapshot of forking `codex2api` into `qoder2api` (OpenAI/Anthropic-compatible
gateway targeting Qoder / Qoder CN upstream). This file summarizes what has been
discovered and what has been built so far.

---

## 1. What qoder-cn actually is

`qoder-cn` (the `qoder-cn` CLI / `QoderCN` app) is a **VS Code / Electron-based AI
IDE** (Aliyun Lingma / Tongyi lineage), not a plain HTTP API client. Architecture:

```
Electron UI ──> aicoding-agent extension (TS) ──IPC/WebSocket──> Go backend "QoderCN" ──HTTPS──> Qoder cloud
```

- Extension: `…/resources/app/extensions/aicoding-agent/dist/extension.js`
  (namespace `aicoding`, talks to a bundled Go binary).
- Go backend (the real upstream client):
  `…/resources/app/resources/bin/x86_64_linux/QoderCN` — a 108 MB stripped Go
  binary containing all `cosy/*` packages (auth, remoting, chat, codebase, etc.).
- Running instance state: `~/.config/QoderCN/SharedClientCache/`
  (`.info.json` → websocket port + unix socket `qodercn.sock`, `cache/`,
  `logs/qoder.log`, encrypted `cache/user` + `cache/quota`).

Everything in the protocol below was reverse-engineered from the Go binary
(`strings`), the running client's logs, and config files.

---

## 2. Upstream endpoints (from embedded `remote_config`)

Two editions are baked into the binary:

| Field | Qoder (intl.) | Qoder CN (Aliyun RDC) |
|---|---|---|
| big_model_endpoint | `https://center.qoder.sh/algo` | `https://qoder.com.cn` |
| infer_api_endpoint | `https://api3.qoder.sh` | (region-routed via RDC) |
| open_api_endpoint | `https://openapi.qoder.sh` | (same RDC host) |
| quest_server_endpoint | `https://qts2.qoder.sh` | — |
| login_url | `https://www.qoder.com/device/selectAccounts` | `https://devops.aliyun.com/lingma/login` |
| message_encode | `1` | `1` |
| login_encode | `2` | `2` |

- The CN endpoint is also confirmed at runtime in
  `~/.config/QoderCN/SharedClientCache/cache/app-config.json`
  (`"endpoint": "https://qoder.com.cn"`).
- All algo calls live under `<endpoint>/algo/api/v2/...` and append `?Encode=1`.

---

## 3. Auth model (device OAuth2 + machine token)

NOT OpenAI RT/AT. Flow recovered from binary symbols + endpoints:

1. `POST /api/auth/login/generate_nonce` → nonce
2. `POST /api/auth/login/generate_url` → browser login URL
3. Poll `POST /api/v3/user/oauth2/deviceToken/poll` until login completes
4. Login yields `access_token`, `refresh_token`, `userId`,
   `organizationId`/`orgId`, `expires`/`expireTime`.
5. A **machine token** (`getMachineToken`, stored as `machine_token.json`)
   identifies the device → sent as `Cosy-MachineToken`.
6. Refresh: `POST /api/v3/user/refresh_token`.
7. User/quota: `GET /api/v2/user/plan`, `GET /api/v3/user/status`,
   `/api/v3/user/data_region`.

Local credentials in `~/.config/QoderCN/SharedClientCache/cache/user` and
`/quota` are **encrypted** (opaque base64 blobs); `cli/.auth/id` holds a plain
UUID. machineid: `~/.config/QoderCN/machineid`.

---

## 4. Request signing — the "Cosy" scheme

`cosy/remoting.buildRequest` → `addBasicHeaders` + `addBigModelSignatureHeaders`
+ `addBigModelAuthorizationHeaders`. Headers on signed big-model requests:

- `Authorization: Bearer <access_token>` (`BuildBearerTokenRequest`)
- `Cosy-User`, `Cosy-Organization-Id`, `Cosy-Organization-Tags`
- `Cosy-MachineToken`, `Cosy-MachineId`, `Cosy-MachineOS`, `Cosy-MachineType`, `Cosy-MachineCode`
- `Cosy-ClientType: vscode`, `Cosy-Version: <e.g. 2.6.0>`
- `Cosy-Date` (unix-ms), `Cosy-Key` (per-request uuid), `Cosy-SigPath` (path w/o query)
- `Cosy-BodyHash`, `Cosy-BodyLength`, `Cosy-Data-Policy`, `Cosy-fallback-IP`, `Cosy-ClientIp`
- `X-Request-ID`, `X-Model-Name`, `X-Model-Source`, `X-Model-Key`

Signature is an **MD5**, per the binary's verifier debug print
("Signature Components (used for MD5)"):

```
md5( base64(payload) + Cosy-Key + Cosy-Date + body + Cosy-SigPath )
```

`payload`/`body` are base64-encoded when `message_encode=1`; body may be
encrypted when `shouldEncryptBody` is true (`getAppSalt` derives a key; AES is
present in the binary).

### ⚠️ Not yet recovered (needs live capture)
- The exact `getAppSalt` constant / body-encryption key.
- Whether `Cosy-Key` is a literal MD5 component vs an HMAC key.
- The client routes via `https_proxy=http://127.0.0.1:28888` (an existing
  mitmproxy is configured: `~/.mitmproxy/`), so a byte-for-byte confirmation is
  feasible with a live login — but the proxy was **down / not owned by our
  namespace** during this session, so capture is pending.

---

## 5. Chat / inference flow (two-stage)

1. `POST /algo/api/v2/service/invoke/choose_model` (or `/pro/invoke/choose_model`)
   → `{ model, modelName, endpoint, token, bizType, securityToken }` — routes the
   caller to an inference node + short-lived token.
2. Actual generation is **OpenAI-compatible `/chat/completions`** (streaming)
   against the returned inference `endpoint`. Binary contains
   `run node %s/chat/completions` and references DashScope
   `https://dashscope.aliyuncs.com/compatible-mode/v1`.
3. Models: `GET /algo/api/v2/model/list` →
   `{ modelId, modelName, displayName, provider, maxTokens, capabilities }`.
   Observed families: `qwen3-coder`, `qwen-max`, `claude-*`, `gpt-5.4` / `gpt_5_4`,
   `deepseek-*`, `glm-*`.

Quest/agent sessions use `/algo/api/v2/remoteAgent/qoder/...` + SSE
`/api/v2/remoteAgent/sse/qoder/user/events/stream` — **not needed** for an
OpenAI-compatible gateway (only stage 1+2 are).

---

## 6. Mapping codex2api → qoder2api

| codex2api concept | qoder2api equivalent |
|---|---|
| OpenAI RT/AT account | Qoder device-login account (access/refresh/machine token) |
| `auth.RefreshAccessToken` (oauth/token) | `POST /api/v3/user/refresh_token` |
| `CodexBaseURL /responses` | `choose_model` + inference `/chat/completions` |
| `applyCodexRequestHeaders` | `applyQoderCosyHeaders` (Cosy signing) |
| WHAM usage endpoint | `/api/v2/user/plan` quota |

The OpenAI/Anthropic front door, account-pool scheduler, admin dashboard, proxy
pool, API keys, rate limiting, etc. are **reused unchanged**; only the upstream
transport is Qoder-specific. Per direction: **codex-specific paths are being
removed** since the project focuses solely on Qoder.

---

## 7. Work completed so far

- **Repo forked** from `../codex2api` into `qoder2api/` (excluded `.git`,
  `node_modules`, build artifacts).
- **Go module renamed**: `github.com/codex2api` → `github.com/ttimasdf/qoder2api`
  (all 65 import sites + `go.mod`). `auth/` package builds clean under
  `nix shell nixpkgs#go`.
- **`auth/qoder_token.go`** (new): edition endpoint sets (`QoderEndpoints`),
  device login (`StartQoderDeviceLogin` / `PollQoderDeviceLogin`),
  `RefreshQoderToken`, token/account-info parsing.
- **`auth/store.go`** (edited): new `UpstreamQoder` constant; `Account` gains
  `QoderUserID / QoderOrgID / QoderMachineID / QoderMachineToken / QoderClientVer`;
  helpers `IsQoder()` and `QoderCredentials()`.
- **`proxy/qoder.go`** (new): Cosy header builder + MD5 signature
  (`buildQoderCosyRequest` / `applyQoderCosyHeaders` / `computeQoderSignature`),
  `ExecuteQoderRequest` (choose_model → `/chat/completions`),
  `qoderChooseModel`, `ListQoderModels`.
- **`docs/QODER_PROTOCOL.md`**: the full protocol write-up.

---

## 8. Remaining work

- Verify build of `proxy/qoder.go` (needs a `setJSONString` helper or switch to
  `sjson`; `uuid` import availability).
- **Live-capture the Cosy signature** to confirm/fix `computeQoderSignature`
  (salt + body encryption) — the single highest-risk unknown.
- Wire `UpstreamQoder` into the dispatch path (`proxy/handler.go`) so
  `/v1/chat/completions`, `/v1/messages`, `/v1/models` route to `ExecuteQoderRequest`.
- Remove codex-only code paths (`CodexBaseURL`, `/responses`, WHAM usage, WS relay)
  now that the project is Qoder-only.
- Account import UI/admin: device-login flow instead of RT paste.
- Translate Qoder model list → `/v1/models`; map quota → health tiers.
- Rebrand remaining user-facing strings/docs (`Codex` → `Qoder`).
