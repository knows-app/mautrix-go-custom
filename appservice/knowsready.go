// Copyright (c) 2026 Knows
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package appservice

import (
	"net/http"
	"sync"

	"go.mau.fi/util/exhttp"
)

// KnowsLoginsReadiness is the Knows "logins loaded" readiness: the bridge's
// existing logins have all been started (connected, or given their place in the
// startup pool up).
type KnowsLoginsReadiness struct {
	lock    sync.Mutex
	started int
	total   int
	ready   bool
}

// Set records the startup progress. total is 0 while it is not known yet.
func (klr *KnowsLoginsReadiness) Set(started, total int, ready bool) {
	klr.lock.Lock()
	klr.started, klr.total, klr.ready = started, total, ready
	klr.lock.Unlock()
}

// Get returns the startup progress.
func (klr *KnowsLoginsReadiness) Get() (started, total int, ready bool) {
	klr.lock.Lock()
	defer klr.lock.Unlock()
	return klr.started, klr.total, klr.ready
}

type knowsReadyResponse struct {
	Ready         bool `json:"ready"`
	LoginsReady   bool `json:"logins_ready"`
	LoginsStarted int  `json:"logins_started"`
	LoginsTotal   int  `json:"logins_total,omitempty"`
}

// getReadyWithLogins answers GET /_matrix/mau/ready?logins=1: 200 once the
// appservice is ready and the existing logins are started, 500 before (like the
// plain readiness), with the progress in the body.
func (as *AppService) getReadyWithLogins(w http.ResponseWriter) {
	started, total, loginsReady := as.KnowsLogins.Get()
	resp := knowsReadyResponse{Ready: as.Ready, LoginsReady: loginsReady, LoginsStarted: started, LoginsTotal: total}
	status := http.StatusInternalServerError
	if as.Ready && loginsReady {
		status = http.StatusOK
	}
	exhttp.WriteJSONResponse(w, status, resp)
}
