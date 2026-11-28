package provisionutil

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// knowsStartingClient is a login whose client connects after the login is complete.
type knowsStartingClient struct {
	bridgev2.NetworkAPI
	loggedIn atomic.Bool
}

func (c *knowsStartingClient) IsLoggedIn() bool { return c.loggedIn.Load() }
func (c *knowsStartingClient) GetContactList(context.Context) ([]*bridgev2.ResolveIdentifierResponse, error) {
	return []*bridgev2.ResolveIdentifierResponse{{UserID: networkid.UserID("contact")}}, nil
}
func (c *knowsStartingClient) ResolveIdentifier(context.Context, string, bool) (*bridgev2.ResolveIdentifierResponse, error) {
	return nil, nil
}
func (c *knowsStartingClient) SearchUsers(context.Context, string) ([]*bridgev2.ResolveIdentifierResponse, error) {
	return []*bridgev2.ResolveIdentifierResponse{{UserID: networkid.UserID("contact")}}, nil
}

func connectsAfter(client *knowsStartingClient, after time.Duration) {
	go func() {
		time.Sleep(after)
		client.loggedIn.Store(true)
	}()
}

func TestKnowsContactListWaitsForALoginThatIsStillConnecting(t *testing.T) {
	for _, wait := range []time.Duration{0, 5 * time.Second} {
		client := &knowsStartingClient{}
		connectsAfter(client, 150*time.Millisecond)
		start := time.Now()
		resp, err := GetContactListWait(context.Background(), knowsLogin(client), wait)
		require.NoError(t, err, "a login that connects within the wait gets its list, not a 403")
		require.GreaterOrEqual(t, time.Since(start), 140*time.Millisecond)
		require.Less(t, time.Since(start), 3*time.Second)
		require.Len(t, resp.Contacts, 1)
	}

	client := &knowsStartingClient{}
	connectsAfter(client, 150*time.Millisecond)
	resp, err := GetContactList(context.Background(), knowsLogin(client))
	require.NoError(t, err)
	require.Len(t, resp.Contacts, 1)
}

func TestKnowsUserSearchWaitsForALoginThatIsStillConnecting(t *testing.T) {
	client := &knowsStartingClient{}
	connectsAfter(client, 150*time.Millisecond)
	resp, err := SearchUsers(context.Background(), knowsLogin(client), "query")
	require.NoError(t, err)
	require.Len(t, resp.Results, 1)
}

func TestKnowsContactListRefusesALoginThatNeverConnects(t *testing.T) {
	t.Setenv(bridgev2.KnowsLoginStartWaitEnv, "250")
	start := time.Now()
	_, err := GetContactListWait(context.Background(), knowsLogin(&knowsStartingClient{}), 0)
	require.ErrorIs(t, err, bridgev2.ErrNotLoggedIn)
	require.GreaterOrEqual(t, time.Since(start), 240*time.Millisecond)
	require.Less(t, time.Since(start), 2*time.Second)
	_, err = SearchUsers(context.Background(), knowsLogin(&knowsStartingClient{}), "query")
	require.ErrorIs(t, err, bridgev2.ErrNotLoggedIn)

	ctx, cancel := context.WithCancel(context.Background())
	t.Setenv(bridgev2.KnowsLoginStartWaitEnv, "60000")
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start = time.Now()
	_, err = GetContactList(ctx, knowsLogin(&knowsStartingClient{}))
	require.ErrorIs(t, err, bridgev2.ErrNotLoggedIn)
	require.Less(t, time.Since(start), 2*time.Second, "a caller that goes away stops the wait")
}

func TestKnowsContactListLoginWaitOff(t *testing.T) {
	t.Setenv(bridgev2.KnowsLoginStartWaitEnv, "0")
	client := &knowsStartingClient{}
	connectsAfter(client, 150*time.Millisecond)
	start := time.Now()
	_, err := GetContactListWait(context.Background(), knowsLogin(client), 5*time.Second)
	require.ErrorIs(t, err, bridgev2.ErrNotLoggedIn, "off: refused at once, as upstream")
	require.Less(t, time.Since(start), 100*time.Millisecond)
}
