// Copyright (c) 2026 Knows
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

// Paths of the Knows callbacks, on the API host that already receives the
// shard's bridge states (homeserver.status_endpoint). They are knows-api's
// bridge_callbacks constants.
const (
	KnowsPortalReadyPath    = "/internal/bridge/portal-ready"
	KnowsContactsStagedPath = "/internal/bridge/contacts-staged"
	KnowsContactChangedPath = "/internal/bridge/contact-changed"
)

// KnowsCallbacksEnv turns the Knows callbacks on ("1"). Unset, nothing is sent.
const KnowsCallbacksEnv = "KNOWS_CALLBACKS"

// KnowsFlag reports whether the environment variable name is set to 1, true,
// yes or on. Unset (the default) is off: every Knows behaviour change is off
// until its flag is set.
func KnowsFlag(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Kinds of contact change for ContactChanged.
const (
	KnowsContactAdded   = "added"
	KnowsContactName    = "name"
	KnowsContactAvatar  = "avatar"
	KnowsContactRemoved = "removed"
)

var (
	// KnowsCallbackTimeout bounds each callback attempt.
	KnowsCallbackTimeout = 5 * time.Second
	// KnowsCallbackRetryDelay is the pause before the only retry.
	KnowsCallbackRetryDelay = 1 * time.Second
	// KnowsContactChangedWindow groups the contact changes of one login into one call.
	KnowsContactChangedWindow = 2 * time.Second
	// KnowsContactChangedMaxBatch sends a login's changes early once this many are waiting.
	KnowsContactChangedMaxBatch = 200
)

// KnowsContactChange is one entry of a contact-changed callback.
type KnowsContactChange struct {
	ContactID   string `json:"contact_id"`
	Change      string `json:"change"`
	TimestampMS int64  `json:"ts_ms"`
}

// KnowsCallbacks posts the Knows bridge callbacks to knows-api: portal-ready,
// contacts-staged and contact-changed. The caller is identified by the
// appservice token (Authorization: Bearer), never by the body.
//
// Every method returns at once: the post runs in a goroutine with a 5 s timeout
// and at most one retry (on a network error or a 5xx; a 4xx is final, which
// includes an API that does not know the path yet). Nothing in a bridge waits on
// these calls. A nil *KnowsCallbacks is valid and sends nothing.
type KnowsCallbacks struct {
	base   string
	token  string
	client *http.Client
	log    zerolog.Logger

	pendingLock sync.Mutex
	pending     map[networkid.UserLoginID]*knowsPendingChanges
	inflight    sync.WaitGroup
}

type knowsPendingChanges struct {
	changes map[[2]string]KnowsContactChange
	order   [][2]string
	timer   *time.Timer
}

// NewKnowsCallbacks builds the callback client from homeserver.status_endpoint
// (only its scheme and host are kept) and the appservice token. It returns nil,
// which sends nothing, when KNOWS_CALLBACKS is not on or the endpoint is not an
// http(s) URL.
func NewKnowsCallbacks(statusEndpoint, asToken string, log zerolog.Logger) *KnowsCallbacks {
	if !KnowsFlag(KnowsCallbacksEnv) {
		return nil
	}
	u, err := url.Parse(statusEndpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil
	}
	if asToken == "generate" {
		asToken = ""
	}
	return &KnowsCallbacks{
		base:    u.Scheme + "://" + u.Host,
		token:   asToken,
		client:  &http.Client{},
		log:     log.With().Str("component", "knows callbacks").Logger(),
		pending: make(map[networkid.UserLoginID]*knowsPendingChanges),
	}
}

type knowsPortalReadyBody struct {
	LoginID    string `json:"login_id"`
	ThreadID   string `json:"thread_id"`
	RoomID     string `json:"room_id"`
	FirstBatch bool   `json:"first_batch"`
}

type knowsContactsStagedBody struct {
	LoginID  string `json:"login_id"`
	Count    int    `json:"count"`
	Complete bool   `json:"complete"`
}

type knowsContactChangedBody struct {
	LoginID string               `json:"login_id"`
	Changes []KnowsContactChange `json:"changes"`
}

// PortalReady says a chat's room exists (firstBatch false), or that the first
// batch of its history is written (firstBatch true).
func (kc *KnowsCallbacks) PortalReady(loginID networkid.UserLoginID, threadID networkid.PortalID, roomID id.RoomID, firstBatch bool) {
	if kc == nil || loginID == "" || roomID == "" {
		return
	}
	kc.post(KnowsPortalReadyPath, knowsPortalReadyBody{
		LoginID: string(loginID), ThreadID: string(threadID), RoomID: string(roomID), FirstBatch: firstBatch,
	})
}

// ContactsStaged says the contact list of a login changed: its first list is
// ready, a next page was stored, or it is complete.
func (kc *KnowsCallbacks) ContactsStaged(loginID networkid.UserLoginID, count int, complete bool) {
	if kc == nil || loginID == "" {
		return
	}
	kc.post(KnowsContactsStagedPath, knowsContactsStagedBody{LoginID: string(loginID), Count: count, Complete: complete})
}

// ContactChanged queues contact changes of a login. The changes of one login
// are sent together, KnowsContactChangedWindow after the first one (or as soon
// as KnowsContactChangedMaxBatch are waiting). A contact with the same kind of
// change twice in a window is sent once, with the latest timestamp.
func (kc *KnowsCallbacks) ContactChanged(loginID networkid.UserLoginID, changes ...KnowsContactChange) {
	if kc == nil || loginID == "" || len(changes) == 0 {
		return
	}
	kc.pendingLock.Lock()
	defer kc.pendingLock.Unlock()
	p := kc.pending[loginID]
	if p == nil {
		p = &knowsPendingChanges{changes: make(map[[2]string]KnowsContactChange)}
		kc.pending[loginID] = p
		// The window counts as in flight, so Wait also waits for queued changes.
		kc.inflight.Add(1)
		p.timer = time.AfterFunc(KnowsContactChangedWindow, func() {
			defer kc.inflight.Done()
			kc.flushContactChanges(loginID, p)
		})
	}
	for _, change := range changes {
		if change.ContactID == "" || change.Change == "" {
			continue
		}
		if change.TimestampMS <= 0 {
			change.TimestampMS = time.Now().UnixMilli()
		}
		key := [2]string{change.ContactID, change.Change}
		if prev, ok := p.changes[key]; !ok {
			p.order = append(p.order, key)
		} else if prev.TimestampMS > change.TimestampMS {
			change.TimestampMS = prev.TimestampMS
		}
		p.changes[key] = change
	}
	if len(p.order) >= KnowsContactChangedMaxBatch && p.timer.Stop() {
		go func() {
			defer kc.inflight.Done()
			kc.flushContactChanges(loginID, p)
		}()
	}
}

func (kc *KnowsCallbacks) flushContactChanges(loginID networkid.UserLoginID, p *knowsPendingChanges) {
	kc.pendingLock.Lock()
	if kc.pending[loginID] != p {
		kc.pendingLock.Unlock()
		return
	}
	delete(kc.pending, loginID)
	kc.pendingLock.Unlock()
	if len(p.order) == 0 {
		return
	}
	for start := 0; start < len(p.order); start += KnowsContactChangedMaxBatch {
		end := min(start+KnowsContactChangedMaxBatch, len(p.order))
		batch := make([]KnowsContactChange, 0, end-start)
		for _, key := range p.order[start:end] {
			batch = append(batch, p.changes[key])
		}
		kc.post(KnowsContactChangedPath, knowsContactChangedBody{LoginID: string(loginID), Changes: batch})
	}
}

// FlushContactChanges sends every queued contact change now.
func (kc *KnowsCallbacks) FlushContactChanges() {
	if kc == nil {
		return
	}
	kc.pendingLock.Lock()
	all := make(map[networkid.UserLoginID]*knowsPendingChanges, len(kc.pending))
	for loginID, p := range kc.pending {
		if p.timer.Stop() {
			all[loginID] = p
		}
	}
	kc.pendingLock.Unlock()
	for loginID, p := range all {
		kc.flushContactChanges(loginID, p)
		kc.inflight.Done()
	}
}

// Wait blocks until the callbacks already posted, and the contact changes
// already queued, are sent (tests, shutdown).
func (kc *KnowsCallbacks) Wait() {
	if kc != nil {
		kc.inflight.Wait()
	}
}

func (kc *KnowsCallbacks) post(path string, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		return
	}
	kc.inflight.Add(1)
	go func() {
		defer kc.inflight.Done()
		kc.send(path, data)
	}()
}

func (kc *KnowsCallbacks) send(path string, data []byte) {
	log := kc.log.With().Str("callback", path).Logger()
	for attempt := 1; attempt <= 2; attempt++ {
		status, err := kc.sendOnce(path, data)
		if err == nil && status < 300 {
			log.Debug().Int("attempt", attempt).Msg("Knows callback sent")
			return
		} else if err == nil && status < 500 {
			log.Debug().Int("status", status).Msg("Knows callback refused")
			return
		}
		if err == nil {
			err = fmt.Errorf("HTTP %d", status)
		}
		log.Debug().Err(err).Int("attempt", attempt).Msg("Knows callback not delivered")
		if attempt == 1 {
			time.Sleep(KnowsCallbackRetryDelay)
		}
	}
}

func (kc *KnowsCallbacks) sendOnce(path string, data []byte) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), KnowsCallbackTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kc.base+path, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if kc.token != "" {
		req.Header.Set("Authorization", "Bearer "+kc.token)
	}
	resp, err := kc.client.Do(req)
	if err != nil {
		return 0, err
	}
	// Drained so the connection is reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
