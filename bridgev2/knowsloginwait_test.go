package bridgev2

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/status"
)

// knowsConnectingClient is logged in once loggedIn is set.
type knowsConnectingClient struct {
	NetworkAPI
	loggedIn atomic.Bool
}

func (c *knowsConnectingClient) IsLoggedIn() bool { return c.loggedIn.Load() }

func knowsWaitLogin(client NetworkAPI) *UserLogin {
	login := &UserLogin{UserLogin: &database.UserLogin{ID: "login-1"}, Client: client, Log: zerolog.Nop()}
	login.BridgeState = &BridgeStateQueue{login: login, ch: make(chan status.BridgeState, 100)}
	return login
}

func knowsShortLoginWait(t *testing.T) {
	old := knowsLoginStartPoll
	knowsLoginStartPoll = 5 * time.Millisecond
	t.Cleanup(func() { knowsLoginStartPoll = old })
}

func TestKnowsWaitLoggedInAnswersAtOnceWhenLoggedIn(t *testing.T) {
	client := &knowsConnectingClient{}
	client.loggedIn.Store(true)
	start := time.Now()
	require.True(t, knowsWaitLogin(client).KnowsWaitLoggedIn(context.Background(), time.Hour))
	require.Less(t, time.Since(start), 50*time.Millisecond)
}

func TestKnowsWaitLoggedInWaitsForAConnectingLogin(t *testing.T) {
	knowsShortLoginWait(t)
	for _, state := range []status.BridgeStateEvent{"", status.StateConnecting} {
		t.Run(string(state), func(t *testing.T) {
			client := &knowsConnectingClient{}
			login := knowsWaitLogin(client)
			if state != "" {
				login.BridgeState.Send(status.BridgeState{StateEvent: state})
			}
			go func() {
				time.Sleep(80 * time.Millisecond)
				client.loggedIn.Store(true)
			}()
			start := time.Now()
			require.True(t, login.KnowsWaitLoggedIn(context.Background(), 5*time.Second))
			elapsed := time.Since(start)
			require.GreaterOrEqual(t, elapsed, 70*time.Millisecond)
			require.Less(t, elapsed, 2*time.Second, "released by the connection, not by the cap")
		})
	}
}

func TestKnowsWaitLoggedInIsBounded(t *testing.T) {
	knowsShortLoginWait(t)
	login := knowsWaitLogin(&knowsConnectingClient{})
	start := time.Now()
	require.False(t, login.KnowsWaitLoggedIn(context.Background(), 60*time.Millisecond))
	elapsed := time.Since(start)
	require.GreaterOrEqual(t, elapsed, 55*time.Millisecond)
	require.Less(t, elapsed, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	start = time.Now()
	require.False(t, login.KnowsWaitLoggedIn(ctx, time.Hour))
	require.Less(t, time.Since(start), time.Second, "a caller that goes away stops the wait")
}

func TestKnowsWaitLoggedInDoesNotWaitForAStoppedLogin(t *testing.T) {
	knowsShortLoginWait(t)
	for _, state := range []status.BridgeStateEvent{
		status.StateBadCredentials, status.StateLoggedOut, status.StateUnconfigured,
		status.StateTransientDisconnect, status.StateUnknownError,
	} {
		t.Run(string(state), func(t *testing.T) {
			login := knowsWaitLogin(&knowsConnectingClient{})
			login.BridgeState.Send(status.BridgeState{StateEvent: state})
			start := time.Now()
			require.False(t, login.KnowsWaitLoggedIn(context.Background(), time.Hour))
			require.Less(t, time.Since(start), 50*time.Millisecond)
		})
	}
	start := time.Now()
	require.False(t, (&UserLogin{}).KnowsWaitLoggedIn(context.Background(), time.Hour), "no client")
	require.False(t, (*UserLogin)(nil).KnowsWaitLoggedIn(context.Background(), time.Hour))
	require.Less(t, time.Since(start), 50*time.Millisecond)
}

func TestKnowsWaitLoggedInStopsWhenTheLoginFails(t *testing.T) {
	knowsShortLoginWait(t)
	login := knowsWaitLogin(&knowsConnectingClient{})
	login.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnecting})
	go func() {
		time.Sleep(40 * time.Millisecond)
		login.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials})
	}()
	start := time.Now()
	require.False(t, login.KnowsWaitLoggedIn(context.Background(), time.Hour))
	require.Less(t, time.Since(start), time.Second, "bad credentials end the wait")
}

func TestKnowsLoginStartWaitEnv(t *testing.T) {
	t.Setenv(KnowsLoginStartWaitEnv, "")
	require.Equal(t, KnowsLoginStartWaitDefault, KnowsLoginStartWait())
	t.Setenv(KnowsLoginStartWaitEnv, "0")
	require.Zero(t, KnowsLoginStartWait(), "0 turns the wait off")
	t.Setenv(KnowsLoginStartWaitEnv, "1500")
	require.Equal(t, 1500*time.Millisecond, KnowsLoginStartWait())
	t.Setenv(KnowsLoginStartWaitEnv, "nope")
	require.Equal(t, KnowsLoginStartWaitDefault, KnowsLoginStartWait())
	t.Setenv(KnowsLoginStartWaitEnv, "-5")
	require.Equal(t, KnowsLoginStartWaitDefault, KnowsLoginStartWait())
	t.Setenv(KnowsLoginStartWaitEnv, "999999999")
	require.Equal(t, knowsLoginStartWaitMax, KnowsLoginStartWait())
}
