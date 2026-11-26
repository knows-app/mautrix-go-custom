package bridgev2

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/bridgev2/database"
)

func TestKnowsContactListStateReleasesEveryWaiterOnce(t *testing.T) {
	var state KnowsContactListState
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-state.KnowsContactListReady()
		}()
	}
	_, _, ready := state.KnowsStaged()
	require.False(t, ready)
	var stagers sync.WaitGroup
	for i := range 5 {
		stagers.Add(1)
		go func() { defer stagers.Done(); state.Staged(nil, i, false) }()
	}
	stagers.Wait()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waiters were not released")
	}
	state.Staged(nil, 9, true)
	count, complete, ready := state.KnowsStaged()
	require.Equal(t, 9, count)
	require.True(t, complete)
	require.True(t, ready)
}

func TestKnowsContactListStagedPostsContactsStaged(t *testing.T) {
	srv := newKnowsCallbackServer(t)
	kc := newTestKnowsCallbacks(t, srv)
	login := &UserLogin{UserLogin: &database.UserLogin{ID: "login-1"}, Bridge: &Bridge{KnowsCallbacks: kc}}
	var state KnowsContactListState
	state.Staged(login, 12, false)
	state.Staged(login, 30, true)
	kc.Wait()
	hits := srv.got()
	require.Len(t, hits, 2)
	for _, h := range hits {
		require.Equal(t, KnowsContactsStagedPath, h.path)
	}

	// Callbacks off: Staged still releases the waiters and sends nothing.
	var quiet KnowsContactListState
	quiet.Staged(&UserLogin{UserLogin: &database.UserLogin{ID: "login-2"}, Bridge: &Bridge{}}, 1, false)
	<-quiet.KnowsContactListReady()
}

func TestKnowsRateLimitUntil(t *testing.T) {
	until := time.Now().Add(1500 * time.Millisecond)
	limit := KnowsRateLimitUntil("FI.MAU.LINKEDIN.RATE_LIMITED", until)
	require.Equal(t, "FI.MAU.LINKEDIN.RATE_LIMITED", limit.Errcode)
	require.Equal(t, 2, limit.RetryAfterS, "whole seconds, rounded up")
	require.Equal(t, until.UnixMilli(), limit.UntilMS)
	require.Equal(t, 1, KnowsRateLimitUntil("X", time.Now().Add(time.Millisecond)).RetryAfterS, "at least 1 s")
	require.Nil(t, KnowsRateLimitUntil("X", time.Now().Add(-time.Second)), "a limit in the past is no limit")
}
