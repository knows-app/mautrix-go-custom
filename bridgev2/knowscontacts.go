// Copyright (c) 2026 Knows
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"sync"
	"sync/atomic"
	"time"
)

// Knows: "first contact list ready".
//
// Right after a login, the app asks for the contact list and, while it is
// empty, asks again after 2 s, up to 6 times. A connector that knows when its
// first list is ready (Telegram read.done, LinkedIn page 1, Discord READY, X
// page 1, TikTok first sync) implements KnowsContactListReadyNetworkAPI, most
// simply by embedding KnowsContactListState and calling Staged. Then
// GET /v3/contacts?wait_ms=N answers as soon as that list is ready (at most
// 12 s), and contacts-staged is posted to knows-api (with KNOWS_CALLBACKS=1).
// A connector without it answers at once, as before.

// KnowsContactListReadyNetworkAPI is a contact-listing client that signals
// when its first contact list can be served.
type KnowsContactListReadyNetworkAPI interface {
	ContactListingNetworkAPI
	// KnowsContactListReady returns a channel closed once the first contact
	// list of the login is ready (already closed when it is). A nil channel
	// means the connector cannot tell (e.g. its switch is off): the request is
	// answered at once without knows_list_ready, as for a connector without
	// this interface. Never return a closed channel for "cannot tell": the app
	// would read an empty list as final.
	KnowsContactListReady() <-chan struct{}
}

// KnowsContactListState is embedded by a connector's client to implement
// KnowsContactListReadyNetworkAPI. The zero value is ready to use. The state
// lives with the client: a connector must call Staged after every start of
// the client (restart, reconnect with a new client), also when the list comes
// from its own store, or each wait_ms request waits for the full cap.
type KnowsContactListState struct {
	once     sync.Once
	ready    chan struct{}
	closed   atomic.Bool
	count    atomic.Int64
	complete atomic.Bool
}

func (s *KnowsContactListState) readyChan() chan struct{} {
	s.once.Do(func() { s.ready = make(chan struct{}) })
	return s.ready
}

// KnowsContactListReady implements KnowsContactListReadyNetworkAPI.
func (s *KnowsContactListState) KnowsContactListReady() <-chan struct{} {
	return s.readyChan()
}

// Staged records that the login's contact list changed: its first list is
// ready, a next page was stored, or it is complete (count is the number of
// contacts the list now has). The first call releases the requests waiting
// with wait_ms. Each call posts contacts-staged when KNOWS_CALLBACKS is on.
// login may be nil (nothing is posted then).
func (s *KnowsContactListState) Staged(login *UserLogin, count int, complete bool) {
	s.count.Store(int64(count))
	s.complete.Store(complete)
	ready := s.readyChan()
	if s.closed.CompareAndSwap(false, true) {
		close(ready)
	}
	if login != nil && login.Bridge != nil {
		login.Bridge.KnowsCallbacks.ContactsStaged(login.ID, count, complete)
	}
}

// KnowsStaged returns the last count and completeness given to Staged, and
// whether Staged was called at all.
func (s *KnowsContactListState) KnowsStaged() (count int, complete, ready bool) {
	return int(s.count.Load()), s.complete.Load(), s.closed.Load()
}

// KnowsRateLimit is a limit the network puts on a login, as the contact list
// reports it (rate_limited): {"errcode": "...", "retry_after_s": N,
// "until_ms": T}. retry_after_s is whole seconds, at least 1; until_ms is the
// same instant in Unix milliseconds.
type KnowsRateLimit struct {
	Errcode     string `json:"errcode"`
	RetryAfterS int    `json:"retry_after_s"`
	UntilMS     int64  `json:"until_ms"`
}

// KnowsRateLimitUntil is the KnowsRateLimit of a limit that ends at until, or
// nil when until is already past.
func KnowsRateLimitUntil(errcode string, until time.Time) *KnowsRateLimit {
	left := time.Until(until)
	if left <= 0 {
		return nil
	}
	return &KnowsRateLimit{
		Errcode:     errcode,
		RetryAfterS: max(1, int((left+time.Second-1)/time.Second)),
		UntilMS:     until.UnixMilli(),
	}
}

// KnowsContactsRateLimitedNetworkAPI is a client that can tell its network is
// rate limiting the login. GET /v3/contacts then carries rate_limited, and
// wait_ms answers at once instead of waiting for a list the limit holds back.
// A connector whose first list gets held while a request waits calls Staged
// (KnowsContactListState) when the hold starts, which releases the request.
type KnowsContactsRateLimitedNetworkAPI interface {
	// KnowsContactsRateLimited returns the login's current limit, nil when no
	// limit is known.
	KnowsContactsRateLimited() *KnowsRateLimit
}

// KnowsFirstContactsAnswer reports whether this is the first contact list
// answered for the login since the process started (for contacts.first_answer_ms).
func (ul *UserLogin) KnowsFirstContactsAnswer() bool {
	return ul.knowsContactsAnswered.CompareAndSwap(false, true)
}
