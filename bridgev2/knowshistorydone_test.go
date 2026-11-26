package bridgev2

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/bridgev2/database"
)

// historyDoneHits are the history-done portal-ready posts the server got.
func historyDoneHits(srv *knowsCallbackServer) []map[string]any {
	var out []map[string]any
	for _, h := range srv.got() {
		if h.path == KnowsPortalReadyPath && h.body["history_done"] != nil {
			out = append(out, h.body)
		}
	}
	return out
}

func historyDoneEnv(t *testing.T, on bool) (*knowsTestEnv, *knowsCallbackServer) {
	t.Helper()
	if on {
		t.Setenv(KnowsPortalReadyHistoryDoneEnv, "1")
	} else {
		t.Setenv(KnowsPortalReadyHistoryDoneEnv, "")
	}
	srv := newKnowsCallbackServer(t)
	env := newKnowsTestEnv(t)
	env.br.KnowsCallbacks = newTestKnowsCallbacks(t, srv)
	return env, srv
}

// historyDoneFetcher answers forward fetches from history and backward ones with backward.
func historyDoneFetcher(history []*BackfillMessage, backward *FetchMessagesResponse) *knowsFetcher {
	return &knowsFetcher{fetch: func(_ context.Context, params FetchMessagesParams) (*FetchMessagesResponse, error) {
		if !params.Forward {
			return backward, nil
		}
		return &FetchMessagesResponse{Messages: latest(history, params.Count), MarkRead: true}, nil
	}}
}

func TestKnowsHistoryDoneBodyIsTheContract(t *testing.T) {
	t.Setenv(KnowsPortalReadyHistoryDoneEnv, "1")
	srv := newKnowsCallbackServer(t)
	kc := newTestKnowsCallbacks(t, srv)
	require.True(t, kc.HistoryDoneOn())
	require.True(t, kc.PortalHistoryDone("login-1", "thread-1", "!room:test", 0))
	require.False(t, kc.PortalHistoryDone("login-1", "thread-1", "!room:test", 3), "once per room")
	require.True(t, kc.PortalHistoryDone("login-1", "thread-2", "!other:test", 7))
	kc.Wait()
	hits := srv.got()
	require.Len(t, hits, 2)
	bodies := map[string]map[string]any{}
	for _, h := range hits {
		require.Equal(t, KnowsPortalReadyPath, h.path)
		bodies[h.body["room_id"].(string)] = h.body
	}
	// The fields of mautrix-meta-custom's PortalReadyBody and knows-api's PortalReady model.
	require.Equal(t, map[string]any{
		"login_id": "login-1", "thread_id": "thread-1", "room_id": "!room:test",
		"first_batch": true, "history_done": true, "messages": float64(0),
	}, bodies["!room:test"], "messages 0 is sent")
	require.Equal(t, float64(7), bodies["!other:test"]["messages"])
}

func TestKnowsPortalReadyBodyWithoutHistoryDoneIsUnchanged(t *testing.T) {
	data, err := json.Marshal(knowsPortalReadyBody{LoginID: "l", ThreadID: "t", RoomID: "!r:test", FirstBatch: true})
	require.NoError(t, err)
	require.JSONEq(t, `{"login_id":"l","thread_id":"t","room_id":"!r:test","first_batch":true}`, string(data))
	require.Equal(t, `{"login_id":"l","thread_id":"t","room_id":"!r:test","first_batch":true}`, string(data))
}

func TestKnowsHistoryDoneIsOffByDefault(t *testing.T) {
	t.Setenv(KnowsPortalReadyHistoryDoneEnv, "")
	srv := newKnowsCallbackServer(t)
	kc := newTestKnowsCallbacks(t, srv)
	require.False(t, kc.HistoryDoneOn())
	require.False(t, kc.PortalHistoryDone("login-1", "thread-1", "!room:test", 0))
	var nilKC *KnowsCallbacks
	require.False(t, nilKC.HistoryDoneOn())
	require.False(t, nilKC.PortalHistoryDone("login-1", "thread-1", "!room:test", 0))
	kc.Wait()
	require.Empty(t, srv.got())
}

