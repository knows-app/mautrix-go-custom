package bridgev2

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"go.mau.fi/util/exsync"

	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// knowsQueuePortal is a portal whose event loop the test drives: nothing reads
// its channel until the test does.
func knowsQueuePortal(t *testing.T, id string, buffer int, overflow bool) *Portal {
	t.Helper()
	br := &Bridge{Config: &bridgeconfig.BridgeConfig{PortalEventBuffer: buffer}}
	br.BackgroundCtx = context.Background()
	portal := &Portal{
		Portal:          &database.Portal{PortalKey: networkid.PortalKey{ID: networkid.PortalID(id)}},
		Bridge:          br,
		Log:             zerolog.Nop(),
		RoomCreated:     exsync.NewEvent(),
		events:          make(chan portalEvent, buffer),
		knowsOverflowOn: overflow,
	}
	portal.backgroundCtx, portal.cancelBackground = context.WithCancel(context.Background())
	portal.deleted = portal.backgroundCtx.Done()
	t.Cleanup(portal.cancelBackground)
	return portal
}

func knowsRemote(login *UserLogin, n int) *portalRemoteEvent {
	return &portalRemoteEvent{source: login, evtType: RemoteEventType(n)}
}

func knowsQueueAsync(portal *Portal, evt portalEvent) chan EventHandlingResult {
	out := make(chan EventHandlingResult, 1)
	go func() { out <- portal.queueEvent(zerolog.Nop().WithContext(context.Background()), evt) }()
	return out
}

func knowsRead(t *testing.T, portal *Portal) int {
	t.Helper()
	select {
	case evt := <-portal.events:
		return int(evt.(*portalRemoteEvent).evtType)
	case <-time.After(2 * time.Second):
		t.Fatal("no event in the channel")
		return -1
	}
}

func knowsSetCaps(t *testing.T, perPortal, perLogin int) {
	oldPortal, oldLogin := KnowsPortalRemoteBuffer, KnowsPortalOverflowPerLogin
	KnowsPortalRemoteBuffer, KnowsPortalOverflowPerLogin = perPortal, perLogin
	t.Cleanup(func() { KnowsPortalRemoteBuffer, KnowsPortalOverflowPerLogin = oldPortal, oldLogin })
}

func TestKnowsPortalOverflowOffBlocksAsUpstream(t *testing.T) {
	portal := knowsQueuePortal(t, "p", 2, false)
	login := &UserLogin{}
	ctx := context.Background()
	require.Equal(t, EventHandlingResultQueued, portal.queueEvent(ctx, knowsRemote(login, 1)))
	require.Equal(t, EventHandlingResultQueued, portal.queueEvent(ctx, knowsRemote(login, 2)))
	res := knowsQueueAsync(portal, knowsRemote(login, 3))
	select {
	case <-res:
		t.Fatal("a full channel did not block the caller without the flag")
	case <-time.After(100 * time.Millisecond):
	}
	require.Equal(t, 1, knowsRead(t, portal))
	require.Equal(t, EventHandlingResultQueued, <-res)
}

func TestKnowsPortalOverflowKeepsTheLoginLoopRunningAndTheOrder(t *testing.T) {
	portal := knowsQueuePortal(t, "busy", 4, true)
	login := &UserLogin{}
	before := KnowsPortalChannelFullCount()
	start := time.Now()
	for i := 1; i <= 300; i++ {
		require.Equal(t, EventHandlingResultQueued, portal.queueEvent(context.Background(), knowsRemote(login, i)))
	}
	require.Less(t, time.Since(start), time.Second, "300 events into a 4-slot channel did not block")
	require.Equal(t, int64(296), KnowsPortalChannelFullCount()-before, "portal_channel_full counts each event kept in memory")
	for i := 1; i <= 300; i++ {
		require.Equal(t, i, knowsRead(t, portal))
	}
	// Drained: the memory queue is released and the channel is used directly again.
	require.Eventually(t, func() bool {
		portal.knowsOverflowQueue.lock.Lock()
		defer portal.knowsOverflowQueue.lock.Unlock()
		return !portal.knowsOverflowQueue.pumping && portal.knowsOverflowQueue.items == nil
	}, time.Second, time.Millisecond)
	require.Equal(t, EventHandlingResultQueued, portal.queueEvent(context.Background(), knowsRemote(login, 301)))
	require.Equal(t, 301, knowsRead(t, portal))
}

