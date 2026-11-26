// Copyright (c) 2026 Knows
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/exerrors"

	"maunium.net/go/mautrix/bridgev2/database"
)

// Knows: answer create_dm / sync-thread at the first batch of history.
//
// Without the opt-in, CreateMatrixRoom returns once the whole initial forward
// backfill is written (up to backfill.max_initial_messages, media included), as
// upstream does. A caller that asks for it (WithKnowsFirstBatch, set by
// create_dm?first_batch=1 and by the bridges' sync-thread?first_batch=1) gets
// the room as soon as it exists and the first KnowsFirstBatchSize messages are
// written, or after KnowsFirstBatchMaxWait at the latest. The rest of the
// history is then fetched in the background and written above the first batch
// (a backward batch deduplicated by ID, the same rule as a KnowsFillGap batch),
// and the portal's backward backfill task is made due at once.

var (
	// KnowsFirstBatchSize is the size of the first batch a waiting caller gets.
	KnowsFirstBatchSize = 50
	// KnowsFirstBatchMaxWait is how long a waiting caller waits for the first batch at most.
	KnowsFirstBatchMaxWait = 5 * time.Second
)

type knowsFirstBatchRequest struct {
	historyDone     func()
	historyDoneOnce sync.Once
	answeredEarly   atomic.Bool
}

type knowsContextKey int

const (
	knowsContextKeyFirstBatch knowsContextKey = iota
	knowsContextKeyRoomReady
)

// WithKnowsFirstBatch marks a room creation (CreateMatrixRoom) a user is
// waiting on: it returns at the first batch of history instead of at the end of
// the initial backfill. historyDone, which may be nil, is called once, from
// another goroutine, when the whole initial history of the room is written (or
// given up), for example to release a lock the bridge held across the creation.
// It is called once whenever this call created the room, even if
// CreateMatrixRoom itself returned an error to the caller (its context ended
// first); it is not called when the room already existed or was not created.
func WithKnowsFirstBatch(ctx context.Context, historyDone func()) context.Context {
	return context.WithValue(ctx, knowsContextKeyFirstBatch, &knowsFirstBatchRequest{historyDone: historyDone})
}

// KnowsAnsweredAtFirstBatch reports whether a CreateMatrixRoom call made with a
// WithKnowsFirstBatch context returned before the whole initial history was
// written (the rest is still being backfilled).
func KnowsAnsweredAtFirstBatch(ctx context.Context) bool {
	req := knowsFirstBatchFromContext(ctx)
	return req != nil && req.answeredEarly.Load()
}

// KnowsWantsFirstBatch reports whether ctx carries WithKnowsFirstBatch.
func KnowsWantsFirstBatch(ctx context.Context) bool {
	return knowsFirstBatchFromContext(ctx) != nil
}

func knowsFirstBatchFromContext(ctx context.Context) *knowsFirstBatchRequest {
	req, _ := ctx.Value(knowsContextKeyFirstBatch).(*knowsFirstBatchRequest)
	return req
}

func (req *knowsFirstBatchRequest) done() {
	if req == nil {
		return
	}
	req.historyDoneOnce.Do(func() {
		if req.historyDone != nil {
			go req.historyDone()
		}
	})
}

// knowsOnce wraps a room creation callback so that only its first call counts:
// the creation can answer its caller early (first batch) and again at the end.
func knowsOnce(cb func(error)) func(error) {
	var once sync.Once
	return func(err error) {
		once.Do(func() { cb(err) })
	}
}

// knowsWithRoomReady puts the early answer of a waiting room creation in its context.
func knowsWithRoomReady(ctx context.Context, cb func(error)) context.Context {
	if knowsFirstBatchFromContext(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, knowsContextKeyRoomReady, func() { cb(nil) })
}

func knowsRoomReadyFromContext(ctx context.Context) func() {
	fn, _ := ctx.Value(knowsContextKeyRoomReady).(func())
	if fn == nil {
		return func() {}
	}
	return fn
}

