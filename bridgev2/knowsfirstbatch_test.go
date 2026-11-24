package bridgev2

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/bridgev2/database"
)

type knowsFetchCall struct {
	count      int
	firstBatch bool
}

// knowsRecorder fetches from a fixed history and records each call.
type knowsRecorder struct {
	mu      sync.Mutex
	calls   []knowsFetchCall
	history []*BackfillMessage
	// gate, when set for a call index, is waited on before that call answers.
	gate map[int]chan struct{}
	err  map[int]error
}

func (r *knowsRecorder) fetch(ctx context.Context, params FetchMessagesParams) (*FetchMessagesResponse, error) {
	r.mu.Lock()
	idx := len(r.calls)
	r.calls = append(r.calls, knowsFetchCall{count: params.Count, firstBatch: params.KnowsFirstBatch})
	gate := r.gate[idx]
	err := r.err[idx]
	r.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return &FetchMessagesResponse{Messages: latest(r.history, params.Count), MarkRead: true}, nil
}

func (r *knowsRecorder) callList() []knowsFetchCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]knowsFetchCall(nil), r.calls...)
}

func knowsRun(env *knowsTestEnv, ctx context.Context, answered func(error)) chan struct{} {
	done := make(chan struct{})
	cb := knowsOnce(answered)
	go func() {
		defer close(done)
		env.portal.knowsCreateForwardBackfill(knowsWithRoomReady(ctx, cb), env.source, nil)
		cb(nil)
	}()
	return done
}

func TestKnowsFirstBatchAnswersAtTheFirstBatchAndWritesTheRestAbove(t *testing.T) {
	env := newKnowsTestEnv(t)
	rec := &knowsRecorder{history: knowsHistory(120), gate: map[int]chan struct{}{1: make(chan struct{})}}
	env.source.Client = &knowsFetcher{fetch: rec.fetch}
	historyDone := make(chan struct{})
	ctx := WithKnowsFirstBatch(env.ctx, func() { close(historyDone) })

	answered := make(chan error, 2)
	loopDone := knowsRun(env, ctx, func(err error) { answered <- err })

	select {
	case err := <-answered:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("the caller was not answered at the first batch")
	}
	<-loopDone
	require.True(t, KnowsAnsweredAtFirstBatch(ctx), "the answer says the rest of the history is still coming")
	// The first batch is written, the second pass is still fetching.
	require.Equal(t, []knowsBatch{{forward: true, ids: knowsEventIDs(71, 120)}}, env.matrix.sent())
	select {
	case <-historyDone:
		t.Fatal("history done before the second pass")
	default:
	}

	close(rec.gate[1])
	select {
	case <-historyDone:
	case <-time.After(3 * time.Second):
		t.Fatal("history never done")
	}
	require.Equal(t, []knowsFetchCall{{count: KnowsFirstBatchSize, firstBatch: true}, {count: 500}}, rec.callList())
	require.Equal(t, []knowsBatch{
		{forward: true, ids: knowsEventIDs(71, 120)},
		{forward: false, ids: knowsEventIDs(1, 70)},
	}, env.matrix.sent(), "the rest is a backward batch of only the older messages, each once")
	var n int
	require.NoError(t, env.raw.QueryRow(`SELECT COUNT(*) FROM message WHERE room_id='thread-1'`).Scan(&n))
	require.Equal(t, 120, n)
}

