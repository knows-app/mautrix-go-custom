// Copyright (c) 2026 Knows
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"errors"
	"fmt"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// roomExistenceChecker is implemented by a Matrix API that can tell "does this room still exist" without a
// homeserver round trip (the Knows messages store).
type roomExistenceChecker interface {
	RoomExists(ctx context.Context, roomID id.RoomID) (bool, error)
}

// RoomIsGone reports whether the portal points at a room the homeserver no longer has (deleted by an account
// deletion or an admin purge while the portal row kept its mxid). Only a definite answer counts: M_NOT_FOUND or
// M_FORBIDDEN on the room's create event, or the store saying the room is not there. A transient error keeps
// the room, since a spurious re-create would orphan a live one.
func (portal *Portal) RoomIsGone(ctx context.Context) bool {
	if portal.MXID == "" {
		return false
	}
	if checker, ok := portal.Bridge.Bot.(roomExistenceChecker); ok {
		exists, err := checker.RoomExists(ctx, portal.MXID)
		return err == nil && !exists
	}
	// The bot intent's GetStateEvent is a real /state call; the connector-level variant answers m.room.create
	// from the bridge's own state store cache and never notices the room is gone.
	if stateGetter, ok := portal.Bridge.Bot.(MatrixAPIWithArbitraryRoomState); ok {
		_, err := stateGetter.GetStateEvent(ctx, portal.MXID, event.StateCreate, "")
		return err != nil && (errors.Is(err, mautrix.MNotFound) || errors.Is(err, mautrix.MForbidden))
	}
	return false
}

// ForgetDeadRoom clears the mxid of a portal whose room is gone (or is being replaced, CleanupOrphanedDM), and
// the portal's message and reaction rows, which belong to the old room: a fresh room re-inserts the same message
// IDs, and forward backfill anchors on the newest message row, so leftovers would make the new room's first fill
// fetch nothing, or drop what it fetched as duplicates.
//
// The deletes and the cleared mxid are one transaction: when saving the portal fails, the rows stay with the old
// room and the portal keeps its mxid, in memory too.
func (portal *Portal) ForgetDeadRoom(ctx context.Context) error {
	oldMXID, oldRelay, oldRelayLoginID := portal.MXID, portal.Relay, portal.RelayLoginID
	wasCreated := portal.RoomCreated != nil && portal.RoomCreated.IsSet()
	err := portal.Bridge.DB.DoTxn(ctx, nil, func(ctx context.Context) error {
		for _, tbl := range []string{"reaction", "message"} {
			if _, err := portal.Bridge.DB.Exec(ctx,
				"DELETE FROM "+tbl+" WHERE bridge_id=$1 AND room_id=$2 AND room_receiver=$3",
				portal.Bridge.DB.BridgeID, portal.ID, portal.Receiver,
			); err != nil {
				return fmt.Errorf("clear old %s rows: %w", tbl, err)
			}
		}
		// RemoveMXID saves the portal with this ctx, so inside the transaction.
		return portal.RemoveMXID(ctx)
	})
	if err != nil && oldMXID != "" && portal.MXID == "" {
		portal.MXID, portal.Relay, portal.RelayLoginID = oldMXID, oldRelay, oldRelayLoginID
		if wasCreated {
			portal.RoomCreated.Set()
		}
		portal.Bridge.cacheLock.Lock()
		portal.Bridge.portalsByMXID[oldMXID] = portal
		portal.Bridge.cacheLock.Unlock()
	}
	return err
}

// RecoverDeadRoom forgets the portal's room when it is gone, so the caller creates a fresh one.
// It returns true when the room was forgotten.
func (portal *Portal) RecoverDeadRoom(ctx context.Context) bool {
	if !portal.RoomIsGone(ctx) {
		return false
	}
	log := zerolog.Ctx(ctx).With().Stringer("dead_mxid", portal.MXID).Logger()
	if err := portal.ForgetDeadRoom(ctx); err != nil {
		log.Err(err).Msg("Portal room is gone from the homeserver, but forgetting it failed")
		return false
	}
	log.Warn().Msg("Portal room was gone from the homeserver; it will be re-created")
	return true
}

// ErrPortalHasNoRoom is the error of a backfill of a portal that has no Matrix room (never created, or forgotten
// by ForgetDeadRoom). Nothing is fetched or written: a write to an empty room ID only fails with a bare 404.
var ErrPortalHasNoRoom = errors.New("portal has no Matrix room")

// KnowsRecoverDeadRoomOnWriteEnv turns on ("1") the recovery of a room found gone by a write. Unset, a room
// deleted behind the bridge is only forgotten when a user picks the DM (create_dm), as before.
const KnowsRecoverDeadRoomOnWriteEnv = "KNOWS_RECOVER_DEAD_ROOM_ON_WRITE"

// knowsRecoverDeadRoomOnWrite is called with the error of a write into the portal's room (backfill batch,
// remote event). With KNOWS_RECOVER_DEAD_ROOM_ON_WRITE, an M_NOT_FOUND or M_FORBIDDEN makes it check that the
// room is gone (RoomIsGone, a definite answer only) and forget it (RecoverDeadRoom), so the next resync or pick
// creates a fresh room instead of failing at every start. It reports whether the room was forgotten.
func (portal *Portal) knowsRecoverDeadRoomOnWrite(ctx context.Context, err error) bool {
	if err == nil || portal.MXID == "" || !KnowsFlag(KnowsRecoverDeadRoomOnWriteEnv) {
		return false
	}
	if !errors.Is(err, mautrix.MNotFound) && !errors.Is(err, mautrix.MForbidden) {
		return false
	}
	return portal.RecoverDeadRoom(ctx)
}
