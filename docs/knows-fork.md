# The Knows fork of mautrix-go

Verified 2026-11-14 against `origin/knows/v0.32.0` @ `bf44b068`, `origin/knows/v0.32.1` @ `92aa8e97`,
`main` @ `bd0b1eef` and `upstream/main` @ `06ba5a82`. Line numbers below are on `knows/v0.32.0` unless stated.

This repository (`knows-app/mautrix-go-custom`) is a fork of `mautrix/go`, the library every bridge builds on
(`bridgev2` is the bridge framework). It carries a small number of Knows commits on top of upstream releases.

## Branches: where the Knows code is

`main` is **not** the Knows branch. It is an upstream mirror: `git log upstream/main..main` is empty, and `main`
has no Knows code (`git grep -il admission main -- bridgev2` finds nothing). On 2026-11-14 it sits at upstream
`bd0b1eef` (2026-11-03), 27 commits behind `upstream/main`.

The Knows code lives on release branches named `knows/<upstream tag>`:

| Branch | Base | Carries | Used by |
|---|---|---|---|
| `knows/v0.32.0` | upstream tag `v0.32.0` (`aae5273d`) | customer login admission (PR #1), its config-upgrade fix and startup log (PR #3), DM dead-room recovery (PR #4) | mautrix-meta-custom (see below) |
| `knows/v0.32.1` | upstream `main` at `744963f8` (2026-10-26) | customer login admission only (PR #2, the PR #1 change rebased); **not** the PR #3 or PR #4 fixes | no bridge on 2026-11-14 |
| `codex/customer-login-recovery` | `v0.32.0` | the two admission commits, merged as PR #1 | mautrix-meta-custom `main` pins its tip `d387aadf` |
| `codex/customer-login-recovery-v0321` | `744963f8` | admission rebased, merged as PR #2 | nothing directly |
| `fix/login-admission-upgrade` | `knows/v0.32.0` | `1dcbc1a8` config upgrade, `730ec954` startup log, merged as PR #3 | mautrix-meta-custom `develop` pins merge `8015722d` |
| `fix/create-dm-dead-room` | `knows/v0.32.0` | `8d04ddc5` DM dead-room recovery, merged as PR #4 | nothing directly |

Every other branch on `origin` is an upstream branch (same tip as `upstream`, or an upstream branch that upstream
has since deleted).

### Knows commits

`git log v0.32.0..origin/knows/v0.32.0`:

| Commit | What |
|---|---|
| `0c5993ff` | Resolve permanent login ownership before changing bridge sessions (customer login admission) |
| `d387aadf` | Keep the verified provider identity (`ProviderUserID`) separate from the bridge login ID |
| `e2b2c099` | Merge PR #1 |
| `1dcbc1a8` | `bridgeconfig`: keep a `login_admission` map through the config upgrade |
| `730ec954` | Log at startup whether customer login admission is enabled |
| `8015722d` | Merge PR #3 |
| `8d04ddc5` | Re-create a DM portal's room when it was deleted behind the bridge |
| `bf44b068` | Merge PR #4 |

On `knows/v0.32.1` (`git log 744963f8..origin/knows/v0.32.1`): `4cb852c3` (admission, rebased) and merge `92aa8e97`.

Files changed (`git diff --stat v0.32.0 origin/knows/v0.32.0`): `bridgev2/loginadmission.go` (new),
`bridgev2/portaldeadroom.go` (new), `bridgev2/bridge.go`, `bridgev2/bridgeconfig/config.go`,
`bridgev2/bridgeconfig/upgrade.go`, `bridgev2/login.go`, `bridgev2/userlogin.go`, `bridgev2/matrix/connector.go`,
`bridgev2/matrix/provisioninglogin.go`, `bridgev2/matrix/mxmain/example-config.yaml`,
`bridgev2/provisionutil/resolveidentifier.go`, plus tests.

## Which bridges use the fork

Read from each bridge's `go.mod` on 2026-11-14 (`origin/main` and `origin/develop` give the same answer except for
Meta).

| Bridge repo | mautrix-go it builds | Knows fork? |
|---|---|---|
| mautrix-meta-custom `develop` | `replace` => `knows-app/mautrix-go-custom` `8015722d` (PR #3 merge on `knows/v0.32.0`) | yes: admission + upgrade fix, no dead-room recovery |
| mautrix-meta-custom `main` | `replace` => `knows-app/mautrix-go-custom` `d387aadf` (tip of `codex/customer-login-recovery`) | yes: admission, without the upgrade fix and startup log |
| mautrix-whatsapp-custom | upstream `4b387c30` (2026-01-29) copied to `third_party/mautrix-go` and patched with the bridge's own `patches/*.patch` (`patches/apply-bridgev2-patch.sh`) | no |
| mautrix-linkedin-custom | upstream `744963f8` | no (its `pkg/connector/provision_native.go` says it has no login admission) |
| mautrix-twitter-custom | upstream `ae411b49` | no |
| mautrix-discord-custom | upstream `920bc073` | no |
| mautrix-signal-custom | upstream `v0.32.0` | no |
| mautrix-telegram-custom | upstream `v0.32.0` | no |

So customer login admission can only run on the Meta bridge (Instagram and Messenger shards). No bridge builds
the DM dead-room recovery (`8d04ddc5`) yet.

## What the fork adds

### 1. Customer login admission (one platform account = one Knows account)

Problem it solves: a platform account (an Instagram login, say) must stay with the Knows account that first
connected it, on the shard that holds it. Stock bridgev2 lets a new login of the same remote account replace or
delete the old one (`DeleteOnConflict`).

How it works (`bridgev2/loginadmission.go`, `bridgev2/userlogin.go`):

1. A network connector proves the provider login and calls `User.NewLogin` (optionally with
   `NewLoginParams.ProviderUserID`, the immutable provider ID when it differs from the login ID, e.g. Instagram
   IGID vs FBID: `userlogin.go:180-183`).
2. Before taking the bridge cache lock, `NewLogin` calls `resolveLoginAdmission` (`userlogin.go:200-213`). If
   admission is not configured, nothing changes.
3. The resolver POSTs to the deployment's callback (`loginadmission.go:101-147`). The answer names the permanent
   owner (`owner_mxid`) and the owner's shard (`shard_id`).
4. If the owner is another Matrix user, the login is created for that owner (`loginadmission.go:177-186`).
5. If an existing login with this ID belongs to a different Matrix user, admission refuses with
   `LOGIN_ADMISSION_CONFLICT` and never deletes the old login (`userlogin.go:240-246`).
6. The decision is stored in the login process context (`WithLoginAdmissionCapture`,
   `loginadmission.go:195-221`), not in the shared login cache, so two concurrent attempts never swap proofs.
7. The provisioning API attaches it to the `complete` step as `complete.login_admission`
   (`matrix/provisioninglogin.go:533-539`). When admission is configured and no decision was captured, the login
   is deleted with `LOGIN_ADMISSION_UNAVAILABLE`. When a decision exists, the stock "override login resulted in a
   different ID, delete the old one" step is skipped (`provisioninglogin.go:573`).

Configuration (`bridgeconfig/config.go:67-79`, `matrix/mxmain/example-config.yaml`), default `null` (off):

```yaml
bridge:
    login_admission:
        url: https://<knows-api host>/internal/bridge/customer-login/admit
        platform: instagram
        shard_id: ig01
        secret: <optional, defaults to appservice.as_token>
        allow_insecure_http: false
```

- `url` must be absolute, without credentials, query or fragment; `https`, or `http` only on loopback or with
  `allow_insecure_http: true` (`CheckLoginAdmissionConfig`, `loginadmission.go:80-99`).
- An empty `secret` defaults to the bridge's `appservice.as_token` (`matrix/connector.go:127-143`).
- A config that fails the check makes every new login fail closed. Since `730ec954` the bridge logs
  `Customer login admission enabled` (platform, shard_id, callback host) or an error at startup.
- Since `1dcbc1a8` the config upgrader keeps a `login_admission` map (`bridgeconfig/upgrade.go`
  `copyNullableMap`). Before it (`d387aadf`, `knows/v0.32.1`) a map in the user's config was rewritten to
  `login_admission: !!map` and startup failed with `cannot unmarshal !!map`.

Callback request (bridge -> knows-api), `POST <url>`, `Authorization: Bearer <secret>`,
`Content-Type: application/json`, 8 s timeout, redirects not followed (`loginadmission.go:70-77`):

```json
{"version": 1, "platform": "instagram", "shard_id": "ig01",
 "candidate_mxid": "@user_123:<server>", "login_id": "<bridge login id>", "provider_user_id": "<optional>"}
```

Callback answer, HTTP 200, one JSON object, at most 16 KiB (`loginadmission.go:121-133`):

```json
{"version": 1, "candidate_mxid": "@user_123:<server>", "login_id": "<same login id>",
 "owner_mxid": "@user_45:<server>", "shard_id": "ig01", "proof": "<opaque, at most 8192 chars>"}
```

It is accepted only when `version` is 1, `candidate_mxid` and `login_id` echo the request, `shard_id` and `proof`
are set, and `owner_mxid` is on the same server as the candidate (`validLoginAdmission`, `loginadmission.go:149-157`).
Any other status, a timeout, trailing data or a failed check is `LOGIN_ADMISSION_UNAVAILABLE`.

The receiver is knows-api `customer_login_api.py` (`PATH = '/internal/bridge/customer-login/admit'`); the gateway
reads `complete.login_admission` (`bridge_gateway.py`). Which shards enable it is deployment config, not verified here.

Errors (all `bridgev2.RespError`, written as Matrix-style `{"errcode", "error", ...}`):

| errcode | HTTP | When |
|---|---|---|
| `FI.MAU.BRIDGE.LOGIN_ADMISSION_UNAVAILABLE` | 503 | callback unreachable, non-200, invalid answer, bad config, or no decision at `complete` |
| `FI.MAU.BRIDGE.LOGIN_ADMISSION_CONFLICT` | 409 | the login ID already belongs to another Matrix user on this shard |
| `FI.MAU.BRIDGE.LOGIN_ADMISSION_OTHER_SHARD` | 409 | the owner's shard is not this one; the body also carries `login_admission` (the decision with its proof) so the gateway can continue on the original shard. This shard creates or deletes nothing (`loginadmission.go:134-145`) |

### 2. DM dead-room recovery (`8d04ddc5`, on `knows/v0.32.0` only)

`bridgev2/portaldeadroom.go`. When `create_dm` (`provisionutil.ResolveIdentifier` with `createChat`) finds a
portal whose Matrix room no longer exists (deleted by an account deletion or an admin purge), it forgets the room
and creates a new one (`provisionutil/resolveidentifier.go:113-117`). "Gone" needs a definite answer:
`RoomExists` false from a Matrix API that implements it (the Knows messages store), or `M_NOT_FOUND` /
`M_FORBIDDEN` on the room's create event. A transient error keeps the room. Forgetting deletes the portal's old
`message` and `reaction` rows, then clears its `mxid` (`ForgetDeadRoom`, `portaldeadroom.go:51-62`). No bridge
builds this commit yet.

### 3. Log hygiene

`LoginStep` logging prints only `completed_user_login_id`, not the whole `complete` object (`login.go:178-180`),
so the admission proof is never logged.

## The provisioning API, as the fork exposes it

Served on the bridge's appservice HTTP listener under `/_matrix/provision/` (`matrix/provisioning.go:199`).
All of this is upstream bridgev2; the fork only adds `login_admission` to the `complete` step.

### Auth (`AuthMiddleware`, `provisioning.go:264-326`)

- `Authorization: Bearer <provisioning.shared_secret>` and `?user_id=@user_<id>:<server>` names the Matrix user the
  call acts for. The secret must be at least 16 characters, otherwise every route answers `M_FORBIDDEN`
  "Provisioning API is disabled".
- With `provisioning.allow_matrix_auth: true` the bearer may instead be that user's Matrix access token (checked
  with `/whoami`, cached 5 min), or `openid:<token>` (checked over federation, cached 1 h).
- The user must have login permission (`bridge.permissions`). Request bodies are capped at 64 KiB, except
  `client_http` (10 MiB).
- Missing token: `M_MISSING_TOKEN`; wrong token: `M_UNKNOWN_TOKEN`; unknown route: `M_UNRECOGNIZED`.

### Endpoints (`provisioning.go:129-154`)

All paths are under `/_matrix/provision`. `login_id` (query) picks one of the user's logins; without it the
default login is used, and a user with none gets `FI.MAU.NOT_LOGGED_IN` (400).

| Method | Path | Purpose |
|---|---|---|
| GET | `/v3/whoami` | network, login flows, homeserver, bridge bot, logins with their last bridge state |
| GET | `/v3/capabilities` | the network's provisioning capabilities |
| GET | `/v3/login/flows` | `{"flows": [{"name", "description", "id"}]}` |
| POST | `/v3/login/start/{flowID}` | start a login; `?client_http=1` makes the phone carry the network's HTTP calls; `?login_id=` re-logs an existing login |
| POST | `/v3/login/step/{loginProcessID}/{stepID}/{stepType}` | submit a step (`user_input`, `cookies`, `webauthn`, `display_and_wait`); `?txn_id=` |
| POST | `/v3/login/step/{loginProcessID}/{stepID}/cancel` | go back from a cancellable step; `?txn_id=` |
| POST | `/v3/login/client_http/{loginProcessID}/{txnID}/{reqID}` | post the response of a relayed HTTP request |
| POST | `/v3/login/cancel/{loginProcessID}` | cancel the whole login process |
| POST | `/v3/logout/{loginID}` | log out one login, or `all` |
| GET | `/v3/logins` | `{"login_ids": [...]}` |
| GET | `/v3/contacts` | the login's contact list |
| POST | `/v3/search_users` | `{"query": "..."}` |
| GET | `/v3/resolve_identifier/{identifier}` | resolve a user; 404 `M_NOT_FOUND` when unknown |
| POST | `/v3/create_dm/{identifier}` | resolve and make sure a DM room exists; 201 when just created. Answer: `id`, `name`, `avatar_url`, `identifiers`, `mxid`, `dm_room_mxid` (`provisionutil/resolveidentifier.go:21-32`) |
| POST | `/v3/create_group/{type}` | create a group (`bridgev2.GroupCreateParams` body) |
| POST | `/v3/backfill/{roomID}` | manual backfill; needs batch sending and `backfill.queue.manual` |
| POST | `/v3/view_limited_media/{eventID}` | open view-once media |
| GET, POST | `/v3/image_pack/import`, GET `/v3/image_pack/list` | sticker packs |
| POST | `/v3/resolve_media/{eventID}` | resolve media |
| POST | `/v3/session_transfer/init`, `/v3/session_transfer/finish` | only with `provisioning.enable_session_transfers` |

`/debug/pprof/*` is served at `/debug/` (not under `/_matrix/provision`) only with
`provisioning.debug_endpoints: true`, shared secret only.

Bridges add their own routes on the same router: mautrix-meta-custom's `/_matrix/provision/v3/selective/...`
(cookie login, contacts, sync-thread, native login) and mautrix-linkedin-custom's native login routes. They are
documented in those repos.

Upstream `main` has more routes than `knows/v0.32.0`: `GET /v3/login/client_websocket/{loginProcessID}/{streamID}`,
`POST /v3/profile`, `GET /v3/poll_results/{eventID}`, `DELETE /v3/direct_media_keys/{mediaID}`, and gzipped
`client_http` bodies (`9d1eb795`, also on `knows/v0.32.1`).

## Native login: the framework side

The Knows native login (email and password, step by step, with the network's HTTP calls made from the user's
phone) is built from bridgev2's login types. The framework provides the step model, the stock `/v3/login/*` routes
and a relay (`ProvLogin` as `http.RoundTripper`). mautrix-meta-custom and mautrix-linkedin-custom carry the
conversation on their **own** store and routes (`pkg/igconnector/provision_native.go`,
`pkg/connector/provision_native.go`, with their own relay `nativeLoginRelay` implementing
`bridgev2.FingerprintingRoundTripper`), and reuse the framework's types: `LoginStep`, `LoginClientHTTPParams`,
`LoginClientHTTPResponse`, `LoginStartParams.HTTP`. The contract with the app is in mautrix-meta-custom
`docs/native-login.md`.

### Step types (`login.go:113-122`)

`LoginStep.type` is one of, exactly:

| type | Params field | The client |
|---|---|---|
| `user_input` | `user_input.fields[]` (+ `attachments`, `can_cancel`) | shows the fields, posts `{field id: value}` |
| `cookies` | `cookies` (url, user_agent, fields, extract_js, wait_for_url_pattern, ...) | runs a WebView, posts the cookies |
| `client_http` | `client_http` | makes the HTTP request itself and posts the response (the relay) |
| `display_and_wait` | `display_and_wait` (`type`: `qr`, `emoji`, `code`, `nothing`; `data`, `image_url`, `can_cancel`) | shows it, posts with no body and waits |
| `webauthn` | `webauthn` | passkey; refused with `COM.BEEPER.WEBAUTHN_UNSUPPORTED` when `provisioning.fail_on_webauthn` |
| `complete` | `complete` (`user_login_id`, and on the fork `login_admission`) | done |

Every step also has `step_id`, `txn_id` (prefix `bls_`, `provisioninglogin.go:242`) and `instructions`. Input field
`type`s are `username`, `password`, `phone_number`, `email`, `2fa_code`, `token`, `url`, `domain`, `select`
(with `options`), `captcha_code` (`login.go:321-334`).

`select`, `wait`, `pending` are **not** framework step types. They are capability names the app announces to
knows-api in the `X-Knows-Login-Capabilities` header (knows-api `bridge_native_login.py`,
`CAPABILITIES_HEADER`). The framework never reads that header. The bridges' own stores map steps to a
`step_kind` (`form`, `webview`, `webview_script`, `wait`, `client_http`) and answer `202 {"pending": true}` for a
step that takes too long; see the bridge repos.

### Stock login lifecycle (`matrix/provisioninglogin.go`)

- `POST /v3/login/start/{flowID}` creates the process, gives it a 30 min context (`:105`) and returns
  `{"login_id": "<process id>", ...first step}`. The process ID is the `loginProcessID` of later calls.
- `POST .../step/...` validates step ID, `txn_id` and type (`M_BAD_STATE` on mismatch); posting the previous
  step's ID or `txn_id` again returns the current step (idempotent retry, `:378-386`).
- A step error deletes the whole process (`deleteLogin`). A timed-out process answers `FI.MAU.BRIDGE.LOGIN_TIMED_OUT`
  (410), a cancelled one `FI.MAU.BRIDGE.LOGIN_CANCELLED` (410), a finished one `FI.MAU.BRIDGE.LOGIN_ALREADY_FINISHED`
  (410). Too many logins: `FI.MAU.BRIDGE.TOO_MANY_LOGINS` (403).

### Cancellation

- Whole process: `POST /v3/login/cancel/{loginProcessID}` calls `LoginProcess.Cancel()` and drops it. A client that
  disconnects from `login/start` before the first step also cancels it (`:139-144`).
- One step ("back"): `POST /v3/login/step/{id}/{stepID}/cancel` works only for a `user_input` or `display_and_wait`
  step with `can_cancel: true` and a process implementing `LoginProcessStepCancel`. A running `display_and_wait`
  is cancelled with cause `FI.MAU.LOGIN_STEP_CANCELLED` (409, `login.go:23-30`), then `CancelStep` returns the step
  to show (`provisioninglogin.go:246-346`).

### The `client_http` relay contract (`matrix/provisioning_clienthttp.go`, `login.go:183-216`)

Start with `?client_http=1`. The process then gets `LoginStartParams.HTTP` (the `ProvLogin`). Each time the
connector sends an HTTP request through it, the step in progress returns a `client_http` step instead of waiting:

```json
{"login_id": "<process id>", "type": "client_http", "step_id": "fi.mau.bridgev2.client_http.1", "txn_id": "bls_...",
 "client_http": {"request_id": "fi.mau.bridgev2.client_http.1", "method": "POST", "url": "https://...",
                 "headers": {"Content-Type": ["..."]}, "body": "<base64>", "fingerprint": "<optional>"}}
```

`headers` is a Go `http.Header` (name -> list of values), `body` is bytes, so base64 in JSON. `request_id`
counts up per process (`:164`). The phone makes the request and posts the answer to
`POST /v3/login/client_http/{loginProcessID}/{txn_id}/{request_id}`:

```json
{"status_code": 200, "final_url": "https://... after redirects", "headers": {"Set-Cookie": ["..."]},
 "body": "<base64>", "fingerprint": "<optional>"}
```

or, when the phone could not make it, `{"error": "<text>"}`. Valid means `error` set, or `status_code` 100-599
(`IsValid`). Up to 10 MiB (`:46`). The bridge hands the connector a response with `Proto: MauClientHTTP/1.0`;
an `error` becomes a Go error starting `error from client: ` (`:245-249`). The answer to the POST is the next step
(possibly another `client_http`). Posting the same `txn_id`/`request_id` again returns the current step. If the
phone's connection drops while waiting, the bridge answers 499. On `knows/v0.32.0` the route does not check that
the process belongs to `user_id` (upstream `main` does).

### Customer admission in the login flow

See "Customer login admission" above: the decision rides `complete.login_admission`, and
`LOGIN_ADMISSION_OTHER_SHARD` (409) carries `login_admission` at the top level of the error body.

## Bridge state reporting: `homeserver.status_endpoint`

`bridgeconfig/homeserver.go:32`. When set (and the bridge is not in websocket mode), every `BridgeState` the bridge
sends is POSTed to that URL with `Authorization: Bearer <appservice.as_token>` and a JSON `BridgeState` body
(`matrix/connector.go:534-550`, `status/bridgestate.go:116-136,169`): `state_event`, `timestamp`, `ttl`, `source`,
`error`, `message`, `user_action`, `user_id`, `remote_id`, `remote_name`, `remote_profile`, `version`, `reason`,
`info`. `CONNECTING` states are **not** posted (`connector.go:544`). Knows points it at knows-api
`/internal/bridge-status?platform=<platform>` (knows-api `main.py`, the `bridge-status` route). `homeserver.message_send_checkpoint_endpoint` is the per-message
equivalent (same token).

## Updating the fork

1. Branch from the upstream tag the bridges should move to, name it `knows/<tag>`.
2. Cherry-pick the Knows commits listed above, keep the tests green (`go test ./bridgev2/...`).
3. In the bridge, `go mod edit -replace=maunium.net/go/mautrix=github.com/knows-app/mautrix-go-custom@<commit>`.

## Not verified

- Which shards set `bridge.login_admission` in production (deployment config, knows-infra / knows-api
  `bridge_shards.py`).
- Whether mautrix-meta-custom `main` (pinned at `d387aadf`) starts with a `login_admission` map in its config,
  given the config-upgrade bug fixed in `1dcbc1a8`.
- The bridge repos' `origin` refs were read as present in the local clones on 2026-11-14, not re-fetched.
