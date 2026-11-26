package bridgev2

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// goneRoomBot answers the room's create event like a homeserver whose room was deleted (err), and counts the
// checks: each one is a RoomIsGone, so a RecoverDeadRoom.
type goneRoomBot struct {
	knowsFakeBot
	err    error
	checks atomic.Int32
}

func (b *goneRoomBot) GetStateEvent(context.Context, id.RoomID, event.Type, string) (*event.Event, error) {
	b.checks.Add(1)
	if b.err != nil {
		return nil, b.err
	}
	return &event.Event{}, nil
}

func knowsMatrixError(status int, errcode string) error {
	return mautrix.HTTPError{
		Request:   &http.Request{},
		Response:  &http.Response{StatusCode: status},
		RespError: &mautrix.RespError{ErrCode: errcode, Err: "x", StatusCode: status},
	}
}

func (env *knowsTestEnv) savedMXID(t *testing.T) string {
	t.Helper()
	var mxid sql.NullString
	require.NoError(t, env.raw.QueryRow(`SELECT mxid FROM portal WHERE id='thread-1'`).Scan(&mxid))
	return mxid.String
}

func (env *knowsTestEnv) messageRows(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, env.raw.QueryRow(`SELECT COUNT(*) FROM message WHERE room_id='thread-1'`).Scan(&n))
	return n
}

// Dev 26/11 (fb01): 8 Messenger portals whose room was deleted failed their forward backfill with M_NOT_FOUND
// at every start, and their receipts and reactions too: nothing but create_dm ever forgot the room.
func TestKnowsForwardBackfillIntoAGoneRoomForgetsIt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		flag       string
		writeErr   error
		roomErr    error
		wantChecks int32
		wantGone   bool
	}{
		{"room deleted (M_NOT_FOUND)", "1", knowsMatrixError(404, "M_NOT_FOUND"), mautrix.MNotFound, 1, true},
		{"bot kicked out (M_FORBIDDEN)", "1", knowsMatrixError(403, "M_FORBIDDEN"), mautrix.MForbidden, 1, true},
		{"room alive after all", "1", knowsMatrixError(404, "M_NOT_FOUND"), nil, 1, false},
		{"transient error: no check", "1", knowsMatrixError(502, "M_UNKNOWN"), mautrix.MNotFound, 0, false},
		{"flag off: as before", "", knowsMatrixError(404, "M_NOT_FOUND"), mautrix.MNotFound, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(KnowsRecoverDeadRoomOnWriteEnv, tc.flag)
			env := newKnowsTestEnv(t)
			bot := &goneRoomBot{err: tc.roomErr}
			env.br.Bot = bot
			env.br.portalsByMXID[env.portal.MXID] = env.portal
			env.matrix.batchErr = tc.writeErr
			_, err := env.raw.Exec(`INSERT INTO message (bridge_id, id, part_id, mxid, room_id, room_receiver, sender_id, sender_mxid, timestamp, edit_count, metadata)
				VALUES ('test', 'old', '', '$old:test', 'thread-1', 'login-1', 'u', '', 1, 0, '{}')`)
			require.NoError(t, err)
			var fetches int
			env.source.Client = &knowsFetcher{fetch: func(context.Context, FetchMessagesParams) (*FetchMessagesResponse, error) {
				fetches++
				return &FetchMessagesResponse{Messages: knowsHistory(3), MarkRead: true}, nil
			}}

			env.portal.doForwardBackfill(env.ctx, env.source, nil, nil)
			require.Len(t, env.matrix.sent(), 1, "one write")
			require.Equal(t, tc.wantChecks, bot.checks.Load(), "RecoverDeadRoom runs once, on a definite answer only")
			if !tc.wantGone {
				require.Equal(t, id.RoomID("!room:test"), env.portal.MXID)
				require.Equal(t, "!room:test", env.savedMXID(t))
				require.Equal(t, 1, env.messageRows(t))
				return
			}
			require.Equal(t, id.RoomID(""), env.portal.MXID)
			require.Empty(t, env.savedMXID(t), "the cleared mxid is saved")
			require.Zero(t, env.messageRows(t), "the old room's rows go with it")
			require.NotContains(t, env.br.portalsByMXID, id.RoomID("!room:test"))

			// The next fill of this portal writes nothing until a new room exists.
			env.portal.doForwardBackfill(env.ctx, env.source, nil, nil)
			require.Len(t, env.matrix.sent(), 1, "no second write into the dead room")
			require.Equal(t, tc.wantChecks, bot.checks.Load(), "no second check")
		})
	}
}

// knowsFailingRemoteEvent is a remote event whose handling fails with err.
type knowsFailingRemoteEvent struct {
	err error
}

