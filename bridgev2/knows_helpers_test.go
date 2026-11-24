package bridgev2

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"go.mau.fi/util/dbutil"
	"go.mau.fi/util/exsync"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// knowsFakeMatrix records batch sends; everything else it is not asked for panics.
type knowsFakeMatrix struct {
	MatrixConnector
	mu      sync.Mutex
	batches []knowsBatch
}

type knowsBatch struct {
	forward bool
	ids     []string
}

func (m *knowsFakeMatrix) GetCapabilities() *MatrixCapabilities {
	return &MatrixCapabilities{BatchSending: true}
}
func (m *knowsFakeMatrix) GhostIntent(networkid.UserID) MatrixAPI { return nil }
func (m *knowsFakeMatrix) GenerateDeterministicEventID(_ id.RoomID, _ networkid.PortalKey, msgID networkid.MessageID, _ networkid.PartID) id.EventID {
	return id.EventID("$" + string(msgID) + ":test")
}
func (m *knowsFakeMatrix) BatchSend(_ context.Context, _ id.RoomID, req *mautrix.ReqBeeperBatchSend, _ []*MatrixSendExtra) (*mautrix.RespBeeperBatchSend, error) {
	b := knowsBatch{forward: req.Forward}
	for _, evt := range req.Events {
		b.ids = append(b.ids, string(evt.ID))
	}
	m.mu.Lock()
	m.batches = append(m.batches, b)
	m.mu.Unlock()
	return &mautrix.RespBeeperBatchSend{}, nil
}

func (m *knowsFakeMatrix) sent() []knowsBatch {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]knowsBatch(nil), m.batches...)
}

type knowsFakeBot struct{ MatrixAPI }

func (knowsFakeBot) GetMXID() id.UserID   { return "@bot:test" }
func (knowsFakeBot) IsDoublePuppet() bool { return false }

// knowsFetcher is a backfilling network API whose FetchMessages the test drives.
type knowsFetcher struct {
	BackfillingNetworkAPI
	fetch func(ctx context.Context, params FetchMessagesParams) (*FetchMessagesResponse, error)
}

func (f *knowsFetcher) FetchMessages(ctx context.Context, params FetchMessagesParams) (*FetchMessagesResponse, error) {
	return f.fetch(ctx, params)
}

type knowsTestEnv struct {
	ctx    context.Context
	raw    *sql.DB
	br     *Bridge
	portal *Portal
	matrix *knowsFakeMatrix
	source *UserLogin
}

func newKnowsTestEnv(t *testing.T) *knowsTestEnv {
	t.Helper()
	ctx := zerolog.Nop().WithContext(context.Background())
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = raw.Close() })
	db, err := dbutil.NewWithDB(raw, "sqlite3")
	require.NoError(t, err)
	bdb := database.New("test", database.MetaTypes{}, db)
	require.NoError(t, bdb.Upgrade(ctx))
	_, err = raw.Exec(`INSERT INTO portal (bridge_id, id, receiver, mxid, other_user_id, name, topic, avatar_id, avatar_hash, avatar_mxc,
		name_set, avatar_set, topic_set, name_is_custom, in_space, room_type, metadata)
		VALUES ('test', 'thread-1', 'login-1', '!room:test', '', '', '', '', '', '', false, false, false, false, false, 'dm', '{}')`)
	require.NoError(t, err)
	matrix := &knowsFakeMatrix{}
	cfg := &bridgeconfig.BridgeConfig{}
	cfg.Backfill.Enabled = true
	cfg.Backfill.MaxInitialMessages = 500
	br := &Bridge{
		ID: "test", DB: bdb, Config: cfg, Matrix: matrix, Bot: knowsFakeBot{},
		portalsByKey: map[networkid.PortalKey]*Portal{}, portalsByMXID: map[id.RoomID]*Portal{},
		ghostsByID: map[networkid.UserID]*Ghost{}, usersByMXID: map[id.UserID]*User{},
		userLoginsByID:      map[networkid.UserLoginID]*UserLogin{},
		wakeupBackfillQueue: make(chan struct{}, 1),
		manualBackfills:     make(chan *ManualBackfill, 1),
		stopBackfillQueue:   exsync.NewEvent(),
	}
	br.BackgroundCtx = ctx
	dbPortal, err := bdb.Portal.GetByKey(ctx, networkid.PortalKey{ID: "thread-1", Receiver: "login-1"})
	require.NoError(t, err)
	portal := &Portal{Portal: dbPortal, Bridge: br, Log: zerolog.Nop(), RoomCreated: exsync.NewEvent()}
	portal.backgroundCtx, portal.cancelBackground = context.WithCancel(ctx)
	portal.deleted = portal.backgroundCtx.Done()
	t.Cleanup(portal.cancelBackground)
	br.portalsByKey[portal.PortalKey] = portal
	source := &UserLogin{UserLogin: &database.UserLogin{ID: "login-1", UserMXID: "@user:test"}, Bridge: br}
	return &knowsTestEnv{ctx: ctx, raw: raw, br: br, portal: portal, matrix: matrix, source: source}
}

// knowsHistory returns n messages, one second apart, oldest first: m001 .. m<n>.
func knowsHistory(n int) []*BackfillMessage {
	base := time.Unix(1_700_000_000, 0)
	msgs := make([]*BackfillMessage, 0, n)
	for i := 1; i <= n; i++ {
		msgs = append(msgs, &BackfillMessage{
			ID:        networkid.MessageID(knowsMsgID(i)),
			Timestamp: base.Add(time.Duration(i) * time.Second),
			ConvertedMessage: &ConvertedMessage{Parts: []*ConvertedMessagePart{{
				Type:    event.EventMessage,
				Content: &event.MessageEventContent{MsgType: event.MsgText, Body: "x"},
			}}},
		})
	}
	return msgs
}

func knowsMsgID(i int) string {
	return "m" + string(rune('0'+i/100%10)) + string(rune('0'+i/10%10)) + string(rune('0'+i%10))
}

func knowsEventIDs(from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, "$"+knowsMsgID(i)+":test")
	}
	return out
}

// latest returns the newest count messages of history, oldest first, like a forward fetch.
func latest(history []*BackfillMessage, count int) []*BackfillMessage {
	if count > len(history) {
		count = len(history)
	}
	out := make([]*BackfillMessage, 0, count)
	for _, msg := range history[len(history)-count:] {
		cp := *msg
		out = append(out, &cp)
	}
	return out
}
