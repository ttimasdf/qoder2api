# Qoder CN / Qoder upstream protocol (reverse-engineered)

This document records what was extracted from the bundled Go backend
(`.../resources/app/resources/bin/x86_64_linux/QoderCN`, Go `cosy/*` packages),
the `aicoding-agent` VS Code extension, and the running client's
`~/.config/QoderCN/SharedClientCache` state. It is the basis for the qoder2api
upstream layer (`proxy/` + `auth/`).

## Editions / endpoints

The backend embeds a `remote_config` block. Two editions exist:

| Field | Qoder (intl.) | Qoder CN (Aliyun RDC) |
|---|---|---|
| big_model_endpoint | `https://center.qoder.sh/algo` | `https://qoder.com.cn` (`endpoint` in `cache/app-config.json`) |
| infer_api_endpoint | `https://api3.qoder.sh` | (region-routed via RDC) |
| open_api_endpoint | `https://openapi.qoder.sh` | |
| quest_server_endpoint | `https://qts2.qoder.sh` | |
| login_url | `https://www.qoder.com/device/selectAccounts` | `https://devops.aliyun.com/lingma/login` |
| message_encode | `1` | `1` |
| login_encode | `2` | `2` |

All algo API calls are under `<endpoint>/algo/api/v2/...` (note the `/algo` prefix
and a `?Encode=1` query param added by `shouldAddEncodeParam`).

## Auth

Device OAuth2 + machine token, NOT OpenAI-style RT/AT.

1. `POST /api/auth/login/generate_nonce` -> nonce
2. `POST /api/auth/login/generate_url`  -> browser login URL (open in browser)
3. Poll `POST /api/v3/user/oauth2/deviceToken/poll` until login completes
4. Login response yields `access_token`, `refresh_token`, `userId`,
   `organizationId`/`orgId`, `expires`/`expireTime`.
5. A machine token (`getMachineToken`, stored as `machine_token.json`) identifies
   the device. Sent as header `Cosy-MachineToken`.
6. Refresh via `POST /api/v3/user/refresh_token`.
7. User/quota: `GET /api/v2/user/plan`, `GET /api/v3/user/status`,
   `/api/v3/user/data_region`.

## Request signing ("Cosy" scheme)

> **Superseded / corrected:** the section below describes the IDE daemon
> (`QoderCN`, `cosy/remoting`) headers as first observed from strings. The
> **authoritative, statically-verified** signing algorithm (from the standalone
> `qodercncli`) is in **[QODER_SIGNING.md](QODER_SIGNING.md)** and is what the
> fork implements:
>
> ```
> date      = time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
> Signature = hex(md5("cosy" + "d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw==" + date))
> ```
>
> The body, request path, and a body hash do NOT participate. Header name is
> `Signature`. `Cosy-Date` is unix seconds; the signed `Date` is RFC1123 GMT.

`cosy/remoting.buildRequest` -> `addBasicHeaders` + `addBigModelSignatureHeaders`
+ `addBigModelAuthorizationHeaders`.

Headers sent on signed big-model requests:

- `Authorization: Bearer <access_token>`  (`BuildBearerTokenRequest`)
- `Cosy-User: <userId>`
- `Cosy-MachineToken: <machine token>`
- `Cosy-MachineId`, `Cosy-MachineOS`, `Cosy-MachineType`, `Cosy-MachineCode`
- `Cosy-ClientType: vscode`
- `Cosy-Version: <client version, e.g. 2.6.0>`
- `Cosy-Organization-Id`, `Cosy-Organization-Tags`
- `Cosy-Date: <unix-ms timestamp>`
- `Cosy-Key: <per-request key (uuid)>`
- `Cosy-SigPath: <request path, query trimmed by trimQueryPath>`
- `Cosy-BodyHash`, `Cosy-BodyLength`
- `X-Request-ID`, `X-Model-Name`, `X-Model-Source`, `X-Model-Key`
- `Cosy-Data-Policy`, `Cosy-fallback-IP`, `Cosy-ClientIp`

Signature is an **MD5** over the concatenation (from the verifier's debug print
"Signature Components (used for MD5)"):

```
md5( base64(payload) + Cosy-Key + Cosy-Date + body + Cosy-SigPath )
```

where `payload` is the request body (base64-encoded if `message_encode=1`), and
`body` may be encrypted when `shouldEncryptBody` is true (`getAppSalt` derives a
key; AES is present in the binary). `isCompleteUTF8` is checked on the body.

> NOT YET RECOVERED from static analysis: the exact `getAppSalt` constant / body
> encryption key, and whether `Cosy-Key` participates as the MD5 salt directly.
> These need a live capture (mitmproxy on `127.0.0.1:28888`, which the client is
> already configured to use via `https_proxy`) to confirm byte-for-byte.

## Chat / inference flow

The chat is two-stage:

1. `POST /algo/api/v2/service/invoke/choose_model` (or `/pro/invoke/choose_model`)
   returns `{ model, modelName, endpoint, token, bizType, securityToken }` —
   i.e. it routes the caller to an inference node + short-lived token.
2. The actual generation is an OpenAI-compatible **`/chat/completions`** stream
   against the returned inference `endpoint` (the binary contains
   `run node %s/chat/completions` and an OpenAI-compatible client; DashScope
   `https://dashscope.aliyuncs.com/compatible-mode/v1` is also referenced).
3. Models: `GET /algo/api/v2/model/list` -> `{ modelId, modelName, displayName,
   provider, maxTokens, capabilities }`. Observed model families: `qwen3-coder`,
   `qwen-max`, `claude-*`, `gpt-5.4` / `gpt_5_4`, `deepseek-*`, `glm-*`.

Quest/agent sessions use `/algo/api/v2/remoteAgent/qoder/...` and SSE via
`/api/v2/remoteAgent/sse/qoder/user/events/stream`. The OpenAI-compatible gateway
only needs stage 1+2 (choose_model + chat/completions), not the quest APIs.

## Mapping onto the codex2api architecture

| codex2api concept | qoder2api equivalent |
|---|---|
| OpenAI RT/AT account | Qoder device-login account (access/refresh/machine token) |
| `auth.RefreshAccessToken` (oauth/token) | `POST /api/v3/user/refresh_token` |
| `CodexBaseURL` `/responses` | `<big_model_endpoint>/algo/api/v2/service/invoke/choose_model` + inference `/chat/completions` |
| Codex request headers (`applyCodexRequestHeaders`) | Cosy signed headers (`applyQoderRequestHeaders`) |
| WHAM usage endpoint | `/api/v2/user/plan` quota |
| plan_type / health tiers | Qoder plan + quota windows |

The OpenAI/Anthropic-compatible front door, account pool scheduler, admin
dashboard, proxy pool, API keys, rate limiting, etc. are reused unchanged; only
the upstream transport (`proxy/executor.go`, `proxy/translator.go`,
`auth/token.go`) is qoder-specific.
