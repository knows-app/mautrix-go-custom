// Copyright (c) 2026 Knows
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2/status"
)

// Knows: a login that is still starting is waited for, not refused.
//
// The provisioning contact list and user search refuse a login whose client is
// not logged in (403 "Login is not logged in", upstream #650). A login is
// complete before its client is connected: TikTok returns the last login step,
// then connects in the background and picks its message data centre first, up
// to ~16 s (prod 2026-11-27); the app's first contact list came in between and
// was refused. KnowsWaitLoggedIn lets such a request wait, bounded, for a login
// that is starting (no bridge state yet, or CONNECTING); a login in any other
// state is answered at once, as upstream.

// KnowsLoginStartWaitEnv overrides KnowsLoginStartWaitDefault, in milliseconds;
// 0 turns the wait off (upstream behaviour).
const KnowsLoginStartWaitEnv = "KNOWS_LOGIN_START_WAIT_MS"

// KnowsLoginStartWaitDefault is how long a request waits for a starting login.
const KnowsLoginStartWaitDefault = 25 * time.Second

var (
	knowsLoginStartWaitMax = 2 * time.Minute
	knowsLoginStartPoll    = 100 * time.Millisecond
)

// KnowsLoginStartWait is the wait for a starting login: KNOWS_LOGIN_START_WAIT_MS
// when it is a valid number of milliseconds (at most 2 minutes), else
// KnowsLoginStartWaitDefault.
func KnowsLoginStartWait() time.Duration {
	raw := strings.TrimSpace(os.Getenv(KnowsLoginStartWaitEnv))
	if raw == "" {
		return KnowsLoginStartWaitDefault
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms < 0 {
		return KnowsLoginStartWaitDefault
	}
	return min(time.Duration(ms)*time.Millisecond, knowsLoginStartWaitMax)
}

// KnowsWaitLoggedIn reports whether the login's client is logged in, waiting up
// to wait (and while ctx lasts) when the login is still starting: no bridge
// state reported yet, or CONNECTING. The wait ends early when the login reports
// BAD_CREDENTIALS, LOGGED_OUT or UNCONFIGURED. A login without a client, or in
// any other state, is answered at once.
//
// bridgev2 has no event for "the client is logged in" (IsLoggedIn is the
// connector's own state, and its CONNECTED bridge state can come before or
// after it), so this checks it every 100 ms, as Portal.waitForReceiverLogin.
func (ul *UserLogin) KnowsWaitLoggedIn(ctx context.Context, wait time.Duration) bool {
	if ul == nil || ul.Client == nil {
		return false
	}
	if ul.Client.IsLoggedIn() {
		return true
	}
	if wait <= 0 || !knowsLoginStarting(ul.BridgeState.knowsLatest().StateEvent) {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	ticker := time.NewTicker(knowsLoginStartPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ul.Client.IsLoggedIn()
		case <-ticker.C:
		}
		if ul.Client.IsLoggedIn() {
			return true
		}
		if knowsLoginStopped(ul.BridgeState.knowsLatest().StateEvent) {
			return false
		}
	}
}

func knowsLoginStarting(evt status.BridgeStateEvent) bool {
	return evt == "" || evt == status.StateConnecting
}

func knowsLoginStopped(evt status.BridgeStateEvent) bool {
	switch evt {
	case status.StateBadCredentials, status.StateLoggedOut, status.StateUnconfigured:
		return true
	}
	return false
}

// knowsLatest is the newest state the connector reported (queued, sent or
// not), read under the queue's lock. Every sent state was queued first, so
// prevSent is never newer.
func (bsq *BridgeStateQueue) knowsLatest() status.BridgeState {
	if bsq == nil {
		return status.BridgeState{}
	}
	bsq.writeLock.Lock()
	defer bsq.writeLock.Unlock()
	if bsq.prevUnsent != nil {
		return *bsq.prevUnsent
	}
	return status.BridgeState{}
}