// knowsCreateForwardBackfill is the initial forward backfill of a room
// createMatrixRoomInLoop just created. Without WithKnowsFirstBatch it is
// doForwardBackfill, unchanged (with KNOWS_BACKWARD_DUE_NOW, the backward task
// is then made due: knowsInitialForwardBackfill).
func (portal *Portal) knowsCreateForwardBackfill(ctx context.Context, source *UserLogin, bundle any) {
	req := knowsFirstBatchFromContext(ctx)
	limit := portal.Bridge.Config.Backfill.MaxInitialMessages
	// Without batch sending the rest could not be written above the first batch: one pass, as upstream.
	if req == nil || bundle != nil || limit <= KnowsFirstBatchSize || !portal.Bridge.Matrix.GetCapabilities().BatchSending {
		portal.knowsInitialForwardBackfill(ctx, source, bundle, req != nil)
		portal.knowsFirstBatchWritten(source)
		if req != nil {
			portal.knowsMakeBackfillTaskDue(ctx, source)
			req.done()
		}
		return
	}
	handedOff := false
	defer func() {
		if !handedOff {
			req.done()
		}
	}()
	release := knowsRoomReadyFromContext(ctx)
	// The caller may stop waiting (5 s cap, request gone): the backfill must not
	// stop with it.
	bgCtx := zerolog.Ctx(ctx).WithContext(portal.backgroundCtx)
	capTimer := time.AfterFunc(KnowsFirstBatchMaxWait, func() {
		req.answeredEarly.Store(true)
		release()
	})
	oldest, more, firstDone := portal.knowsDoFirstBatch(bgCtx, source, limit)
	capTimer.Stop()
	portal.knowsFirstBatchWritten(source)
	if !more {
		if firstDone != nil {
			firstDone()
		}
		release()
		portal.knowsMakeBackfillTaskDue(bgCtx, source)
		return
	}
	req.answeredEarly.Store(true)
	release()
	handedOff = true
	go func() {
		defer req.done()
		// The first batch's CompleteCallback runs after the second pass: a connector
		// may drop its staged history in it (Signal, WhatsApp without the queue).
		if firstDone != nil {
			defer firstDone()
		}
		defer func() {
			if v := recover(); v != nil {
				zerolog.Ctx(bgCtx).Err(exerrors.RecoverToError(v)).
					Bytes(zerolog.ErrorStackFieldName, debug.Stack()).
					Msg("Panic in the rest of the initial history backfill")
			}
		}()
		portal.knowsDoRestOfHistory(bgCtx, source, limit, oldest)
		portal.knowsMakeBackfillTaskDue(bgCtx, source)
	}()
}

func (portal *Portal) knowsFirstBatchWritten(source *UserLogin) {
	if portal.RoomType == database.RoomTypeSpace || portal.MXID == "" {
		return
	}
	portal.Bridge.KnowsCallbacks.PortalReady(source.ID, portal.ID, portal.MXID, true)
}

// knowsDoFirstBatch fetches and writes the first batch, in the portal's event
// loop like doForwardBackfill. oldest is the timestamp of the oldest message
// written (zero if none was), and more says whether the rest of the history
// still has to be fetched.
//
// The response's CompleteCallback is not run here but returned as firstDone,
// for the caller to run once the whole initial history is written.
func (portal *Portal) knowsDoFirstBatch(ctx context.Context, source *UserLogin, limit int) (oldest time.Time, more bool, firstDone func()) {
	log := zerolog.Ctx(ctx).With().Str("action", "forward backfill").Str("knows_pass", "first_batch").Logger()
	ctx = log.WithContext(ctx)
	if !portal.forwardBackfillLock.TryLock() {
		log.Warn().Msg("Another forward backfill is already running")
		return time.Time{}, false, nil
	}
	defer portal.forwardBackfillLock.Unlock()
	api, ok := source.Client.(BackfillingNetworkAPI)
	if !ok {
		log.Debug().Msg("Network API does not support backfilling")
		return time.Time{}, false, nil
	}
	portal.backfillLock.Lock()
	defer portal.backfillLock.Unlock()
	log.Info().Int("count", KnowsFirstBatchSize).Msg("Fetching first batch for forward backfill")
	resp, err := api.FetchMessages(ctx, FetchMessagesParams{
		Portal:          portal,
		Forward:         true,
		Count:           KnowsFirstBatchSize,
		KnowsFirstBatch: true,
	})
	if err != nil {
		log.WithLevel(FailureLogLevel()).Err(err).Msg("Failed to fetch first batch for forward backfill")
		reportBackfillFailure(ctx, "forward_fetch", err)
		// The second pass fetches the whole history, as without the opt-in.
		return time.Time{}, true, nil
	} else if resp == nil {
		log.Debug().Msg("Didn't get backfill response")
		return time.Time{}, false, nil
	} else if len(resp.Messages) == 0 {
		log.Debug().Msg("No messages to backfill")
		return time.Time{}, false, resp.CompleteCallback
	}
	fetched := len(resp.Messages)
	// A full first batch means there may be more; a short one (or a connector that
	// ignored Count and gave the whole history) does not need a second fetch.
	more = fetched >= KnowsFirstBatchSize && fetched < limit
	resp.Messages = portal.cutoffMessages(ctx, resp.Messages, resp.AggressiveDeduplication, true, nil)
	if len(resp.Messages) == 0 {
		return time.Time{}, more, resp.CompleteCallback
	}
	err = portal.sendBackfill(ctx, source, resp.Messages, true, resp.MarkRead, false, nil)
	if err != nil {
		log.WithLevel(FailureLogLevel()).Err(err).Msg("Failed to send first batch of forward backfill")
		reportBackfillFailure(ctx, "forward_send", err)
		return time.Time{}, true, nil
	} else if !resp.MarkRead && !resp.ReadUpTo.IsZero() && !resp.ReadUpTo.Before(resp.Messages[0].Timestamp) &&
		!portal.isPastUnreadThreshold(resp.Messages) {
		portal.markBackfillReadUpTo(ctx, source, resp.ReadUpTo)
	}
	return resp.Messages[0].Timestamp, more, resp.CompleteCallback
}