func TestKnowsFirstBatchWaitsAtMostTheCap(t *testing.T) {
	env := newKnowsTestEnv(t)
	old := KnowsFirstBatchMaxWait
	KnowsFirstBatchMaxWait = 50 * time.Millisecond
	t.Cleanup(func() { KnowsFirstBatchMaxWait = old })
	rec := &knowsRecorder{history: knowsHistory(80), gate: map[int]chan struct{}{0: make(chan struct{})}}
	env.source.Client = &knowsFetcher{fetch: rec.fetch}
	historyDone := make(chan struct{})
	ctx, cancel := context.WithCancel(WithKnowsFirstBatch(env.ctx, func() { close(historyDone) }))

	answered := make(chan error, 2)
	start := time.Now()
	loopDone := knowsRun(env, ctx, func(err error) { answered <- err })
	select {
	case err := <-answered:
		require.NoError(t, err)
		require.Less(t, time.Since(start), 2*time.Second)
	case <-time.After(3 * time.Second):
		t.Fatal("the cap did not answer the caller")
	}
	require.True(t, KnowsAnsweredAtFirstBatch(ctx))
	require.Empty(t, env.matrix.sent(), "answered with the room still empty")
	// The caller is gone (its request context ends); the first batch still lands.
	cancel()
	close(rec.gate[0])
	<-loopDone
	<-historyDone
	require.Equal(t, []knowsBatch{
		{forward: true, ids: knowsEventIDs(31, 80)},
		{forward: false, ids: knowsEventIDs(1, 30)},
	}, env.matrix.sent())
}

func TestKnowsFirstBatchWithoutOptInIsOneFetchAnsweredAtTheEnd(t *testing.T) {
	env := newKnowsTestEnv(t)
	rec := &knowsRecorder{history: knowsHistory(120)}
	env.source.Client = &knowsFetcher{fetch: rec.fetch}
	var answeredEarly atomic.Bool
	sentAtAnswer := -1
	cb := knowsOnce(func(error) { sentAtAnswer = len(env.matrix.sent()) })
	ctx := knowsWithRoomReady(env.ctx, cb)
	env.portal.knowsCreateForwardBackfill(ctx, env.source, nil)
	answeredEarly.Store(sentAtAnswer >= 0)
	cb(nil)
	require.False(t, answeredEarly.Load(), "no early answer without the opt-in")
	require.Equal(t, 1, sentAtAnswer)
	require.Equal(t, []knowsFetchCall{{count: 500}}, rec.callList())
	require.Equal(t, []knowsBatch{{forward: true, ids: knowsEventIDs(1, 120)}}, env.matrix.sent())
	require.False(t, KnowsAnsweredAtFirstBatch(ctx))
}

func TestKnowsFirstBatchSmallLimitIsOnePass(t *testing.T) {
	env := newKnowsTestEnv(t)
	env.br.Config.Backfill.MaxInitialMessages = 30
	rec := &knowsRecorder{history: knowsHistory(120)}
	env.source.Client = &knowsFetcher{fetch: rec.fetch}
	historyDone := make(chan struct{})
	ctx := WithKnowsFirstBatch(env.ctx, func() { close(historyDone) })
	<-knowsRun(env, ctx, func(error) {})
	<-historyDone
	require.Equal(t, []knowsFetchCall{{count: 30}}, rec.callList())
	require.False(t, KnowsAnsweredAtFirstBatch(ctx))
}

func TestKnowsFirstBatchFetchErrorFallsBackToTheWholeHistory(t *testing.T) {
	env := newKnowsTestEnv(t)
	rec := &knowsRecorder{history: knowsHistory(80), err: map[int]error{0: errors.New("rate limited")}}
	env.source.Client = &knowsFetcher{fetch: rec.fetch}
	historyDone := make(chan struct{})
	ctx := WithKnowsFirstBatch(env.ctx, func() { close(historyDone) })
	<-knowsRun(env, ctx, func(error) {})
	<-historyDone
	require.Equal(t, []knowsBatch{{forward: true, ids: knowsEventIDs(1, 80)}}, env.matrix.sent())
}

func TestKnowsFirstBatchShortChatIsNotFetchedTwice(t *testing.T) {
	env := newKnowsTestEnv(t)
	rec := &knowsRecorder{history: knowsHistory(0)}
	env.source.Client = &knowsFetcher{fetch: rec.fetch}
	historyDone := make(chan struct{})
	ctx := WithKnowsFirstBatch(env.ctx, func() { close(historyDone) })
	<-knowsRun(env, ctx, func(error) {})
	<-historyDone
	require.Len(t, rec.callList(), 1, "an empty chat has nothing more to fetch")
}

