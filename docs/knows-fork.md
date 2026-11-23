# The Knows fork of mautrix-go

Verified 2026-11-23 against `origin/knows/v0.33.1` @ `235cc3a2`, `origin/knows/v0.32.0` @ `bf44b068`,
`origin/knows/v0.32.1` @ `92aa8e97`, `origin/main` @ `f481d682` and upstream `mautrix/go` `main` @ `c6ca5f41` (fetched
that day). Line numbers below are on `knows/v0.33.1` unless stated.

This repository (`knows-app/mautrix-go-custom`) is a fork of `mautrix/go`, the library every bridge builds on
(`bridgev2` is the bridge framework). It carries a small number of Knows commits on top of upstream releases.

## Branches: where the Knows code is

`main` is **not** the Knows branch. It is an upstream mirror plus this doc: `git log c6ca5f41..origin/main` lists only
the docs commits of PR #5 (`ecbe9d41`, `977ee94e`, `b0d4210f`, merge `f481d682`: `docs/knows-fork.md` and the
`README.md` pointer), and `main` has no Knows code (`git grep -il admission origin/main -- bridgev2` finds nothing). On
2026-11-23 it sits on upstream `bd0b1eef` (2026-11-03), 51 commits behind upstream `main`.

The Knows code lives on release branches named `knows/<upstream version>`. `knows/v0.33.1` is named after the Go
version of its base, `v0.33.1-0.20261119115005-29c00d9a06ea` (upstream tag `v0.33.0` + 10 commits), the mautrix-go
upstream mautrix/meta `main` builds with:

| Branch | Base | Carries | Used by |
|---|---|---|---|
| `knows/v0.33.1` | upstream `main` at `29c00d9a` (2026-11-19) | the five Knows commits of `knows/v0.32.0`, ported by PR #6 (merged 2026-11-23 with a merge commit, `235cc3a2`) | mautrix-meta-custom `develop` (pins `58fd3a44`, same tree as `235cc3a2`) |
| `knows/v0.32.0` | upstream tag `v0.32.0` (`aae5273d`) | customer login admission (PR #1), its config-upgrade fix and startup log (PR #3), DM dead-room recovery (PR #4) | mautrix-meta-custom `main` (prod, pins merge `bf44b068`) |
| `knows/v0.32.1` | upstream `main` at `744963f8` (2026-10-26) | customer login admission only (PR #2, the PR #1 change rebased); **not** the PR #3 or PR #4 fixes | nothing |
| `port/knows-v0.33.1` | `29c00d9a` | the port (five cherry-picks and `58fd3a44`), merged as PR #6 | its tip `58fd3a44` is what mautrix-meta-custom `develop` pins |
| `codex/customer-login-recovery` | `v0.32.0` | the two admission commits, merged as PR #1 | nothing |
| `codex/customer-login-recovery-v0321` | `744963f8` | admission rebased, merged as PR #2 | nothing |
| `fix/login-admission-upgrade` | `knows/v0.32.0` | `1dcbc1a8` config upgrade, `730ec954` startup log, merged as PR #3 | nothing |
| `fix/create-dm-dead-room` | `knows/v0.32.0` | `8d04ddc5` DM dead-room recovery, merged as PR #4 | nothing |

Every other branch on `origin` is an upstream branch (at upstream's tip, at an older tip, or one that upstream has
since deleted), except `docs/refresh-2026-11`, the branch of docs PR #5, merged into `main`.

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

On `knows/v0.33.1` (`git log 29c00d9a..origin/knows/v0.33.1`), the same five commits, cherry-picked in order on
`port/knows-v0.33.1`:

| Commit | Ported from | What |
|---|---|---|
| `626110f9` | `0c5993ff` | customer login admission |
| `34654fb5` | `d387aadf` | `ProviderUserID` |
| `4e9444f5` | `1dcbc1a8` | `login_admission` map kept through the config upgrade |
| `83168510` | `730ec954` | startup log |
| `c3eee4e8` | `8d04ddc5` | DM dead-room recovery |
| `58fd3a44` | (new) | `loginadmission_test.go` imports grouped the way `goimports -local` does |
| `235cc3a2` | | Merge PR #6 |

The port needed one hand resolution. Upstream `71effd07` (in the base) moved the existing-login check of
`User.NewLogin` before the bridge cache lock, so that disconnecting the old client never waits under it, and re-checks
it under the lock (`ErrLoginChangedConcurrently`). On `knows/v0.33.1` the owner is still resolved first, the admission
conflict is refused in that pre-lock check before anything is deleted, and the decision is captured just before
`NewLogin` returns (details below). `bridgeconfig/config.go` keeps upstream's new fields next to `LoginAdmission`;
`matrix/provisioninglogin.go` keeps upstream's `UserMXID` next to `Admission`.

Files changed (`git diff --stat v0.32.0 origin/knows/v0.32.0`; `git diff --stat 29c00d9a origin/knows/v0.33.1` gives
the same list): `bridgev2/loginadmission.go` (new), `bridgev2/portaldeadroom.go` (new), `bridgev2/bridge.go`,
`bridgev2/bridgeconfig/config.go`, `bridgev2/bridgeconfig/upgrade.go`, `bridgev2/login.go`, `bridgev2/userlogin.go`,
`bridgev2/matrix/connector.go`, `bridgev2/matrix/provisioninglogin.go`, `bridgev2/matrix/mxmain/example-config.yaml`,
`bridgev2/provisionutil/resolveidentifier.go`, plus tests.

## Which bridges use the fork

Read from each bridge's `go.mod` on `origin/develop` (dev) and `origin/main` (prod) after `git fetch` on 2026-11-23.
That day the upstream syncs were merged into `develop` only; `main` stays as it is until the full-app test on dev.

| Bridge repo | `develop` (dev) | `main` (prod) |
|---|---|---|
| mautrix-meta-custom | `replace` => `knows-app/mautrix-go-custom` `58fd3a44` (`knows/v0.33.1`) | `replace` => `knows-app/mautrix-go-custom` `bf44b068` (`knows/v0.32.0`, PR #4 merge) |
| mautrix-whatsapp-custom | upstream `33abe66d` copied to `third_party/mautrix-go` and patched with the bridge's own `patches/*.patch` (`patches/apply-bridgev2-patch.sh`) | upstream `4b387c30` (2026-01-29), same copy-and-patch |
| mautrix-linkedin-custom | upstream `33abe66d` | upstream `744963f8` |
| mautrix-twitter-custom | upstream `33abe66d` | upstream `ae411b49` |
| mautrix-telegram-custom | upstream `33abe66d` | upstream `v0.32.0` |
| mautrix-discord-custom | upstream `17e64bf3` | upstream `920bc073` |
| mautrix-signal-custom | upstream `v0.32.0` (no prod shard; its upstream sync is set aside) | upstream `v0.32.0` |
| mautrix-tiktok-custom | upstream `f3fab8d3` (v0.26.5-dev) | no `main` |

So customer login admission and the DM dead-room recovery run only on the Meta bridge (Instagram and Messenger
shards), and both its builds have them. `develop` builds `knows/v0.33.1` since 2026-11-23 (before that `knows/v0.32.0`
@ `bf44b068`, since mautrix-meta-custom PR #76 on 2026-11-14). `main` builds `knows/v0.32.0` @ `bf44b068` since
mautrix-meta-custom PR #89 (2026-11-18), and `bf44b068` contains `8d04ddc5` (`git merge-base --is-ancestor 8d04ddc5
bf44b068`). No other bridge builds the fork (mautrix-linkedin-custom's `pkg/connector/provision_native.go` says it has
no login admission).

## What the fork adds

### 1. Customer login admission (one platform account = one Knows account)

Problem it solves: a platform account (an Instagram login, say) must stay with the Knows account that first
connected it, on the shard that holds it. Stock bridgev2 lets a new login of the same remote account replace or
delete the old one (`DeleteOnConflict`).

How it works (`bridgev2/loginadmission.go`, `bridgev2/userlogin.go`):

1. A network connector proves the provider login and calls `User.NewLogin` (optionally with
   `NewLoginParams.ProviderUserID`, the immutable provider ID when it differs from the login ID, e.g. Instagram
   IGID vs FBID: `userlogin.go:180-183`).
2. First, before `data.UserMXID` is set and before any existing login is looked at, `NewLogin` calls
   `resolveLoginAdmission` (`userlogin.go:203-213`). If admission is not configured, nothing changes.
3. The resolver POSTs to the deployment's callback (`loginadmission.go:101-147`). The answer names the permanent
   owner (`owner_mxid`) and the owner's shard (`shard_id`).
4. If the owner is another Matrix user, the login is created for that owner (`loginadmission.go:177-184`).
5. `NewLogin` then looks up an existing login with this ID, before taking the bridge cache lock. If it belongs to a
   different Matrix user, admission refuses with `LOGIN_ADMISSION_CONFLICT` right there, whatever `DeleteOnConflict`
   says, before the old login is deleted or its client disconnected (`userlogin.go:229-241`). Under the lock,
   upstream's re-check answers `ErrLoginChangedConcurrently` if the login changed meanwhile (`userlogin.go:275-281`).
6. The decision is captured just before `NewLogin` returns (`captureLoginAdmission`, `userlogin.go:323`), in the login
   process context (`WithLoginAdmissionCapture`, `loginadmission.go:195-221`), not in the shared login cache, so two
   concurrent attempts never swap proofs.
7. The provisioning API attaches it to the `complete` step as `complete.login_admission` in `executeStep`
   (`matrix/provisioninglogin.go:549-555`). That is the one place a step is finished, so every `complete` goes
   through it, whether it follows a step POST, a `client_http` answer or a client WebSocket. When admission is
   configured and no decision was captured, the login is deleted with `LOGIN_ADMISSION_UNAVAILABLE`. When a decision
   exists, the stock "override login resulted in a different ID, delete the old one" step is skipped
   (`provisioninglogin.go:589`).

On `knows/v0.32.0` the owner is also resolved first, but everything after it runs under the cache lock (stock v0.32.0
order), and the conflict is refused there (`userlogin.go:241-246` on that branch).

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
- An empty `secret` defaults to the bridge's `appservice.as_token` (`matrix/connector.go:140-142`).
- A config that fails the check makes every new login fail closed. Since `730ec954` (`83168510` on
  `knows/v0.33.1`) the bridge logs `Customer login admission enabled` (platform, shard_id, callback host) or an error
  at startup (`matrix/connector.go:144-151`).
- Since `1dcbc1a8` (`4e9444f5` on `knows/v0.33.1`) the config upgrader keeps a `login_admission` map
  (`bridgeconfig/upgrade.go` `copyNullableMap`). Before it (`d387aadf`, `knows/v0.32.1`) a map in the user's config
  was rewritten to `login_admission: !!map` and startup failed with `cannot unmarshal !!map`.

Callback request (bridge -> knows-api), `POST <url>`, `Authorization: Bearer <secret>`,
`Content-Type: application/json`, 8 s timeout, redirects not followed (`loginadmission.go:70-76`):

```json
{"version": 1, "platform": "instagram", "shard_id": "ig01",
 "candidate_mxid": "@user_<candidate id>:<server>", "login_id": "<bridge login id>", "provider_user_id": "<optional>"}
```

Callback answer, HTTP 200, one JSON object, at most 16 KiB (`loginadmission.go:121-133`):

```json
{"version": 1, "candidate_mxid": "@user_<candidate id>:<server>", "login_id": "<same login id>",
 "owner_mxid": "@user_<owner id>:<server>", "shard_id": "ig01", "proof": "<opaque, at most 8192 chars>"}
```

It is accepted only when `version` is 1, `candidate_mxid` and `login_id` echo the request, `shard_id` and `proof`
are set, and `owner_mxid` is on the same server as the candidate (`validLoginAdmission`, `loginadmission.go:149-157`).
Any other status, a timeout, trailing data or a failed check is `LOGIN_ADMISSION_UNAVAILABLE`.

The receiver is knows-api `customer_login_api.py` (`PATH = '/internal/bridge/customer-login/admit'`); the gateway
reads `complete.login_admission` (`bridge_gateway.py`). Which shards enable it is deployment config: knows-infra
`hosts/bridge-shard/render.py` (`login_admission_yaml`) writes the block, with `url` = the API's internal address +
`/internal/bridge/customer-login/admit`, no `secret` (so the as_token is used), and `allow_insecure_http: true` when
that address is `http://`. Since 2026-11-16 the switch is per platform in `k8s/bridges-base.json`, applied by
`scripts/bridge-shard.py` and overridable per env in `k8s/<env>/bridges/env.json`. On knows-infra `main` @ `ca17eea`
(2026-11-23) it is on for `instagram` and `messenger` with no override, so on dev `ig01`, `fb01` and prod `ig02`,
`fb01` (the API's `BRIDGE_SHARDS` in `k8s/<env>/env.yaml` say the same). Shard 0 (`ig00`) keeps its own config and is
never rendered.

Errors (all `bridgev2.RespError`, written as Matrix-style `{"errcode", "error", ...}`):

| errcode | HTTP | When |
|---|---|---|
| `FI.MAU.BRIDGE.LOGIN_ADMISSION_UNAVAILABLE` | 503 | callback unreachable, non-200, invalid answer, bad config, or no decision at `complete` |
| `FI.MAU.BRIDGE.LOGIN_ADMISSION_CONFLICT` | 409 | the login ID already belongs to another Matrix user on this shard |
| `FI.MAU.BRIDGE.LOGIN_ADMISSION_OTHER_SHARD` | 409 | the owner's shard is not this one; the body also carries `login_admission` (the decision with its proof) so the gateway can continue on the original shard. This shard creates or deletes nothing (`loginadmission.go:136-145`) |

### 2. DM dead-room recovery (`c3eee4e8` on `knows/v0.33.1`, `8d04ddc5` on `knows/v0.32.0`)

`bridgev2/portaldeadroom.go`. When `create_dm` (`provisionutil.ResolveIdentifier` with `createChat`) finds a
portal whose Matrix room no longer exists (deleted by an account deletion or an admin purge), it forgets the room
and creates a new one (`provisionutil/resolveidentifier.go:116-120`). "Gone" needs a definite answer:
`RoomExists` false from a Matrix API that implements it (the Knows messages store), or `M_NOT_FOUND` /
`M_FORBIDDEN` on the room's create event. A transient error keeps the room. Forgetting deletes the portal's old
`message` and `reaction` rows, then clears its `mxid` (`ForgetDeadRoom`, `portaldeadroom.go:51-61`). mautrix-meta-custom
builds it on `develop` and `main` (see "Which bridges use the fork").

### 3. Log hygiene

`LoginStep` logging prints only `completed_user_login_id`, not the whole `complete` object (`login.go:179-181`),
so the admission proof is never logged.

## The provisioning API, as the fork exposes it

Served on the bridge's appservice HTTP listener under `/_matrix/provision/` (`matrix/provisioning.go:191`).
All of this is upstream bridgev2; the fork only adds `login_admission` to the `complete` step. `knows/v0.33.1` has the
upstream v0.33 changes below; what `knows/v0.32.0` (prod Meta) lacks is listed at the end of "Endpoints".

### Auth (`AuthMiddleware`, `provisioning.go:256-312`)

- `Authorization: Bearer <provisioning.shared_secret>` and `?user_id=@user_<id>:<server>` names the Matrix user the
  call acts for. The secret must be at least 16 characters, otherwise every route answers `M_FORBIDDEN`
  "Provisioning API is disabled".
- With `provisioning.allow_matrix_auth: true` the bearer may instead be that user's Matrix access token (checked
  with `/whoami`, cached 5 min), or `openid:<token>` (checked over federation, cached 1 h).
- The user must have login permission (`bridge.permissions`). Request bodies are capped at 64 KiB, except
  `client_http` (10 MiB).
- Missing token: `M_MISSING_TOKEN`; wrong token: `M_UNKNOWN_TOKEN`; unknown route: `M_UNRECOGNIZED`.

### Endpoints (`provisioning.go:123-147`)

All paths are under `/_matrix/provision`. `login_id` (query) picks one of the user's logins; without it the
default login is used, and a user with none gets `FI.MAU.NOT_LOGGED_IN` (400).

A login that exists but is logged out on the network (`login.Client.IsLoggedIn()` false) gets 403 `M_FORBIDDEN`
"Login is not logged in" from `GET /v3/contacts`, `POST /v3/search_users`, `GET /v3/resolve_identifier`,
`POST /v3/create_dm` and `POST /v3/create_group` (upstream `b5763ae4`): `provisionutil` returns
`bridgev2.ErrNotLoggedIn` (`provisionutil/listcontacts.go:27-29,49-51`, `provisionutil/resolveidentifier.go:42-44`,
`provisionutil/creategroup.go:33-35`) and `RespondWithError` writes it (`provisioning.go:470-471`). `POST /v3/profile`
and `GET /v3/poll_results` do the same.

| Method | Path | Purpose |
|---|---|---|
| GET | `/v3/whoami` | network, login flows, homeserver, bridge bot, logins with their last bridge state |
| GET | `/v3/capabilities` | the network's provisioning capabilities |
| GET | `/v3/login/flows` | `{"flows": [{"name", "description", "id"}]}` |
| POST | `/v3/login/start/{flowID}` | start a login; `?client_http=1` makes the phone carry the network's HTTP calls, `?client_websocket=2` its WebSockets too; `?login_id=` re-logs an existing login |
| POST | `/v3/login/step/{loginProcessID}/{stepID}/{stepType}` | submit a step (`user_input`, `cookies`, `webauthn`, `display_and_wait`); `?txn_id=` |
| POST | `/v3/login/step/{loginProcessID}/{stepID}/cancel` | go back from a cancellable step; `?txn_id=` |
| POST | `/v3/login/client_http/{loginProcessID}/{txnID}/{reqID}` | post the response of a relayed HTTP request |
| GET | `/v3/login/client_websocket/{loginProcessID}/{streamID}` | carry a relayed WebSocket (see the relay contract) |
| POST | `/v3/login/cancel/{loginProcessID}` | cancel the whole login process |
| POST | `/v3/logout/{loginID}` | log out one login, or `all` |
| GET | `/v3/logins` | `{"login_ids": [...]}` |
| GET | `/v3/contacts` | the login's contact list; `dm_room_mxid` only for a portal that already exists, since listing no longer creates portal rows (`7a4a86d1`, `GetExistingPortalByKey`, `provisionutil/listcontacts.go:104`) |
| POST | `/v3/search_users` | `{"query": "..."}`; same portal rule as contacts |
| GET | `/v3/resolve_identifier/{identifier}` | resolve a user; 404 `M_NOT_FOUND` when unknown |
| POST | `/v3/create_dm/{identifier}` | resolve and make sure a DM room exists; 201 when just created. Answer: `id`, `name`, `avatar_url`, `identifiers`, `mxid`, `dm_room_mxid` (`provisionutil/resolveidentifier.go:21-32`) |
| POST | `/v3/create_group/{type}` | create a group (`bridgev2.GroupCreateParams` body) |
| POST | `/v3/profile` | update the remote profile (`bridgev2.ProfileUpdateParams` body); `?login_id=` required |
| POST | `/v3/backfill/{roomID}` | manual backfill; needs batch sending and `backfill.queue.manual` |
| POST | `/v3/view_limited_media/{eventID}` | open view-once media |
| GET | `/v3/poll_results/{eventID}` | a poll's results, from the network |
| GET, POST | `/v3/image_pack/import`, GET `/v3/image_pack/list` | sticker packs |
| POST | `/v3/resolve_media/{eventID}` | resolve media |
| DELETE | `/v3/direct_media_keys/{mediaID}` | mark an encrypted direct-media key handled (204) |

`/debug/pprof/*` is served at `/debug/` (not under `/_matrix/provision`) only with
`provisioning.debug_endpoints: true`, shared secret only.

Bridges add their own routes on the same router: mautrix-meta-custom's `/_matrix/provision/v3/selective/...`
(cookie login, contacts, sync-thread, native login) and mautrix-linkedin-custom's native login routes. They are
documented in those repos.

`knows/v0.32.0` (prod Meta) has none of the v0.33 changes above: no `client_websocket`, `profile`, `poll_results` or
`direct_media_keys` route, no gzipped `client_http` bodies (`9d1eb795`), no 403 for a logged-out login (`b5763ae4`),
and listing contacts still creates portal rows (`7a4a86d1`). It still serves `POST /v3/session_transfer/init` and
`/finish` with `provisioning.enable_session_transfers`, which upstream removed (`55a1fd5c`). Upstream `main`
(`c6ca5f41`) has the same routes as `knows/v0.33.1`; its one provisioning change after the base is `028ad639` (see
"Stock login lifecycle").

## Native login: the framework side

The Knows native login (email and password, step by step, with the network's HTTP calls made from the user's
phone) is built from bridgev2's login types. The framework provides the step model, the stock `/v3/login/*` routes
and a relay (`ProvLogin` as `http.RoundTripper`). mautrix-meta-custom and mautrix-linkedin-custom carry the
conversation on their **own** store and routes (`pkg/igconnector/provision_native.go`,
`pkg/connector/provision_native.go`, with their own relay `nativeLoginRelay` implementing
`bridgev2.FingerprintingRoundTripper`), and reuse the framework's types: `LoginStep`, `LoginClientHTTPParams`,
`LoginClientHTTPResponse`, `LoginStartParams.HTTP`. The contract with the app is in mautrix-meta-custom
`docs/native-login.md`.

### Step types (`login.go:114-123`)

`LoginStep.type` is one of, exactly:

| type | Params field | The client |
|---|---|---|
| `user_input` | `user_input.fields[]` (+ `attachments`, `can_cancel`) | shows the fields, posts `{field id: value}` |
| `cookies` | `cookies` (url, user_agent, fields, extract_js, wait_for_url_pattern, ...) | runs a WebView, posts the cookies |
| `client_http` | `client_http` | makes the HTTP request itself and posts the response (the relay) |
| `display_and_wait` | `display_and_wait` (`type`: `qr`, `emoji`, `code`, `nothing`; `data`, `image_url`, `can_cancel`) | shows it, posts with no body and waits |
| `webauthn` | `webauthn` | passkey; refused with `COM.BEEPER.WEBAUTHN_UNSUPPORTED` when `provisioning.fail_on_webauthn` |
| `complete` | `complete` (`user_login_id`, and on the fork `login_admission`) | done |

Every step also has `step_id`, `txn_id` (prefix `bls_`, `provisioninglogin.go:258`) and `instructions`. Input field
`type`s are `username`, `password`, `phone_number`, `email`, `2fa_code`, `token`, `url`, `domain`, `select`
(with `options`), `captcha_code` (`login.go:330-343`).

`select`, `wait`, `pending` are **not** framework step types. They are capability names the app announces to
knows-api in the `X-Knows-Login-Capabilities` header (knows-api `bridge_native_login.py`,
`CAPABILITIES_HEADER`). The framework never reads that header. The bridges' own stores map steps to a
`step_kind` (`form`, `webview`, `webview_script`, `wait`, `client_http`) and answer `202 {"pending": true}` for a
step that takes too long; see the bridge repos.

### Stock login lifecycle (`matrix/provisioninglogin.go`)

- `POST /v3/login/start/{flowID}` creates the process, gives it a 30 min context (`:111`) and returns
  `{"login_id": "<process id>", ...first step}`. The process ID is the `loginProcessID` of later calls.
- `POST .../step/...` validates step ID, `txn_id` and type (`M_BAD_STATE` on mismatch). Posting the previous step's
  `txn_id` again returns the current step (idempotent retry, `:394-402`). So does posting the previous step's ID
  while the current step runs, unless the request carries the current step's `txn_id`: since `c0bcc102` that is a
  retry of the current step (a `display_and_wait` followed by another one with the same `step_id`, say), and it waits
  on the running step (`:423-428`).
- A step error deletes the whole process (`deleteLogin`). A timed-out process answers `FI.MAU.BRIDGE.LOGIN_TIMED_OUT`
  (410), a cancelled one `FI.MAU.BRIDGE.LOGIN_CANCELLED` (410), a finished one `FI.MAU.BRIDGE.LOGIN_ALREADY_FINISHED`
  (410). Too many logins: `FI.MAU.BRIDGE.TOO_MANY_LOGINS` (403).
- A completed process is dropped (`handleCompleteStep`, `:581-600`), so a late retry of its last step gets 404
  `M_NOT_FOUND`. Upstream `main` keeps completed login IDs (`028ad639`, 2026-11-20, not on `knows/v0.33.1`) and
  answers such a retry with a synthetic `complete` step (`fi.mau.bridgev2.already_completed`) that carries no
  `login_admission`.

### Cancellation

- Whole process: `POST /v3/login/cancel/{loginProcessID}` calls `LoginProcess.Cancel()` and drops it. A client that
  disconnects from `login/start` before the first step also cancels it (`:155-160`).
- One step ("back"): `POST /v3/login/step/{id}/{stepID}/cancel` works only for a `user_input` or `display_and_wait`
  step with `can_cancel: true` and a process implementing `LoginProcessStepCancel`. A running `display_and_wait`
  is cancelled with cause `FI.MAU.LOGIN_STEP_CANCELLED` (409, `login.go:23-30`), then `CancelStep` returns the step
  to show (`provisioninglogin.go:240-362`).

### The `client_http` relay contract (`matrix/provisioning_clienthttp.go`, `login.go:184-224`)

Start with `?client_http=1` (`provisioninglogin.go:143-145`). The process then gets `LoginStartParams.HTTP` (the
`ProvLogin`). Each time the connector sends an HTTP request through it, the step in progress returns a `client_http`
step instead of waiting:

```json
{"login_id": "<process id>", "type": "client_http", "step_id": "fi.mau.bridgev2.client_http.1", "txn_id": "bls_...",
 "client_http": {"request_id": "fi.mau.bridgev2.client_http.1", "method": "POST", "url": "https://...",
                 "headers": {"Content-Type": ["..."]}, "body": "<base64>", "fingerprint": "<optional>"}}
```

`headers` is a Go `http.Header` (name -> list of values), `body` is bytes, so base64 in JSON. `request_id`
counts up per process (`provisioning_clienthttp.go:209-210`). The phone makes the request and posts the answer to
`POST /v3/login/client_http/{loginProcessID}/{txn_id}/{request_id}`:

```json
{"status_code": 200, "final_url": "https://... after redirects", "headers": {"Set-Cookie": ["..."]},
 "body": "<base64>", "fingerprint": "<optional>"}
```

or, when the phone could not make it, `{"error": "<text>"}`. Valid means `error` set, or `status_code` 100-599
(`IsValid`). Up to 10 MiB (`:32`, `:67`). The bridge hands the connector a response with `Proto: MauClientHTTP/1.0`;
an `error` becomes a Go error starting `error from client: ` (`:267-269`). The answer to the POST is the next step
(possibly another `client_http`). Posting the same `txn_id`/`request_id` again returns the current step. If the
phone's connection drops while waiting, the bridge answers 499 (`:130-136`). The route checks that the process belongs
to `user_id` (`:61`, upstream `0907c3c7`); on `knows/v0.32.0` it does not.

Gzipped answers (`9d1eb795`): when the step has `accepts_gzip_response: true` (every process unless the bridge runs in
appservice websocket mode, `provisioninglogin.go:126-127`), the phone may post with
`Content-Type: application/vnd.mautrix.client-http-response+gzip`: the JSON object, then the gzipped response body,
which fills `body` (`decodeGzippedClientHTTPResponse`, `provisioning_clienthttp.go:34-51`; 10 MiB once decompressed).

WebSocket tunnel (`0907c3c7`): `?client_websocket=2` on `login/start` (ignored in websocket mode) also gives the
process `LoginStartParams.WebSocketHTTP` (`provisioninglogin.go:146-149`). A WebSocket upgrade the connector sends
through it (GET, `https` only) becomes a `client_http` step with a `stream_id`
(`provisioning_clienthttp.go:165-169,217`). The phone opens the WebSocket itself, posts `status_code` 101 with the
same `stream_id` and no body, and carries the frames as binary messages over
`GET /v3/login/client_websocket/{loginProcessID}/{stream_id}`, attached while that POST is still pending (it answers
only once the connector moves on). A stream is attached once, a process has at most 8, and one not open within a
minute is closed (`provisioning_clientstream.go:38-57,117-157`). Neither mautrix-meta-custom nor
mautrix-linkedin-custom uses the WebSocket tunnel or the gzip format on `develop`.

### Customer admission in the login flow

See "Customer login admission" above: the decision rides `complete.login_admission`, and
`LOGIN_ADMISSION_OTHER_SHARD` (409) carries `login_admission` at the top level of the error body.

## Bridge state reporting: `homeserver.status_endpoint`

`bridgeconfig/homeserver.go:32`. When set (and the bridge is not in websocket mode), every `BridgeState` the bridge
sends is POSTed to that URL with `Authorization: Bearer <appservice.as_token>` and a JSON `BridgeState` body
(`matrix/connector.go:541-558`, `status/bridgestate.go:118-141,177`): `state_event`, `timestamp`, `ttl`, `source`,
`error`, `message`, `user_action`, `user_id`, `remote_id`, `remote_name`, `remote_profile`, `version`, `reason`,
`info`. `CONNECTING` states are **not** posted (`connector.go:551`). Knows points it at knows-api
`/internal/bridge-status?platform=<platform>&shard=<shard id>` (knows-infra
`hosts/bridge-shard/common.config.template.yaml`; knows-api `main.py`, `@app.post(bridge_callbacks.BRIDGE_STATUS)`).
`homeserver.message_send_checkpoint_endpoint` is the per-message equivalent (same token). On `knows/v0.33.1` the POST
uses `http.DefaultClient` (`status/bridgestate.go:200`); upstream `33abe66d` (2026-11-20, after the base) sends it
with the appservice HTTP client.

## Updating the fork

1. Branch from the upstream tag or commit the bridges should move to, and name it `knows/<version>` (`knows/v0.33.1`
   sits on `29c00d9a`, the commit upstream mautrix/meta `main` pins).
2. Cherry-pick the Knows commits listed above on a `port/knows-<version>` branch, keep the tests green
   (`go test ./bridgev2/...`), and merge it with a merge commit (as PR #6 did).
3. In the bridge, `go mod edit -replace=maunium.net/go/mautrix=github.com/knows-app/mautrix-go-custom@<commit>`.

## Not verified

- That the running prod and dev shards match knows-infra's `k8s/bridges-base.json`, `k8s/*/bridges/env.json` and
  `k8s/*/env.yaml` (read in the repo, not on the cluster), and which Meta image each shard runs.
- Whether `knows/v0.33.1` (Meta `develop`) has passed the full-app test on dev; mautrix-meta-custom `main` moves to it
  only after.
- The bridge repos, knows-infra and knows-api were read on their `origin` branches after `git fetch` on 2026-11-23;
  shard 0 (`ig00`, `wa00`) was not looked at.