func TestKnowsPortalOverflowBlocksAtTheLoginCap(t *testing.T) {
	knowsSetCaps(t, 2, 3)
	login := &UserLogin{}
	busy := knowsQueuePortal(t, "busy", 1, true)
	other := knowsQueuePortal(t, "other", 1, true)
	ctx := context.Background()
	// channel 1 + free 2 + login slots 3 = 6 events without blocking.
	for i := 1; i <= 6; i++ {
		require.Equal(t, EventHandlingResultQueued, busy.queueEvent(ctx, knowsRemote(login, i)))
	}
	blocked := knowsQueueAsync(busy, knowsRemote(login, 7))
	select {
	case <-blocked:
		t.Fatal("past the login cap, the caller must block as upstream")
	case <-time.After(100 * time.Millisecond):
	}
	// Another chat of the same login still has its own free buffer...
	for i := 1; i <= 3; i++ {
		require.Equal(t, EventHandlingResultQueued, other.queueEvent(ctx, knowsRemote(login, 100+i)))
	}
	// ...but shares the login's slots.
	otherBlocked := knowsQueueAsync(other, knowsRemote(login, 104))
	select {
	case <-otherBlocked:
		t.Fatal("the login's slots are shared by its portals")
	case <-time.After(50 * time.Millisecond):
	}
	// A portal of another login is not affected.
	stranger := knowsQueuePortal(t, "stranger", 1, true)
	for i := 1; i <= 6; i++ {
		require.Equal(t, EventHandlingResultQueued, stranger.queueEvent(ctx, knowsRemote(&UserLogin{}, i)))
	}
	// Consuming the busy portal frees a slot; order is kept.
	for i := 1; i <= 7; i++ {
		require.Equal(t, i, knowsRead(t, busy))
	}
	require.Equal(t, EventHandlingResultQueued, <-blocked)
	for i := 101; i <= 104; i++ {
		require.Equal(t, i, knowsRead(t, other))
	}
	require.Equal(t, EventHandlingResultQueued, <-otherBlocked)
	require.Eventually(t, func() bool { return len(login.knowsOverflowSlots()) == 0 }, time.Second, time.Millisecond,
		"every slot is given back")
}

func TestKnowsPortalOverflowDeletedPortalReleasesEverything(t *testing.T) {
	knowsSetCaps(t, 0, 2)
	login := &UserLogin{}
	portal := knowsQueuePortal(t, "p", 1, true)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		require.Equal(t, EventHandlingResultQueued, portal.queueEvent(ctx, knowsRemote(login, i)))
	}
	blocked := knowsQueueAsync(portal, knowsRemote(login, 4))
	time.Sleep(50 * time.Millisecond)
	portal.cancelBackground()
	require.Equal(t, EventHandlingResultIgnored, <-blocked)
	require.Eventually(t, func() bool { return len(login.knowsOverflowSlots()) == 0 }, time.Second, time.Millisecond)
	require.Equal(t, EventHandlingResultIgnored, portal.queueEvent(ctx, knowsRemote(login, 5)))
}

func TestKnowsPortalOverflowConcurrentChatsKeepTheirOrder(t *testing.T) {
	knowsSetCaps(t, 8, 50)
	login := &UserLogin{}
	const chats, perChat = 6, 200
	portals := make([]*Portal, chats)
	for c := range portals {
		portals[c] = knowsQueuePortal(t, fmt.Sprintf("chat-%d", c), 2, true)
	}
	var consumers sync.WaitGroup
	got := make([][]int, chats)
	for c := range portals {
		consumers.Add(1)
		go func(c int) {
			defer consumers.Done()
			for len(got[c]) < perChat {
				evt := <-portals[c].events
				got[c] = append(got[c], int(evt.(*portalRemoteEvent).evtType))
				if len(got[c])%50 == 0 {
					time.Sleep(time.Millisecond)
				}
			}
		}(c)
	}
	// One login loop, events of all chats interleaved, as a network connector sends them.
	for i := 1; i <= perChat; i++ {
		for c := range portals {
			require.Equal(t, EventHandlingResultQueued, portals[c].queueEvent(context.Background(), knowsRemote(login, i)))
		}
	}
	consumers.Wait()
	for c := range portals {
		require.Len(t, got[c], perChat)
		for i, v := range got[c] {
			require.Equal(t, i+1, v, "chat %d out of order", c)
		}
	}
}

func TestKnowsPortalOverflowLeavesMatrixEventsUpstream(t *testing.T) {
	portal := knowsQueuePortal(t, "p", 1, true)
	require.Equal(t, EventHandlingResultQueued, portal.queueEvent(context.Background(), &portalMatrixEvent{}))
	res := knowsQueueAsync(portal, &portalMatrixEvent{})
	select {
	case <-res:
		t.Fatal("Matrix events keep the upstream backpressure")
	case <-time.After(100 * time.Millisecond):
	}
	<-portal.events
	require.Equal(t, EventHandlingResultQueued, <-res)
}
