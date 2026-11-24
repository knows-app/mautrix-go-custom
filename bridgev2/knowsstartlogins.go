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
	"sync"
	"time"
)

// Knows: existing logins connected in a bounded pool at startup.
//
// Upstream connects the logins of a restarted bridge one by one, so the n-th
// login receives nothing until the n-1 before it are connected (dc01 77 s for
// 42 logins, fb01 79 s for 32). KNOWS_START_LOGINS_CONCURRENCY=N connects up to
// N at once; a login still connecting after KnowsStartLoginMaxWait no longer
// holds its place in the pool (its Connect keeps running). Unset or 1, the
// loop is serial, as upstream.

// KnowsStartLoginsConcurrencyEnv is the size of the startup Connect pool.
const KnowsStartLoginsConcurrencyEnv = "KNOWS_START_LOGINS_CONCURRENCY"

var (
	// KnowsStartLoginMaxWait is how long one login holds a place in the pool.
	KnowsStartLoginMaxWait = 30 * time.Second
	// knowsStartLoginsMax bounds KNOWS_START_LOGINS_CONCURRENCY.
	knowsStartLoginsMax = 64
)

// KnowsStartLoginsConcurrency reads KNOWS_START_LOGINS_CONCURRENCY: 1 (serial)
// when unset or invalid, at most 64.
func KnowsStartLoginsConcurrency() int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(KnowsStartLoginsConcurrencyEnv)))
	if err != nil || n < 1 {
		return 1
	}
	return min(n, knowsStartLoginsMax)
}

// MatrixConnectorWithKnowsLoginsReady is a Matrix connector that serves the
// Knows "logins loaded" readiness (GET /_matrix/mau/ready?logins=1).
type MatrixConnectorWithKnowsLoginsReady interface {
	MatrixConnector
	KnowsSetLoginsReady(started, total int, ready bool)
}

func (br *Bridge) knowsSetLoginsReady(started, total int, ready bool) {
	if rc, ok := br.Matrix.(MatrixConnectorWithKnowsLoginsReady); ok {
		rc.KnowsSetLoginsReady(started, total, ready)
	}
}

// knowsConnectLogins connects logins with up to n Connect calls at once and
// returns once each one has returned or held its place for KnowsStartLoginMaxWait.
func (br *Bridge) knowsConnectLogins(ctx context.Context, logins []*UserLogin, n int) {
	pool := make(chan struct{}, n)
	var wg sync.WaitGroup
	var startedLock sync.Mutex
	started := 0
	for _, login := range logins {
		pool <- struct{}{}
		wg.Add(1)
		br.Log.Info().Str("id", string(login.ID)).Msg("Starting user login")
		connected := make(chan struct{})
		go func() {
			defer close(connected)
			login.Client.Connect(login.Log.WithContext(ctx))
		}()
		go func() {
			defer wg.Done()
			defer func() { <-pool }()
			select {
			case <-connected:
			case <-time.After(KnowsStartLoginMaxWait):
				br.Log.Warn().
					Stringer("waited", KnowsStartLoginMaxWait).
					Msg("Login still connecting, starting the next ones")
			}
			startedLock.Lock()
			started++
			br.knowsSetLoginsReady(started, len(logins), false)
			startedLock.Unlock()
		}()
	}
	wg.Wait()
}
