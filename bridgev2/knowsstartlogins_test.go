package bridgev2

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

type knowsConnectCounter struct {
	mu        sync.Mutex
	order     []string
	running   atomic.Int32
	maxSeen   atomic.Int32
	connected atomic.Int32
}

type knowsSlowClient struct {
	NetworkAPI
	id      string
	counter *knowsConnectCounter
	delay   time.Duration
	block   chan struct{}
}

func (c *knowsSlowClient) Connect(context.Context) {
	now := c.counter.running.Add(1)
	for {
		seen := c.counter.maxSeen.Load()
		if now <= seen || c.counter.maxSeen.CompareAndSwap(seen, now) {
			break
		}
	}
	c.counter.mu.Lock()
	c.counter.order = append(c.counter.order, c.id)
	c.counter.mu.Unlock()
	if c.block != nil {
		<-c.block
	}
	time.Sleep(c.delay)
	c.counter.running.Add(-1)
	c.counter.connected.Add(1)
}

type knowsReadyMatrix struct {
	MatrixConnector
	mu      sync.Mutex
	history [][3]int
	ready   bool
}

func (m *knowsReadyMatrix) KnowsSetLoginsReady(started, total int, ready bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := 0
	if ready {
		r = 1
	}
	m.history = append(m.history, [3]int{started, total, r})
	m.ready = ready
}

func knowsStartBridge(t *testing.T, logins int, delay time.Duration) (*Bridge, []*User, *knowsConnectCounter, *knowsReadyMatrix) {
	t.Helper()
	matrix := &knowsReadyMatrix{}
	br := &Bridge{Log: zerolog.Nop(), Config: &bridgeconfig.BridgeConfig{}, Matrix: matrix, Background: true}
	counter := &knowsConnectCounter{}
	user := &User{Bridge: br, User: &database.User{MXID: id.UserID("@u:test")}, logins: map[networkid.UserLoginID]*UserLogin{}}
	for i := 0; i < logins; i++ {
		loginID := networkid.UserLoginID(fmt.Sprintf("login-%02d", i))
		user.logins[loginID] = &UserLogin{
			UserLogin: &database.UserLogin{ID: loginID},
			Bridge:    br,
			User:      user,
			Log:       zerolog.Nop(),
			Client:    &knowsSlowClient{id: string(loginID), counter: counter, delay: delay},
		}
	}
	return br, []*User{user}, counter, matrix
}

func TestKnowsStartLoginsIsSerialByDefault(t *testing.T) {
	t.Setenv(KnowsStartLoginsConcurrencyEnv, "")
	br, users, counter, matrix := knowsStartBridge(t, 5, 5*time.Millisecond)
	br.StartLoginsForUsers(context.Background(), users)
	require.Equal(t, int32(5), counter.connected.Load())
	require.Equal(t, int32(1), counter.maxSeen.Load(), "one Connect at a time, as upstream")
	require.True(t, matrix.ready)
	require.Equal(t, [3]int{5, 5, 1}, matrix.history[len(matrix.history)-1])
	for _, bad := range []string{"0", "-3", "x", "1"} {
		t.Setenv(KnowsStartLoginsConcurrencyEnv, bad)
		require.Equal(t, 1, KnowsStartLoginsConcurrency(), bad)
	}
	t.Setenv(KnowsStartLoginsConcurrencyEnv, "1000")
	require.Equal(t, 64, KnowsStartLoginsConcurrency())
}

func TestKnowsStartLoginsPoolIsBounded(t *testing.T) {
	t.Setenv(KnowsStartLoginsConcurrencyEnv, "8")
	br, users, counter, matrix := knowsStartBridge(t, 40, 30*time.Millisecond)
	start := time.Now()
	br.StartLoginsForUsers(context.Background(), users)
	elapsed := time.Since(start)
	require.Equal(t, int32(40), counter.connected.Load(), "every login is connected before the bridge says it started")
	require.Equal(t, int32(8), counter.maxSeen.Load(), "never more than 8 Connect at once")
	require.Less(t, elapsed, 40*30*time.Millisecond/2, "faster than one by one")
	require.True(t, matrix.ready)
	last := matrix.history[len(matrix.history)-1]
	require.Equal(t, [3]int{40, 40, 1}, last)
	for _, h := range matrix.history[:len(matrix.history)-1] {
		require.Zero(t, h[2], "not ready before the last login")
	}
}

func TestKnowsStartLoginsSlowLoginDoesNotHoldThePool(t *testing.T) {
	t.Setenv(KnowsStartLoginsConcurrencyEnv, "2")
	old := KnowsStartLoginMaxWait
	KnowsStartLoginMaxWait = 50 * time.Millisecond
	t.Cleanup(func() { KnowsStartLoginMaxWait = old })
	br, users, counter, matrix := knowsStartBridge(t, 6, time.Millisecond)
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	for _, login := range users[0].logins {
		login.Client.(*knowsSlowClient).block = stuck
		break
	}
	done := make(chan struct{})
	go func() { br.StartLoginsForUsers(context.Background(), users); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a login that never finishes connecting held the startup")
	}
	require.Equal(t, int32(5), counter.connected.Load(), "the others connected")
	require.True(t, matrix.ready)
}
