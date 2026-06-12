# Qoder Cosy signing — verified via Binary Ninja static analysis

Source: `qodercncli` (standalone CLI, Go), package
`code.alibaba-inc.com/qoder-core/qodercli/core/utils/qoder`. Recovered from the
saved analysis DB `docs/re/qodercncli.bndb` (Go pclntab names in
`docs/re/gofuncs.json`).

## Call graph

`buildRequest` (0x8935e0) →
- `addBigModelAuthorizationHeaders` (0x88d2e0)
- `addBigModelSignatureHeaders`     (0x88cf00)

`encrypt.Md5Encode` (0x7d3420) = `hex(md5(strings.Join(parts, "")))`.

## addBigModelSignatureHeaders — the signature

```
date := time.Now().Format("Mon, 02 Jan 2006 15:04:05 GMT")   // RFC1123 GMT, len 29
req.Header.Set("Date", date)
sig  := Md5Encode([]string{ "cosy", "d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw==", date })
//       == hex(md5("cosy" + "d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw==" + date))
req.Header.Set("Signature", sig)
req.Header.Set("Cosy-User", <userId>)
req.Header.Set("Cosy-Date", <unix-seconds>)   // also set here / in auth headers
```

Key facts (these correct the earlier guesses in QODER_PROTOCOL.md):

- The signature is **MD5 of three concatenated strings**: the literal `"cosy"`,
  a constant salt, and the **RFC1123 GMT `Date`** value. No body, no path, no
  body hash participate.
- The salt is the **literal base64 string** `d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw==`,
  used verbatim (NOT base64-decoded). It decodes to the easter egg
  "war, war never changes" but the bytes fed to MD5 are the 32-char base64 text.
- Header name is **`Signature`** (not `Cosy-Sign`).

## addBigModelAuthorizationHeaders

Sets, via the auth service (`qoder.GetAuthService`):

- `Authorization: Bearer <access_token>`
- `Cosy-User: <userId>`
- `Cosy-Date: <unix seconds>`   (`fmt.Sprintf("%d", time.Now().Unix())`)
- `Cosy-Key: <key>`

## Body

The big-model request body is sent as-is (the inference call is OpenAI-compatible
`/chat/completions`). The `?Encode=1` query param and any base64/`Cosy-BodyHash`
seen in the IDE daemon (`QoderCN`) are NOT part of the CLI's signed big-model
request path. The signature does not cover the body at all.

## Auth / login (CLI, no IDE required)

- `generatePKCEChallenge` (0x8862a0): standard PKCE (code_verifier/code_challenge,
  S256).
- `PollDeviceToken` (0x86dc20) → `POST <openapi>/api/v1/deviceToken/poll`.
- `RefreshDeviceToken` (0x86d0e0) → refresh flow.
- Login methods: browser (PKCE device flow), `LINGMA_YUNXIAO_TOKEN` env, personal
  access token, or IDE sync.

## Reusing the analysis DB

```python
import binaryninja as bn, os
with open(os.path.expanduser("~/.binaryninja/license.dat")) as f:
    bn.core_set_license(f.read())
bv = bn.load("docs/re/qodercncli.bndb")   # opens in ~8s, 77k functions
```

Function name→address map: `docs/re/gofuncs.json` (parsed from `.gopclntab`).
