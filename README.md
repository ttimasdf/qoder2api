# Qoder2API

**Turn a pool of Qoder / Qoder CN accounts into an observable, schedulable, OpenAI-compatible gateway.**

Qoder2API is a fork of [codex2api](../codex2api) re-targeted at the Qoder / Qoder CN
upstream (the cloud backend behind the QoderCN IDE). It reuses codex2api's account-pool
scheduler, admin dashboard, proxy pool, API keys, prompt filtering, rate limiting, and
usage tracking, but replaces the upstream transport with the Qoder "Cosy" signed protocol.

## What changed vs codex2api

- **Single downstream API**: only `POST /v1/chat/completions` and `GET /v1/models` are
  exposed. An external AI gateway handles any chat-interface conversion, so the Codex
  Responses API, Anthropic `/v1/messages`, Images, and the WebSocket relay were removed.
- **Single upstream**: every request is signed with the Qoder Cosy scheme and routed
  through `choose_model` to an inference node's OpenAI-compatible `/chat/completions`
  endpoint. There is no protocol translation in the proxy — chat/completions in,
  chat/completions out (passthrough).
- **Auth**: accounts authenticate with Qoder device OAuth (access/refresh token) plus a
  machine token, instead of OpenAI Refresh/Access tokens.

See [docs/QODER_PROTOCOL.md](docs/QODER_PROTOCOL.md) for the reverse-engineered protocol
details (endpoints, Cosy signing, choose_model flow, editions).

## Editions

| Edition | big_model endpoint | login |
|---|---|---|
| `intl` (qoder.com) | `https://center.qoder.sh/algo` | `https://www.qoder.com/device/selectAccounts` |
| `cn` (default) | `https://qoder.com.cn` | `https://devops.aliyun.com/lingma/login` |

Select via `proxy.QoderEdition` (default `cn`).

## Build & test

```bash
go build ./...
go test ./...
```

## Status

The pool/admin/scheduler stack is fully functional and the Qoder upstream client
(Cosy signing + choose_model + chat/completions passthrough + plan-based quota probe)
is implemented and unit-tested against a mock upstream.

> Not yet confirmed against a live account: the exact `getAppSalt` body-encryption key
> and whether `Cosy-Key` participates differently in the MD5 salt. The current signing
> implementation follows the backend's debug-print spec
> (`MD5(base64(body)+Cosy-Key+Cosy-Date+body+Cosy-SigPath)`); confirm byte-for-byte with
> a live capture (the client already proxies through `127.0.0.1:28888`). See
> docs/QODER_PROTOCOL.md.

---

<details>
<summary>Original codex2api documentation (architecture, deployment, admin dashboard)</summary>

The sections below describe the inherited codex2api infrastructure. Account-pool
scheduling, the React/Vite admin dashboard, PostgreSQL/SQLite + Redis/memory deployment
shapes, proxy pools, API keys, prompt filtering, and usage analytics all apply unchanged.

</details>