func (e *knowsFailingRemoteEvent) GetType() RemoteEventType                        { return RemoteEventReadReceipt }
func (e *knowsFailingRemoteEvent) GetPortalKey() networkid.PortalKey               { return networkid.PortalKey{} }
func (e *knowsFailingRemoteEvent) AddLogContext(c zerolog.Context) zerolog.Context { return c }
func (e *knowsFailingRemoteEvent) GetSender() EventSender                          { return EventSender{} }
func (e *knowsFailingRemoteEvent) PreHandleWithResult(context.Context, *Portal) *EventHandlingResult {
	res := EventHandlingResultFailed.WithError(fmt.Errorf("failed to bridge read receipt: %w", e.err))
	return &res
}

func TestKnowsRemoteEventIntoAGoneRoomForgetsIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		flag     string
		err      error
		wantGone bool
	}{
		{"M_NOT_FOUND", "1", knowsMatrixError(404, "M_NOT_FOUND"), true},
		{"other error", "1", errors.New("network down"), false},
		{"flag off", "", knowsMatrixError(404, "M_NOT_FOUND"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(KnowsRecoverDeadRoomOnWriteEnv, tc.flag)
			env := newKnowsTestEnv(t)
			bot := &goneRoomBot{err: mautrix.MNotFound}
			env.br.Bot = bot
			evt := &portalRemoteEvent{evt: &knowsFailingRemoteEvent{err: tc.err}, source: env.source, evtType: RemoteEventReadReceipt}
			var res EventHandlingResult
			env.portal.handleSingleEvent(env.ctx, evt, func(r EventHandlingResult) { res = r })
			require.False(t, res.Success)
			if tc.wantGone {
				require.Equal(t, int32(1), bot.checks.Load())
				require.Equal(t, id.RoomID(""), env.portal.MXID)
				require.Empty(t, env.savedMXID(t))
			} else {
				require.Zero(t, bot.checks.Load())
				require.Equal(t, id.RoomID("!room:test"), env.portal.MXID)
			}
		})
	}
}

// A manual backfill (KnowsFillGap, a connector's own page fill) of a portal whose room was forgotten used to
// write to /rooms//… and fail with a bare 404, once per queued page.
func TestKnowsBackfillTaskOfAPortalWithoutRoomWritesNothing(t *testing.T) {
	env := newKnowsTestEnv(t)
	env.portal.MXID = ""
	env.source.Client = &knowsFetcher{fetch: func(context.Context, FetchMessagesParams) (*FetchMessagesResponse, error) {
		t.Fatal("nothing is fetched for a portal without a room")
		return nil, nil
	}}
	env.br.Config.Backfill.Queue.MaxBatches = -1
	task := &database.BackfillTask{BatchCount: 2}
	completed, err := env.portal.doBackfillTask(env.ctx, env.source, task, &FetchMessagesResponse{Messages: knowsHistory(3)})
	require.ErrorIs(t, err, ErrPortalHasNoRoom)
	require.False(t, completed)
	require.Empty(t, env.matrix.sent())
	require.Equal(t, 2, task.BatchCount, "the task does not move")
}

// ForgetDeadRoom is one transaction: when clearing the mxid fails, the old room's rows stay with it.
func TestKnowsForgetDeadRoomIsOneTransaction(t *testing.T) {
	env := newKnowsTestEnv(t)
	env.br.portalsByMXID[env.portal.MXID] = env.portal
	env.portal.RoomCreated.Set()
	_, err := env.raw.Exec(`INSERT INTO message (bridge_id, id, part_id, mxid, room_id, room_receiver, sender_id, sender_mxid, timestamp, edit_count, metadata)
		VALUES ('test', 'old', '', '$old:test', 'thread-1', 'login-1', 'u', '', 1, 0, '{}')`)
	require.NoError(t, err)
	_, err = env.raw.Exec(`CREATE TRIGGER fail_portal_update BEFORE UPDATE ON portal BEGIN SELECT RAISE(ABORT, 'portal update refused'); END`)
	require.NoError(t, err)

	require.Error(t, env.portal.ForgetDeadRoom(env.ctx))
	require.Equal(t, 1, env.messageRows(t), "the deletes are rolled back")
	require.Equal(t, "!room:test", env.savedMXID(t))
	require.Equal(t, id.RoomID("!room:test"), env.portal.MXID, "memory matches the database")
	require.True(t, env.portal.RoomCreated.IsSet())
	require.Same(t, env.portal, env.br.portalsByMXID["!room:test"])

	_, err = env.raw.Exec(`DROP TRIGGER fail_portal_update`)
	require.NoError(t, err)
	require.NoError(t, env.portal.ForgetDeadRoom(env.ctx))
	require.Zero(t, env.messageRows(t))
	require.Empty(t, env.savedMXID(t))
	require.Equal(t, id.RoomID(""), env.portal.MXID)
}
