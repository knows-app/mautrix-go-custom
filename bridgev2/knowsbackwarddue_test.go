package bridgev2

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/bridgev2/database"
)

// backwardDueEnv is a new room with its backward task written as at room creation.
func backwardDueEnv(t *testing.T, on bool, fetch func(context.Context, FetchMessagesParams) (*FetchMessagesResponse, error)) (*knowsTestEnv, time.Time) {
	t.Helper()
	if on {
		t.Setenv(KnowsBackwardDueNowEnv, "1")
	} else {
		t.Setenv(KnowsBackwardDueNowEnv, "")
	}
	env := newKnowsTestEnv(t)
	env.br.Config.Backfill.Queue.Enabled = true
	if fetch == nil {
		fetch = (&knowsRecorder{history: knowsHistory(30)}).fetch
	}
	env.source.Client = &knowsFetcher{fetch: fetch}
	created := time.Now().Add(BackfillMinBackoffAfterRoomCreate)
	require.NoError(t, env.br.DB.BackfillTask.Upsert(env.ctx, &database.BackfillTask{
		PortalKey: env.portal.PortalKey, UserLoginID: env.source.ID, NextDispatchMinTS: created,
	}))
	return env, created
}

func backwardTask(t *testing.T, env *knowsTestEnv) *database.BackfillTask {
	t.Helper()
	task, err := env.br.DB.BackfillTask.GetNextForPortal(env.ctx, env.portal.PortalKey, true)
	require.NoError(t, err)
	require.NotNil(t, task)
	return task
}

func woken(env *knowsTestEnv) bool {
	select {
	case <-env.br.wakeupBackfillQueue:
		return true
	default:
		return false
	}
}

func TestKnowsBackwardDueNowAfterTheForwardBackfill(t *testing.T) {
	env, created := backwardDueEnv(t, true, nil)
	before := time.Now()
	env.portal.knowsCreateForwardBackfill(env.ctx, env.source, nil)
	task := backwardTask(t, env)
	require.False(t, task.NextDispatchMinTS.After(time.Now()), "due once the forward backfill is over")
	require.False(t, task.NextDispatchMinTS.Before(before))
	require.True(t, created.Sub(task.NextDispatchMinTS) > 50*time.Second)
	require.True(t, woken(env), "the queue is woken")
	require.Len(t, env.matrix.sent(), 1, "the forward backfill itself is unchanged")
}

func TestKnowsBackwardDueNowOffKeepsTheMinute(t *testing.T) {
	env, created := backwardDueEnv(t, false, nil)
	env.portal.knowsCreateForwardBackfill(env.ctx, env.source, nil)
	task := backwardTask(t, env)
	require.WithinDuration(t, created, task.NextDispatchMinTS, time.Millisecond, "today: BackfillMinBackoffAfterRoomCreate")
	require.False(t, woken(env))
	require.Len(t, env.matrix.sent(), 1)
}

func TestKnowsBackwardDueNowWaitsForTheForwardFetch(t *testing.T) {
	gate := make(chan struct{})
	rec := &knowsRecorder{history: knowsHistory(30), gate: map[int]chan struct{}{0: gate}}
	env, created := backwardDueEnv(t, true, rec.fetch)
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.portal.knowsCreateForwardBackfill(env.ctx, env.source, nil)
	}()
	require.Eventually(t, func() bool { return len(rec.callList()) == 1 }, 2*time.Second, time.Millisecond)
	require.WithinDuration(t, created, backwardTask(t, env).NextDispatchMinTS, time.Millisecond,
		"not due while the forward backfill runs")
	close(gate)
	<-done
	require.False(t, backwardTask(t, env).NextDispatchMinTS.After(time.Now()))
}

func TestKnowsBackwardDueNowLeavesTheQueueRules(t *testing.T) {
	// The queue does not run: nothing changes.
	env, created := backwardDueEnv(t, true, nil)
	env.br.Config.Backfill.Queue.Enabled = false
	env.portal.knowsCreateForwardBackfill(env.ctx, env.source, nil)
	require.WithinDuration(t, created, backwardTask(t, env).NextDispatchMinTS, time.Millisecond)

	// A task already dispatched keeps its own schedule.
	env, _ = backwardDueEnv(t, true, nil)
	task := backwardTask(t, env)
	dispatched, err := env.br.DB.BackfillTask.MarkDispatched(env.ctx, task)
	require.NoError(t, err)
	require.True(t, dispatched)
	after := backwardTask(t, env)
	env.portal.knowsCreateForwardBackfill(env.ctx, env.source, nil)
	require.WithinDuration(t, after.NextDispatchMinTS, backwardTask(t, env).NextDispatchMinTS, time.Millisecond)

	// A task with no login to run it stays never due.
	env, _ = backwardDueEnv(t, true, nil)
	task = backwardTask(t, env)
	task.NextDispatchMinTS = database.BackfillNextDispatchNever
	require.NoError(t, env.br.DB.BackfillTask.Update(env.ctx, task))
	env.portal.knowsCreateForwardBackfill(env.ctx, env.source, nil)
	require.Equal(t, database.BackfillNextDispatchNever, backwardTask(t, env).NextDispatchMinTS)
}

func TestKnowsBackwardDueNowWithFirstBatchIsUnchanged(t *testing.T) {
	// The first-batch path already makes the task due; the switch adds nothing to it.
	for _, on := range []bool{false, true} {
		env, created := backwardDueEnv(t, on, nil)
		historyDone := make(chan struct{})
		ctx := WithKnowsFirstBatch(env.ctx, func() { close(historyDone) })
		<-knowsRun(env, ctx, func(error) {})
		<-historyDone
		require.True(t, created.Sub(backwardTask(t, env).NextDispatchMinTS) > 50*time.Second)
	}
}