func TestKnowsHistoryDoneAfterTheForwardWithoutQueue(t *testing.T) {
	env, srv := historyDoneEnv(t, true)
	env.source.Client = historyDoneFetcher(knowsHistory(3), nil)
	env.portal.knowsCreateForwardBackfill(env.ctx, env.source, nil)
	env.br.KnowsCallbacks.Wait()
	hits := historyDoneHits(srv)
	require.Len(t, hits, 1, "no backfill queue: nothing more comes after the forward backfill")
	require.Equal(t, float64(3), hits[0]["messages"])
	require.Equal(t, "!room:test", hits[0]["room_id"])
	require.Equal(t, true, hits[0]["first_batch"])
}

func TestKnowsHistoryDoneWaitsForTheBackwardTask(t *testing.T) {
	env, srv := historyDoneEnv(t, true)
	env.br.Config.Backfill.Queue.Enabled = true
	env.br.Config.Backfill.Queue.BatchSize = 100
	env.br.Config.Backfill.Queue.MaxBatches = -1
	env.source.Client = historyDoneFetcher(knowsHistory(4), &FetchMessagesResponse{HasMore: false})
	require.NoError(t, env.br.DB.BackfillTask.Upsert(env.ctx, &database.BackfillTask{
		PortalKey: env.portal.PortalKey, UserLoginID: env.source.ID,
		NextDispatchMinTS: time.Now().Add(BackfillMinBackoffAfterRoomCreate),
	}))
	env.portal.knowsCreateForwardBackfill(env.ctx, env.source, nil)
	env.br.KnowsCallbacks.Wait()
	require.Empty(t, historyDoneHits(srv), "a backward task is still to run")

	task, err := env.br.DB.BackfillTask.GetNextForPortal(env.ctx, env.portal.PortalKey, false)
	require.NoError(t, err)
	_, err = env.portal.knowsDoBackfillTask(env.ctx, env.source, task, nil)
	require.NoError(t, err)
	require.True(t, task.IsDone)
	env.br.KnowsCallbacks.Wait()
	hits := historyDoneHits(srv)
	require.Len(t, hits, 1, "the backward task found the start of the chat")
	require.Equal(t, float64(4), hits[0]["messages"])

	// A later run of the done task (a manual backfill) tells nothing again.
	_, _ = env.portal.knowsDoBackfillTask(env.ctx, env.source, task, &FetchMessagesResponse{HasMore: false})
	env.br.KnowsCallbacks.Wait()
	require.Len(t, historyDoneHits(srv), 1)
}

func TestKnowsHistoryDoneNotWhileOlderPagesRemain(t *testing.T) {
	env, srv := historyDoneEnv(t, true)
	env.br.Config.Backfill.Queue.Enabled = true
	env.br.Config.Backfill.Queue.BatchSize = 100
	env.br.Config.Backfill.Queue.MaxBatches = -1
	older := knowsHistory(1)
	older[0].ID = "m000"
	older[0].Timestamp = time.Unix(1_600_000_000, 0)
	env.source.Client = historyDoneFetcher(knowsHistory(4), &FetchMessagesResponse{Messages: older, HasMore: true, Cursor: "next"})
	require.NoError(t, env.br.DB.BackfillTask.Upsert(env.ctx, &database.BackfillTask{
		PortalKey: env.portal.PortalKey, UserLoginID: env.source.ID, NextDispatchMinTS: time.Now(),
	}))
	env.portal.knowsCreateForwardBackfill(env.ctx, env.source, nil)
	task, err := env.br.DB.BackfillTask.GetNextForPortal(env.ctx, env.portal.PortalKey, false)
	require.NoError(t, err)
	_, err = env.portal.knowsDoBackfillTask(env.ctx, env.source, task, nil)
	require.NoError(t, err)
	require.False(t, task.IsDone)
	env.br.KnowsCallbacks.Wait()
	require.Empty(t, historyDoneHits(srv), "the network has older pages")
}