func TestKnowsFirstBatchLiveMessageDuringTheRestIsNotWrittenTwice(t *testing.T) {
	env := newKnowsTestEnv(t)
	rec := &knowsRecorder{history: knowsHistory(100), gate: map[int]chan struct{}{1: make(chan struct{})}}
	env.source.Client = &knowsFetcher{fetch: rec.fetch}
	historyDone := make(chan struct{})
	ctx := WithKnowsFirstBatch(env.ctx, func() { close(historyDone) })
	<-knowsRun(env, ctx, func(error) {})
	// A live message arrives while the second pass fetches; the platform now has 101 messages.
	rec.mu.Lock()
	rec.history = append(rec.history, knowsHistory(101)[100])
	rec.mu.Unlock()
	close(rec.gate[1])
	<-historyDone
	require.Equal(t, []knowsBatch{
		{forward: true, ids: knowsEventIDs(51, 100)},
		{forward: false, ids: knowsEventIDs(1, 50)},
	}, env.matrix.sent(), "the live message is newer than the first batch: it is left to the event loop")
}

func TestKnowsFirstBatchMakesTheBackfillTaskDue(t *testing.T) {
	env := newKnowsTestEnv(t)
	env.br.Config.Backfill.Queue.Enabled = true
	rec := &knowsRecorder{history: knowsHistory(80)}
	env.source.Client = &knowsFetcher{fetch: rec.fetch}
	future := time.Now().Add(BackfillMinBackoffAfterRoomCreate)
	require.NoError(t, env.br.DB.BackfillTask.Upsert(env.ctx, &database.BackfillTask{
		PortalKey: env.portal.PortalKey, UserLoginID: env.source.ID, NextDispatchMinTS: future,
	}))
	historyDone := make(chan struct{})
	ctx := WithKnowsFirstBatch(env.ctx, func() { close(historyDone) })
	<-knowsRun(env, ctx, func(error) {})
	<-historyDone
	require.Eventually(t, func() bool {
		task, err := env.br.DB.BackfillTask.GetNextForPortal(env.ctx, env.portal.PortalKey, false)
		return err == nil && task != nil && task.NextDispatchMinTS.Before(future.Add(-30*time.Second))
	}, 2*time.Second, 10*time.Millisecond, "the task is due once the forward backfill is over")
	select {
	case <-env.br.wakeupBackfillQueue:
	case <-time.After(time.Second):
		t.Fatal("the queue was not woken")
	}

	// Without the opt-in the task keeps its one-minute delay.
	env2 := newKnowsTestEnv(t)
	env2.br.Config.Backfill.Queue.Enabled = true
	env2.source.Client = &knowsFetcher{fetch: (&knowsRecorder{history: knowsHistory(80)}).fetch}
	require.NoError(t, env2.br.DB.BackfillTask.Upsert(env2.ctx, &database.BackfillTask{
		PortalKey: env2.portal.PortalKey, UserLoginID: env2.source.ID, NextDispatchMinTS: future,
	}))
	env2.portal.knowsCreateForwardBackfill(env2.ctx, env2.source, nil)
	task, err := env2.br.DB.BackfillTask.GetNextForPortal(env2.ctx, env2.portal.PortalKey, false)
	require.NoError(t, err)
	require.WithinDuration(t, future, task.NextDispatchMinTS, time.Millisecond)
}

func TestKnowsOnceAnswersTheCreatorOnce(t *testing.T) {
	var calls []error
	cb := knowsOnce(func(err error) { calls = append(calls, err) })
	ready := knowsRoomReadyFromContext(knowsWithRoomReady(WithKnowsFirstBatch(context.Background(), nil), cb))
	ready()
	cb(errors.New("late"))
	require.Equal(t, []error{nil}, calls)
	// Without the opt-in there is no early answer to give.
	knowsRoomReadyFromContext(knowsWithRoomReady(context.Background(), cb))()
	require.Len(t, calls, 1)
}
