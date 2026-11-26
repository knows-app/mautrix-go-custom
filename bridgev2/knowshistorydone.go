// Copyright (c) 2026 Knows
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"errors"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix/bridgev2/database"
)

// Knows: tell knows-api when a room's history is over
// (KNOWS_PORTAL_READY_HISTORY_DONE, with KNOWS_CALLBACKS).
//
// knows-api waits for a new chat's history before Genie reads it, up to a cap
// (15 s), because portal-ready first_batch only says the first batch is
// written: more may follow. With the switch on, portal-ready is posted once
// more for the room, with "history_done": true and "messages", as soon as the
// bridge will write nothing more of the room's history on its own:
//
//   - the portal's backward backfill task ends: done (the network has nothing
//     older, the backward fetch answered nothing, max_batches reached) or done
//     for the queue (the rest needs a slow fetch, which the queue does not do).
//     Sent after the task's last batch is written;
//   - the initial forward backfill is over and no backward task is left to
//     run: the room has none (the chat cannot be backfilled), it is already
//     done, or the queue does not run (backfill.queue.enabled false, or a
//     homeserver without batch sending). Sent at once.
//
// Never sent while a backward task is still to run (it is sent at its end
// instead), for a task waiting on a logged-in user (no login), for a space, or
// in background mode. "messages" is the number of messages the bridge holds
// for the room (message rows, one per part), logged by knows-api. Once per room
// for the process (KnowsCallbacks.historyDoneSent); a repeat after a restart is
// the same event for knows-api.
//
// The fields are the ones mautrix-meta-custom posts for Messenger rooms
// (pkg/knowscallbacks.PortalReadyBody, same switch name): a Messenger room may
// be told twice, once by each, which knows-api takes as one event.

// knowsHistoryDone posts the history-done portal-ready of the portal's room.
func (portal *Portal) knowsHistoryDone(ctx context.Context, source *UserLogin, reason string) {
	kc := portal.Bridge.KnowsCallbacks
	if !kc.HistoryDoneOn() || source == nil || portal.MXID == "" || portal.RoomType == database.RoomTypeSpace {
		return
	}
	log := zerolog.Ctx(ctx)
	messages, err := portal.Bridge.DB.Message.CountMessagesInPortal(ctx, portal.PortalKey)
	if err != nil {
		log.Err(err).Msg("Failed to count messages for the history-done callback")
		return
	}
	if kc.PortalHistoryDone(source.ID, portal.ID, portal.MXID, messages) {
		log.Info().
			Int("messages", messages).
			Str("reason", reason).
			Msg("Telling knows-api the room's history is done")
	}
}

// knowsHistoryDoneAfterForward is called once the initial forward backfill of
// a new room is over (or skipped): history_done goes out at once when no
// backward task is left to run. Otherwise the task's end sends it
// (knowsDoBackfillTask).
func (portal *Portal) knowsHistoryDoneAfterForward(ctx context.Context, source *UserLogin) {
	if !portal.Bridge.KnowsCallbacks.HistoryDoneOn() || portal.MXID == "" || portal.Bridge.Background {
		return
	}
	reason, ok := portal.knowsNoBackwardLeft(ctx)
	if ok {
		portal.knowsHistoryDone(ctx, source, reason)
	}
}

// knowsNoBackwardLeft reports whether the bridge will run no backward backfill
// for the portal on its own, and why. When unsure (database error, a task
// waiting for a login) it says no.
func (portal *Portal) knowsNoBackwardLeft(ctx context.Context) (string, bool) {
	cfg := &portal.Bridge.Config.Backfill
	if !cfg.Enabled || !cfg.Queue.Enabled || !portal.Bridge.Matrix.GetCapabilities().BatchSending {
		return "no_backfill_queue", true
	}
	task, err := portal.Bridge.DB.BackfillTask.GetNextForPortal(ctx, portal.PortalKey, true)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to get backfill task for the history-done callback")
		return "", false
	}
	switch {
	case task == nil:
		return "no_backward_task", true
	case task.IsDone || task.QueueDone:
		return "backward_task_done", true
	default:
		return "", false
	}
}

// knowsDoBackfillTask is doBackfillTask, then history_done when this run ended
// the task (done, or done for the queue). A batch whose messages were all cut
// off (errNoMessagesLeftAfterCutoff) still ends it: the task is saved as done.
func (portal *Portal) knowsDoBackfillTask(ctx context.Context, source *UserLogin, task *database.BackfillTask, resp *FetchMessagesResponse) (bool, error) {
	wasOver := task.IsDone || task.QueueDone
	completed, err := portal.doBackfillTask(ctx, source, task, resp)
	if !wasOver && (task.IsDone || task.QueueDone) && (err == nil || errors.Is(err, errNoMessagesLeftAfterCutoff)) {
		portal.knowsHistoryDone(ctx, source, "backward_task_done")
	}
	return completed, err
}
