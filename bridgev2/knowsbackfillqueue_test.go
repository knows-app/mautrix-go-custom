package bridgev2

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

func TestKnowsBackfillQueueDelay(t *testing.T) {
	now := time.Now()
	batch := 5 * time.Second
	require.Equal(t, batch, knowsBackfillQueueDelay(batch, 0, time.Time{}, now), "a queue that just found a task keeps batch_delay")
	require.Equal(t, 10*time.Second, knowsBackfillQueueDelay(batch, 1, time.Time{}, now))
	require.Equal(t, 15*time.Second, knowsBackfillQueueDelay(batch, 2, time.Time{}, now))
	require.Equal(t, KnowsBackfillQueueMaxIdleWait, knowsBackfillQueueDelay(batch, 1000, time.Time{}, now), "capped")
	require.Equal(t, 7*time.Second, knowsBackfillQueueDelay(batch, 1000, now.Add(7*time.Second), now), "never past the next due task")
	require.Equal(t, batch, knowsBackfillQueueDelay(batch, 1000, now.Add(-time.Second), now), "a task already due waits batch_delay")
	require.Equal(t, batch, knowsBackfillQueueDelay(batch, 1000, now.Add(time.Second), now), "never below batch_delay")
}

func TestKnowsNextDispatchMinTS(t *testing.T) {
	env := newKnowsTestEnv(t)
	ctx := env.ctx
	got, err := env.br.DB.BackfillTask.GetNextDispatchMinTS(ctx)
	require.NoError(t, err)
	require.True(t, got.IsZero(), "no task")

	soon := time.Now().Add(40 * time.Second)
	later := time.Now().Add(time.Hour)
	insertPortal := func(id string) networkid.PortalKey {
		_, err := env.raw.Exec(`INSERT INTO portal (bridge_id, id, receiver, mxid, other_user_id, name, topic, avatar_id, avatar_hash, avatar_mxc,
			name_set, avatar_set, topic_set, name_is_custom, in_space, room_type, metadata)
			VALUES ('test', ?, 'login-1', ?, '', '', '', '', '', '', false, false, false, false, false, 'dm', '{}')`, id, "!"+id+":test")
		require.NoError(t, err)
		return networkid.PortalKey{ID: networkid.PortalID(id), Receiver: "login-1"}
	}
	require.NoError(t, env.br.DB.BackfillTask.Upsert(ctx, &database.BackfillTask{PortalKey: insertPortal("p-later"), UserLoginID: "login-1", NextDispatchMinTS: later}))
	require.NoError(t, env.br.DB.BackfillTask.Upsert(ctx, &database.BackfillTask{PortalKey: insertPortal("p-soon"), UserLoginID: "login-1", NextDispatchMinTS: soon}))
	require.NoError(t, env.br.DB.BackfillTask.Upsert(ctx, &database.BackfillTask{PortalKey: insertPortal("p-done"), UserLoginID: "login-1", NextDispatchMinTS: time.Now(), IsDone: true}))
	require.NoError(t, env.br.DB.BackfillTask.Upsert(ctx, &database.BackfillTask{PortalKey: insertPortal("p-never"), UserLoginID: "login-1", NextDispatchMinTS: database.BackfillNextDispatchNever}))
	require.NoError(t, env.br.DB.BackfillTask.Upsert(ctx, &database.BackfillTask{PortalKey: insertPortal("p-nologin"), NextDispatchMinTS: time.Now()}))
	got, err = env.br.DB.BackfillTask.GetNextDispatchMinTS(ctx)
	require.NoError(t, err)
	require.WithinDuration(t, soon, got, time.Millisecond)
}
