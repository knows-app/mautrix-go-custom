package provisionutil

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

type knowsContactsClient struct {
	bridgev2.NetworkAPI
	contacts int
}

func (c *knowsContactsClient) IsLoggedIn() bool { return true }
func (c *knowsContactsClient) GetContactList(context.Context) ([]*bridgev2.ResolveIdentifierResponse, error) {
	out := make([]*bridgev2.ResolveIdentifierResponse, c.contacts)
	for i := range out {
		out[i] = &bridgev2.ResolveIdentifierResponse{UserID: networkid.UserID("contact")}
	}
	return out, nil
}

// knowsReadyClient can tell when its first list is ready.
type knowsReadyClient struct {
	knowsContactsClient
	bridgev2.KnowsContactListState
}

func knowsLogin(client bridgev2.NetworkAPI) *bridgev2.UserLogin {
	return &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login-1"}, Client: client}
}

func TestKnowsContactListWithoutWaitAnswersAtOnce(t *testing.T) {
	client := &knowsReadyClient{knowsContactsClient: knowsContactsClient{contacts: 2}}
	start := time.Now()
	resp, err := GetContactListWait(context.Background(), knowsLogin(client), 0)
	require.NoError(t, err)
	require.Less(t, time.Since(start), 50*time.Millisecond)
	require.Len(t, resp.Contacts, 2)
	require.Nil(t, resp.KnowsListReady, "no wait_ms: the answer is the same as before")
	data, err := json.Marshal(resp)
	require.NoError(t, err)
	require.NotContains(t, string(data), "knows_list_ready")
}

func TestKnowsContactListAnswersWhenTheFirstListIsReady(t *testing.T) {
	client := &knowsReadyClient{knowsContactsClient: knowsContactsClient{contacts: 0}}
	go func() {
		time.Sleep(80 * time.Millisecond)
		client.contacts = 3
		client.Staged(nil, 3, false)
	}()
	start := time.Now()
	resp, err := GetContactListWait(context.Background(), knowsLogin(client), 5*time.Second)
	require.NoError(t, err)
	elapsed := time.Since(start)
	require.GreaterOrEqual(t, elapsed, 70*time.Millisecond)
	require.Less(t, elapsed, 2*time.Second, "released by the event, not by the cap")
	require.Len(t, resp.Contacts, 3)
	require.True(t, *resp.KnowsListReady)

	// Ready already: no wait at all.
	start = time.Now()
	resp, err = GetContactListWait(context.Background(), knowsLogin(client), 5*time.Second)
	require.NoError(t, err)
	require.Less(t, time.Since(start), 50*time.Millisecond)
	require.True(t, *resp.KnowsListReady)
}

func TestKnowsContactListWaitIsCapped(t *testing.T) {
	old := KnowsContactsMaxWait
	KnowsContactsMaxWait = 60 * time.Millisecond
	t.Cleanup(func() { KnowsContactsMaxWait = old })
	client := &knowsReadyClient{}
	start := time.Now()
	resp, err := GetContactListWait(context.Background(), knowsLogin(client), time.Hour)
	require.NoError(t, err)
	require.Less(t, time.Since(start), time.Second)
	require.False(t, *resp.KnowsListReady, "the cap ran out before the list was ready")

	ctx, cancel := context.WithCancel(context.Background())
	KnowsContactsMaxWait = time.Hour
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	start = time.Now()
	_, _ = GetContactListWait(ctx, knowsLogin(client), time.Hour)
	require.Less(t, time.Since(start), time.Second, "a caller that goes away stops the wait")
}

func TestKnowsContactListConnectorWithoutTheEventAnswersAtOnce(t *testing.T) {
	start := time.Now()
	resp, err := GetContactListWait(context.Background(), knowsLogin(&knowsContactsClient{contacts: 1}), 5*time.Second)
	require.NoError(t, err)
	require.Less(t, time.Since(start), 50*time.Millisecond)
	require.Nil(t, resp.KnowsListReady, "nothing announced: the caller keeps its own retries")
}

func TestKnowsContactListAnswerIsMeasured(t *testing.T) {
	var buf bytes.Buffer
	ctx := zerolog.New(&buf).WithContext(context.Background())
	client := &knowsReadyClient{knowsContactsClient: knowsContactsClient{contacts: 4}}
	client.Staged(nil, 4, true)
	login := knowsLogin(client)
	_, err := GetContactListWait(ctx, login, time.Second)
	require.NoError(t, err)
	_, err = GetContactListWait(ctx, login, 0)
	require.NoError(t, err)
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	require.Len(t, lines, 2)
	var first, second map[string]any
	require.NoError(t, json.Unmarshal(lines[0], &first))
	require.NoError(t, json.Unmarshal(lines[1], &second))
	require.Equal(t, "Contact list answered", first["message"])
	require.Contains(t, first, "contacts_first_answer_ms")
	require.Equal(t, float64(4), first["contacts_count"])
	require.Equal(t, true, first["contacts_complete"])
	require.Equal(t, true, first["contacts_list_ready"])
	require.NotContains(t, second, "contacts_first_answer_ms", "only the login's first answer")
	require.NotContains(t, buf.String(), "login-1", "no login id in the measure")
}
