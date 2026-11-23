package bridgev2

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"go.mau.fi/util/dbutil"
	"go.mau.fi/util/exsync"

	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

func TestKnowsFillGapKeepsNewerMessagesAndDropsKnownIDs(t *testing.T) {
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
		VALUES ('test', 'thread-1', 'login-1', '!room:localhost', '', '', '', '', '', '', false, false, false, false, false, '', '{}')`)
	require.NoError(t, err)
	// The room's first message is at t=1000 (unix ms in the db); m3 is already known.
	for _, m := range []struct {
		id string
		ts int64
	}{{"first", 1000}, {"m3", 1200}} {
		_, err = raw.Exec(`INSERT INTO message (bridge_id, id, part_id, mxid, room_id, room_receiver, sender_id, sender_mxid, timestamp, edit_count, metadata)
			VALUES ('test', ?, '', ?, 'thread-1', 'login-1', 'u', '', ?, 0, '{}')`, m.id, "$"+m.id+":localhost", m.ts*int64(time.Millisecond))
		require.NoError(t, err)
	}

	br := &Bridge{ID: "test", DB: bdb, Config: &bridgeconfig.BridgeConfig{},
		portalsByKey: map[networkid.PortalKey]*Portal{}, portalsByMXID: map[id.RoomID]*Portal{}}
	dbPortal, err := bdb.Portal.GetByKey(ctx, key)
	require.NoError(t, err)
	portal := &Portal{Portal: dbPortal, Bridge: br, Log: zerolog.Nop(), RoomCreated: exsync.NewEvent()}
	first, err := bdb.Message.GetFirstPortalMessage(ctx, key)
	require.NoError(t, err)
	require.NotNil(t, first)

	batch := func() []*BackfillMessage {
		var msgs []*BackfillMessage
		for _, m := range []struct {
			id string
			ts int64
		}{{"m1", 900}, {"m2", 1100}, {"m3", 1200}, {"m4", 1300}} {
			msgs = append(msgs, &BackfillMessage{ID: networkid.MessageID(m.id), Timestamp: time.Unix(0, m.ts*int64(time.Millisecond))})
		}
		return msgs
	}
	ids := func(msgs []*BackfillMessage) (out []string) {
		for _, m := range msgs {
			out = append(out, string(m.ID))
		}
		return
	}
	run := func(resp *FetchMessagesResponse) []string {
		anchor, aggressive := knowsCutoff(resp, first)
		return ids(portal.cutoffMessages(ctx, batch(), aggressive, false, anchor))
	}

	require.Equal(t, []string{"m1", "m2", "m4"}, run(&FetchMessagesResponse{KnowsFillGap: true}))
	require.Equal(t, []string{"m1"}, run(&FetchMessagesResponse{}))
}