func TestKnowsHistoryDoneNotForATaskWithoutLogin(t *testing.T) {
	env, srv := historyDoneEnv(t, true)
	env.br.Config.Backfill.Queue.Enabled = true
	env.source.Client = historyDoneFetcher(knowsHistory(2), nil)
	require.NoError(t, env.br.DB.BackfillTask.Upsert(env.ctx, &database.BackfillTask{
		PortalKey: env.portal.PortalKey, NextDispatchMinTS: database.BackfillNextDispatchNever,
	}))
	env.portal.knowsCreateForwardBackfill(env.ctx, env.source, nil)
	env.br.KnowsCallbacks.Wait()
	require.Empty(t, historyDoneHits(srv), "unsure: knows-api keeps its cap")
}

func TestKnowsHistoryDoneSwitchOffIsTheOldBehaviour(t *testing.T) {
	for name, queue := range map[string]bool{"without queue": false, "with queue": true} {
		t.Run(name, func(t *testing.T) {
			env, srv := historyDoneEnv(t, false)
			env.br.Config.Backfill.Queue.Enabled = queue
			env.br.Config.Backfill.Queue.BatchSize = 100
			env.br.Config.Backfill.Queue.MaxBatches = -1
			env.source.Client = historyDoneFetcher(knowsHistory(3), &FetchMessagesResponse{HasMore: false})
			require.NoError(t, env.br.DB.BackfillTask.Upsert(env.ctx, &database.BackfillTask{
				PortalKey: env.portal.PortalKey, UserLoginID: env.source.ID, NextDispatchMinTS: time.Now(),
			}))
			env.portal.knowsCreateForwardBackfill(env.ctx, env.source, nil)
			task, err := env.br.DB.BackfillTask.GetNextForPortal(env.ctx, env.portal.PortalKey, false)
			require.NoError(t, err)
			_, err = env.portal.knowsDoBackfillTask(env.ctx, env.source, task, nil)
			require.NoError(t, err)
			env.br.KnowsCallbacks.Wait()
			require.Empty(t, historyDoneHits(srv))
			// Only the first-batch portal-ready of today, with today's body.
			hits := srv.got()
			require.Len(t, hits, 1)
			require.Equal(t, map[string]any{"login_id": "login-1", "thread_id": "thread-1", "room_id": "!room:test", "first_batch": true}, hits[0].body)
		})
	}
}

// loggedInFetcher is a knowsFetcher the backfill queue can dispatch to.
type loggedInFetcher struct{ *knowsFetcher }

func (loggedInFetcher) IsLoggedIn() bool { return true }

func TestKnowsHistoryDoneFromTheBackfillQueue(t *testing.T) {
	env, srv := historyDoneEnv(t, true)
	env.br.Config.Backfill.Queue.Enabled = true
	env.br.Config.Backfill.Queue.BatchSize = 100
	env.br.Config.Backfill.Queue.MaxBatches = -1
	env.source.Client = loggedInFetcher{historyDoneFetcher(knowsHistory(2), nil)}
	env.br.userLoginsByID[env.source.ID] = env.source
	require.NoError(t, env.br.DB.BackfillTask.Upsert(env.ctx, &database.BackfillTask{
		PortalKey: env.portal.PortalKey, UserLoginID: env.source.ID, NextDispatchMinTS: time.Now().Add(-time.Second),
	}))
	env.portal.knowsCreateForwardBackfill(env.ctx, env.source, nil)
	task, err := env.br.DB.BackfillTask.GetNext(env.ctx)
	require.NoError(t, err)
	require.NotNil(t, task)
	// The backward fetch answers nothing: the queue marks the task done.
	env.br.DoBackfillTask(env.ctx, task)
	env.br.KnowsCallbacks.Wait()
	hits := historyDoneHits(srv)
	require.Len(t, hits, 1)
	require.Equal(t, float64(2), hits[0]["messages"])
	saved, err := env.br.DB.BackfillTask.GetNextForPortal(env.ctx, env.portal.PortalKey, true)
	require.NoError(t, err)
	require.True(t, saved.IsDone)
}
