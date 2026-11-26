// Copyright (c) 2026 Knows
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
)

// Knows: the backward backfill task of a new room is due as soon as its
// forward backfill is over (KNOWS_BACKWARD_DUE_NOW).
//
// When a room is created, its backward backfill task is written due
// BackfillMinBackoffAfterRoomCreate (1 min) later, so that it does not run
// before the initial forward backfill. A room created with WithKnowsFirstBatch
// (create_dm / sync-thread ?first_batch=1) already gets its task made due at
// the end of the forward backfill (knowsMakeBackfillTaskDue). With the switch
// on, every other room creation does too: the event the minute stood for, the
// end of the forward backfill, is waited on instead. The minute stays the
// fallback (a task the switch does not reach keeps it).
//
// Rate limiting is unchanged: the backfill queue still runs one task at a
// time, batch_delay apart, in next_dispatch_min_ts order, with max_batches and
// the connector's own limits. Only the start of a new room's first backward
// batch moves up, by up to a minute.

// KnowsBackwardDueNowEnv turns on the early backward task ("1"). Unset, a room
// created without WithKnowsFirstBatch keeps the one-minute delay, as upstream.
const KnowsBackwardDueNowEnv = "KNOWS_BACKWARD_DUE_NOW"

// knowsInitialForwardBackfill is the one-pass initial forward backfill of a new
// room (doForwardBackfill), then, with KNOWS_BACKWARD_DUE_NOW, its backward task
// made due. firstBatch is true when the caller makes the task due itself
// (WithKnowsFirstBatch).
func (portal *Portal) knowsInitialForwardBackfill(ctx context.Context, source *UserLogin, bundle any, firstBatch bool) {
	portal.doForwardBackfill(ctx, source, nil, bundle)
	if !firstBatch && KnowsFlag(KnowsBackwardDueNowEnv) {
		portal.knowsMakeBackfillTaskDue(ctx, source)
	}
}