// knowsDoRestOfHistory is the second pass, outside the event loop: the same
// forward fetch as without the opt-in, of which only the messages not newer
// than the first batch are written, above it. Live messages that arrive
// meanwhile are newer than the first batch and are bridged by the event loop.
// oldest zero (the first pass wrote nothing) writes the whole fetch, above
// anything bridged meanwhile.
func (portal *Portal) knowsDoRestOfHistory(ctx context.Context, source *UserLogin, limit int, oldest time.Time) {
	log := zerolog.Ctx(ctx).With().Str("action", "forward backfill").Str("knows_pass", "rest").Logger()
	ctx = log.WithContext(ctx)
	api, ok := source.Client.(BackfillingNetworkAPI)
	if !ok || portal.backgroundCtx.Err() != nil {
		return
	}
	// Serialized with the backward backfill tasks of the portal, as every fetch
	// of a portal is. The forward lock is left to live catch-up backfills.
	portal.backfillLock.Lock()
	defer portal.backfillLock.Unlock()
	log.Info().Int("count", limit).Msg("Fetching the rest of the initial history")
	resp, err := api.FetchMessages(ctx, FetchMessagesParams{
		Portal:  portal,
		Forward: true,
		Count:   limit,
	})
	if err != nil {
		log.WithLevel(FailureLogLevel()).Err(err).Msg("Failed to fetch the rest of the initial history")
		reportBackfillFailure(ctx, "forward_fetch", err)
		return
	} else if resp == nil {
		return
	}
	messages := resp.Messages
	if !oldest.IsZero() {
		kept := messages[:0:0]
		for _, msg := range messages {
			if !msg.Timestamp.After(oldest) {
				kept = append(kept, msg)
			}
		}
		messages = kept
	}
	// Deduplicated by ID only, like a KnowsFillGap batch: the first batch and
	// anything bridged meanwhile are already in the database.
	messages = portal.cutoffMessages(ctx, messages, true, oldest.IsZero(), nil)
	if len(messages) == 0 {
		log.Debug().Msg("Nothing older than the first batch to backfill")
		if resp.CompleteCallback != nil {
			resp.CompleteCallback()
		}
		return
	}
	// Always a backward batch: when the first pass wrote nothing, live messages
	// may already be in the room, and the history goes above them.
	err = portal.sendBackfill(ctx, source, messages, false, resp.MarkRead, false, resp.CompleteCallback)
	if err != nil {
		log.WithLevel(FailureLogLevel()).Err(err).Msg("Failed to send the rest of the initial history")
		reportBackfillFailure(ctx, "forward_send", err)
	} else if !resp.MarkRead && !resp.ReadUpTo.IsZero() && !resp.ReadUpTo.Before(messages[0].Timestamp) &&
		!portal.isPastUnreadThreshold(messages) && (oldest.IsZero() || resp.ReadUpTo.Before(oldest)) {
		portal.markBackfillReadUpTo(ctx, source, resp.ReadUpTo)
	}
}

// knowsMakeBackfillTaskDue makes the portal's backward backfill task due now,
// instead of BackfillMinBackoffAfterRoomCreate after the room was created: the
// forward backfill it waited for is over.
func (portal *Portal) knowsMakeBackfillTaskDue(ctx context.Context, source *UserLogin) {
	if !portal.Bridge.Config.Backfill.Queue.Enabled || portal.backgroundCtx.Err() != nil {
		return
	}
	task, err := portal.Bridge.DB.BackfillTask.GetNextForPortal(ctx, portal.PortalKey, false)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to get backfill task after forward backfill")
		return
	} else if task == nil || task.QueueDone || !task.DispatchedAt.IsZero() {
		return
	}
	now := time.Now()
	if task.NextDispatchMinTS.After(now) && task.NextDispatchMinTS != database.BackfillNextDispatchNever {
		task.NextDispatchMinTS = now
		if err = portal.Bridge.DB.BackfillTask.Update(ctx, task); err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to make backfill task due after forward backfill")
			return
		}
	}
	portal.Bridge.WakeupBackfillQueue()
}
