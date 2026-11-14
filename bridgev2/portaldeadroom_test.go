package bridgev2

import (
	"context"
	"database/sql"
	"errors"
	"testing"

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

// stateBot answers the room's create event the way the homeserver does.
type stateBot struct {
	MatrixAPI
	err error
}

func (b *stateBot) GetStateEvent(context.Context, id.RoomID, event.Type, string) (*event.Event, error) {
	return &event.Event{}, b.err
}

// storeBot is a Matrix API backed by the messages store, which can answer without a round trip.
type storeBot struct {
	MatrixAPI
	exists bool
	err    error
}

func (b *storeBot) RoomExists(context.Context, id.RoomID) (bool, error) { return b.exists, b.err }

func portalWithBot(bot MatrixAPI, mxid id.RoomID) *Portal {
	return &Portal{Portal: &database.Portal{MXID: mxid}, Bridge: &Bridge{Bot: bot}}
}

func TestRoomIsGoneOnlyOnADefiniteAnswer(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		bot  MatrixAPI
		mxid id.RoomID
		want bool
	}{
		{"no room yet", &stateBot{err: mautrix.MNotFound}, "", false},
		{"room deleted (M_NOT_FOUND)", &stateBot{err: mautrix.MNotFound}, "!dead:localhost", true},
		{"room deleted (M_FORBIDDEN)", &stateBot{err: mautrix.MForbidden}, "!dead:localhost", true},
		{"room alive", &stateBot{}, "!live:localhost", false},
		{"transient error keeps the room", &stateBot{err: errors.New("connection reset")}, "!live:localhost", false},
		{"store says gone", &storeBot{exists: false}, "!dead:localhost", true},
		{"store says alive", &storeBot{exists: true}, "!live:localhost", false},
		{"store error keeps the room", &storeBot{err: errors.New("db down")}, "!live:localhost", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, portalWithBot(tc.bot, tc.mxid).RoomIsGone(ctx))
		})
	}
}

func TestRecoverDeadRoomClearsTheRoomAndItsOldMessages(t *testing.T) {
	ctx := zerolog.Nop().WithContext(context.Background())
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = raw.Close() })
	db, err := dbutil.NewWithDB(raw, "sqlite3")
	require.NoError(t, err)
	bdb := database.New("test", database.MetaTypes{}, db)
	require.NoError(t, bdb.Upgrade(ctx))

	key := networkid.PortalKey{ID: "thread-1", Receiver: "login-1"}
	_, err = raw.Exec(`INSERT INTO portal (bridge_id, id, receiver, mxid, other_user_id, name, topic, avatar_id, avatar_hash, avatar_mxc,
		name_set, avatar_set, topic_set, name_is_custom, in_space, room_type, metadata)
		VALUES ('test', 'thread-1', 'login-1', '!dead:localhost', '', '', '', '', '', '', false, false, false, false, false, '', '{}')`)
	require.NoError(t, err)
	_, err = raw.Exec(`INSERT INTO message (bridge_id, id, part_id, mxid, room_id, room_receiver, sender_id, sender_mxid, timestamp, edit_count, metadata)
		VALUES ('test', 'm1', '', '$old:localhost', 'thread-1', 'login-1', 'u', '', 1, 0, '{}')`)
	require.NoError(t, err)

	br := &Bridge{ID: "test", DB: bdb, Bot: &stateBot{err: mautrix.MNotFound}, Config: &bridgeconfig.BridgeConfig{},
		portalsByKey: map[networkid.PortalKey]*Portal{}, portalsByMXID: map[id.RoomID]*Portal{}}
	dbPortal, err := bdb.Portal.GetByKey(ctx, key)
	require.NoError(t, err)
	portal := &Portal{Portal: dbPortal, Bridge: br, Log: zerolog.Nop(), RoomCreated: exsync.NewEvent()}
	br.portalsByKey[key] = portal
	br.portalsByMXID[portal.MXID] = portal

	require.True(t, portal.RecoverDeadRoom(ctx))
	require.Equal(t, id.RoomID(""), portal.MXID)
	var mxid sql.NullString
	require.NoError(t, raw.QueryRow(`SELECT mxid FROM portal WHERE id='thread-1'`).Scan(&mxid))
	require.Empty(t, mxid.String, "the cleared mxid is saved")
	var n int
	require.NoError(t, raw.QueryRow(`SELECT COUNT(*) FROM message WHERE room_id='thread-1'`).Scan(&n))
	require.Zero(t, n, "the old room's message rows are gone, so the new room's first fill is not anchored on them")

	br.Bot = &stateBot{}
	portal.MXID = "!live:localhost"
	require.False(t, portal.RecoverDeadRoom(ctx), "a live room is kept")
}
