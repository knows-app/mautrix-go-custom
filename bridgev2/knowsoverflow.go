// Copyright (c) 2026 Knows
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// Knows: a burst in one chat no longer stops the whole login.
//
// Upstream, a remote event that finds its portal's channel full
// (bridge.portal_event_buffer, 64 in Knows configs) blocks the caller, which is
// the login's event loop: every other chat of the account stops receiving until
// the busy portal catches up ("Portal event channel is full"). With
// KNOWS_PORTAL_OVERFLOW=1, such an event is kept in memory instead, in a FIFO
// per portal that feeds the channel in order:
//   - up to KnowsPortalRemoteBuffer (512) events per portal are kept freely;
//   - beyond that, each event takes one of the login's KnowsPortalOverflowPerLogin
//     (10 000) slots, shared by all its portals;
//   - with no slot left, the caller blocks as upstream does, until a slot frees
//     up or the portal is deleted.
// Matrix events and room creations keep the upstream path. The memory queue is
// allocated only while a portal has overflowed events, so the channel stays at
// its configured size.

// KnowsPortalOverflowEnv turns the in-memory overflow on ("1").
const KnowsPortalOverflowEnv = "KNOWS_PORTAL_OVERFLOW"

var (
	// KnowsPortalRemoteBuffer is how many remote events a portal keeps in
	// memory, beyond its channel, before using its login's slots.
	KnowsPortalRemoteBuffer = 512
	// KnowsPortalOverflowPerLogin is how many more remote events a login's
	// portals may keep in memory before its event loop blocks again.
	KnowsPortalOverflowPerLogin = 10000
	// knowsPortalStillFullEvery is how often a blocked caller logs again.
	knowsPortalStillFullEvery = 5 * time.Second
)

var knowsPortalChannelFull atomic.Int64

// KnowsPortalChannelFullCount is the number of remote events that found their
// portal's channel full since the process started (the portal_channel_full
// counter; also logged on the "Portal event channel is full" lines).
func KnowsPortalChannelFullCount() int64 {
	return knowsPortalChannelFull.Load()
}

type knowsOverflowItem struct {
	evt portalEvent
	// login is set when the item holds one of that login's slots.
	login *UserLogin
}

type knowsPortalOverflow struct {
	lock    sync.Mutex
	items   []knowsOverflowItem
	pumping bool
	since   time.Time
}

type knowsLoginOverflow struct {
	once  sync.Once
	slots chan struct{}
}

func (ul *UserLogin) knowsOverflowSlots() chan struct{} {
	ul.knowsOverflow.once.Do(func() {
		ul.knowsOverflow.slots = make(chan struct{}, KnowsPortalOverflowPerLogin)
	})
	return ul.knowsOverflow.slots
}

// knowsTakeOverflowSlot takes one of the login's slots, blocking (as upstream
// does on a full channel) while there is none. It returns false if the portal
// was deleted meanwhile.
func (portal *Portal) knowsTakeOverflowSlot(ctx context.Context, login *UserLogin) bool {
	slots := login.knowsOverflowSlots()
	select {
	case slots <- struct{}{}:
		return true
	default:
	}
	zerolog.Ctx(ctx).Error().
		Str("portal_id", string(portal.ID)).
		Int64("portal_channel_full", knowsPortalChannelFull.Load()).
		Int("login_overflow", cap(slots)).
		Msg("Portal event channel is full, queue will block")
	for {
		select {
		case slots <- struct{}{}:
			return true
		case <-portal.deleted:
			return false
		case <-time.After(knowsPortalStillFullEvery):
			zerolog.Ctx(ctx).Error().
				Str("portal_id", string(portal.ID)).
				Msg("Portal event channel is still full")
		}
	}
}

func (item knowsOverflowItem) release() {
	if item.login != nil {
		<-item.login.knowsOverflowSlots()
	}
}

// knowsQueueRemoteEvent queues a remote event with the in-memory overflow. It
// is called with eventsSendLock read-locked, like the upstream path.
func (portal *Portal) knowsQueueRemoteEvent(ctx context.Context, evt *portalRemoteEvent) EventHandlingResult {
	ov := &portal.knowsOverflowQueue
	ov.lock.Lock()
	if len(ov.items) == 0 {
		select {
		case portal.events <- evt:
			ov.lock.Unlock()
			return EventHandlingResultQueued
		default:
		}
	}
	held := len(ov.items)
	ov.lock.Unlock()

	item := knowsOverflowItem{evt: evt}
	if held >= KnowsPortalRemoteBuffer && evt.source != nil {
		if !portal.knowsTakeOverflowSlot(ctx, evt.source) {
			return EventHandlingResultIgnored
		}
		item.login = evt.source
	}
	total := knowsPortalChannelFull.Add(1)
	ov.lock.Lock()
	ov.items = append(ov.items, item)
	started := !ov.pumping
	if started {
		ov.pumping = true
		ov.since = time.Now()
		go portal.knowsPumpOverflow()
	}
	ov.lock.Unlock()
	if started {
		zerolog.Ctx(ctx).Warn().
			Str("portal_id", string(portal.ID)).
			Int64("portal_channel_full", total).
			Msg("Portal event channel is full, keeping events in memory")
	}
	return EventHandlingResultQueued
}

// knowsPumpOverflow moves a portal's overflowed events into its channel, in
// order. An event leaves the memory queue only once it is in the channel, so a
// newer event can never pass it.
func (portal *Portal) knowsPumpOverflow() {
	ov := &portal.knowsOverflowQueue
	moved := 0
	for {
		ov.lock.Lock()
		if len(ov.items) == 0 {
			ov.items = nil
			ov.pumping = false
			since := ov.since
			ov.lock.Unlock()
			portal.Log.Debug().
				Int("event_count", moved).
				Int64("overflow_ms", time.Since(since).Milliseconds()).
				Msg("Portal event overflow drained")
			return
		}
		item := ov.items[0]
		ov.lock.Unlock()
		sent := portal.knowsSendFromOverflow(item.evt)
		ov.lock.Lock()
		ov.items[0] = knowsOverflowItem{}
		ov.items = ov.items[1:]
		var dropped []knowsOverflowItem
		if !sent {
			dropped, ov.items = ov.items, nil
			ov.pumping = false
		}
		ov.lock.Unlock()
		item.release()
		if !sent {
			for _, rest := range dropped {
				rest.release()
			}
			return
		}
		moved++
	}
}

func (portal *Portal) knowsSendFromOverflow(evt portalEvent) bool {
	portal.eventsSendLock.RLock()
	defer portal.eventsSendLock.RUnlock()
	if portal.backgroundCtx.Err() != nil {
		return false
	}
	select {
	case portal.events <- evt:
		return true
	case <-portal.deleted:
		return false
	}
}
