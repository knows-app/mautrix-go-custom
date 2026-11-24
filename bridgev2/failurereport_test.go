package bridgev2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
)

// Planted secrets: none of them may reach a reporter or the failure log line.
const (
	secretText  = "Secret message text, hello Alice"
	secretName  = "Secret Alice Name"
	secretPhone = "+33612345678"
	secretUser  = "secret_username_alice"
	secretToken = "secret-token-EAAB1234"
)

var secrets = []string{secretText, secretName, secretPhone, secretUser, secretToken}

type reportedFailure struct {
	event   string
	warning bool
	err     error
	attrs   map[string]string
}

type failureRecorder struct {
	mu   sync.Mutex
	got  []reportedFailure
	logs bytes.Buffer
}

func (r *failureRecorder) report(event string, warning bool, err error, attrs map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, reportedFailure{event, warning, err, attrs})
}

func (r *failureRecorder) ctx() context.Context {
	return zerolog.New(&r.logs).WithContext(context.Background())
}

// assertClean fails if a planted secret appears in what was reported (except
// the error value itself, which only the reporter sees and must never print)
// or in the failure log lines.
func (r *failureRecorder) assertClean(t *testing.T) {
	t.Helper()
	var reported strings.Builder
	for _, f := range r.got {
		fmt.Fprint(&reported, f.event, f.attrs)
	}
	for _, secret := range secrets {
		require.NotContains(t, reported.String(), secret)
		require.NotContains(t, r.logs.String(), secret)
	}
}

func recordFailures(t *testing.T) *failureRecorder {
	rec := &failureRecorder{}
	SetFailureReporter(rec.report)
	t.Cleanup(func() { SetFailureReporter(nil) })
	return rec
}

func TestFailureCodeWord(t *testing.T) {
	for in, want := range map[string]string{
		"ig-dgw-connection-error":                   "IG_DGW_CONNECTION_ERROR",
		"wa-transient-disconnect":                   "WA_TRANSIENT_DISCONNECT",
		"meta-lserror-1357004":                      "META_LSERROR_1357004",
		"m.foreign_network_error":                   "M.FOREIGN_NETWORK_ERROR",
		"FI.MAU.BRIDGE.LOGIN_ADMISSION_UNAVAILABLE": "FI.MAU.BRIDGE.LOGIN_ADMISSION_UNAVAILABLE",
		"M_FORBIDDEN":                               "M_FORBIDDEN",
		"":                                          "",
		secretName:                                  "",
		secretPhone:                                 "",
		"user@example.com":                          "",
		strings.Repeat("A", 65):                     "",
	} {
		require.Equal(t, want, failureCodeWord(in), in)
	}
}

func TestReportFailureKeepsOnlyCodeWords(t *testing.T) {
	rec := recordFailures(t)
	ReportFailure(rec.ctx(), FailureReceive, false, errors.New(secretText+" "+secretToken), map[string]string{
		"step":    secretText,
		"errcode": secretName,
		"outcome": secretPhone,
		"status":  secretUser,
		"name":    secretName,
	})
	require.Len(t, rec.got, 1)
	require.Equal(t, map[string]string{"step": "other"}, rec.got[0].attrs)
	require.Contains(t, rec.logs.String(), `"failure_event":"receive_failed"`)
	require.Contains(t, rec.logs.String(), `"level":"warn"`)
	rec.assertClean(t)
}

func TestReportFailureSkipsCancellation(t *testing.T) {
	rec := recordFailures(t)
	ReportFailure(rec.ctx(), FailureSend, false, fmt.Errorf("send: %w", context.Canceled), nil)
	require.Empty(t, rec.got)
	require.Empty(t, rec.logs.String())
}

type statusRecordingMatrix struct {
	MatrixConnector
	statuses []*MessageStatus
}

func (m *statusRecordingMatrix) SendMessageStatus(_ context.Context, ms *MessageStatus, _ *MessageStatusEventInfo) {
	m.statuses = append(m.statuses, ms)
}

func messageEvent(msgType event.MessageType) *event.Event {
	return &event.Event{
		Type: event.EventMessage,
		ID:   "$event",
		Content: event.Content{Parsed: &event.MessageEventContent{
			MsgType: msgType,
			Body:    secretText + " " + secretPhone,
		}},
	}
}

func TestSendErrorStatusReportsSendFailed(t *testing.T) {
	rec := recordFailures(t)
	matrix := &statusRecordingMatrix{}
	portal := &Portal{Portal: &database.Portal{}, Bridge: &Bridge{Matrix: matrix}}

	// A plain network error: retriable, generic reason.
	portal.sendErrorStatus(rec.ctx(), messageEvent(event.MsgText), errors.New(secretToken))
	// A typed failure with its own reason and status.
	portal.sendErrorStatus(rec.ctx(), messageEvent(event.MsgImage), WrapErrorInStatus(errors.New(secretName)).
		WithStatus(event.MessageStatusFail).WithErrorReason(event.MessageStatusNetworkError))
	// The sender wrote the message type: it never becomes the step.
	portal.sendErrorStatus(rec.ctx(), messageEvent(event.MessageType("com.example."+secretUser)), errors.New("x"))
	// An unsupported event is the expected answer: a warning.
	portal.sendErrorStatus(rec.ctx(), &event.Event{Type: event.EventReaction, ID: "$r"}, ErrReactionsNotSupported)
	// A timeout keeps its outcome.
	portal.sendErrorStatus(rec.ctx(), messageEvent(event.MsgFile), fmt.Errorf("upload: %w", context.DeadlineExceeded))
	// Cancelled by shutdown: the status is still sent, nothing is reported.
	portal.sendErrorStatus(rec.ctx(), messageEvent(event.MsgText), context.Canceled)

	require.Len(t, matrix.statuses, 6, "every status is still sent")
	require.Len(t, rec.got, 5)
	for _, f := range rec.got {
		require.Equal(t, FailureSend, f.event)
	}
	require.Equal(t, map[string]string{"step": "m.text", "errcode": "M.EVENT_NOT_HANDLED", "outcome": "fail_retriable"}, rec.got[0].attrs)
	require.False(t, rec.got[0].warning)
	require.Equal(t, map[string]string{"step": "m.image", "errcode": "M.FOREIGN_NETWORK_ERROR", "outcome": "fail_permanent"}, rec.got[1].attrs)
	require.Equal(t, "other", rec.got[2].attrs["step"])
	require.Equal(t, "m.reaction", rec.got[3].attrs["step"])
	require.True(t, rec.got[3].warning)
	require.Equal(t, "timeout", rec.got[4].attrs["outcome"])
	require.Equal(t, "m.file", rec.got[4].attrs["step"])
	rec.assertClean(t)
}

func TestReceiveFailureReportsFailedRemoteEvents(t *testing.T) {
	rec := recordFailures(t)
	ctx := rec.ctx()
	reportReceiveFailure(ctx, RemoteEventMessage, EventHandlingResultSuccess)
	reportReceiveFailure(ctx, RemoteEventMessage, EventHandlingResultIgnored)
	reportReceiveFailure(ctx, RemoteEventMessage, EventHandlingResultFailed)
	reportReceiveFailure(ctx, RemoteEventMessage, EventHandlingResultFailed.WithError(errors.New(secretText)))
	reportReceiveFailure(ctx, RemoteEventReactionRemove, EventHandlingResultFailed.WithError(ErrFailedToGetIntent))
	reportReceiveFailure(ctx, RemoteEventReadReceipt, EventHandlingResultFailed.WithError(errors.New(secretName)))
	require.Len(t, rec.got, 3)
	require.Equal(t, FailureReceive, rec.got[0].event)
	require.Equal(t, map[string]string{"step": "message"}, rec.got[0].attrs)
	require.False(t, rec.got[0].warning)
	require.Equal(t, "reaction_remove", rec.got[1].attrs["step"])
	require.Equal(t, "read_receipt", rec.got[2].attrs["step"])
	require.True(t, rec.got[2].warning, "a lost read receipt loses no content")
	rec.assertClean(t)
}

func TestRemoteEventStepNamesEveryType(t *testing.T) {
	for evtType := RemoteEventUnknown; evtType <= RemoteEventBackfill; evtType++ {
		step := remoteEventStep(evtType)
		require.NotEqual(t, "other", step, evtType.String())
		require.Regexp(t, reFailureStep, step)
	}
	require.Equal(t, "message_upsert", remoteEventStep(RemoteEventMessageUpsert))
	require.Equal(t, "chat_info_change", remoteEventStep(RemoteEventChatInfoChange))
}

func TestBackfillFailureReports(t *testing.T) {
	rec := recordFailures(t)
	reportBackfillFailure(rec.ctx(), "forward_fetch", errors.New(secretText))
	reportBackfillFailure(rec.ctx(), "queue_task", errNoMessagesLeftAfterCutoff)
	reportBackfillFailure(rec.ctx(), "queue_task", nil)
	require.Len(t, rec.got, 1)
	require.Equal(t, FailureBackfill, rec.got[0].event)
	require.Equal(t, "forward_fetch", rec.got[0].attrs["step"])
	rec.assertClean(t)
}

func TestLoginStepFailureReports(t *testing.T) {
	rec := recordFailures(t)
	ReportLoginStepFailure(rec.ctx(), "user_input", RespError{ErrCode: "FI.MAU.META.WRONG_PASSWORD", Err: secretUser, StatusCode: http.StatusForbidden})
	ReportLoginStepFailure(rec.ctx(), "start", errors.New(secretToken))
	ReportLoginStepFailure(rec.ctx(), "create", RespError{ErrCode: "FI.MAU.BRIDGE.LOGIN_ADMISSION_UNAVAILABLE", Err: secretToken, StatusCode: http.StatusServiceUnavailable})
	require.Len(t, rec.got, 3)
	require.Equal(t, map[string]string{"step": "user_input", "errcode": "FI.MAU.META.WRONG_PASSWORD", "status": "403"}, rec.got[0].attrs)
	require.True(t, rec.got[0].warning, "a 4xx is the expected answer to the user's input")
	require.Equal(t, map[string]string{"step": "start"}, rec.got[1].attrs)
	require.False(t, rec.got[1].warning)
	require.Equal(t, "503", rec.got[2].attrs["status"])
	require.False(t, rec.got[2].warning)
	rec.assertClean(t)
}

func TestBridgeStateReportsLostLoginsOncePerCause(t *testing.T) {
	rec := recordFailures(t)
	login := &UserLogin{
		UserLogin: &database.UserLogin{
			ID:            secretPhone,
			RemoteName:    secretName,
			RemoteProfile: status.RemoteProfile{Phone: secretPhone, Username: secretUser, Name: secretName},
		},
		Log: zerolog.New(&rec.logs),
	}
	bsq := &BridgeStateQueue{login: login, ch: make(chan status.BridgeState, 100)}
	for _, state := range []status.BridgeState{
		{StateEvent: status.StateConnected},
		{StateEvent: status.StateTransientDisconnect, Error: "ig-dgw-connection-error"},
		{StateEvent: status.StateBadCredentials, Error: "ig-challenge-required", Message: secretText},
		{StateEvent: status.StateBadCredentials, Error: "ig-challenge-required", Message: secretText},
		{StateEvent: status.StateBadCredentials, Error: "meta-cookie-removed"},
		{StateEvent: status.StateUnknownError, Error: "wa-connect-failure-405"},
		{StateEvent: status.StateConnected},
		{StateEvent: status.StateLoggedOut},
		{StateEvent: status.StateLoggedOut, Error: "wa-logged-out"},
		{StateEvent: status.StateBadCredentials, Error: status.BridgeStateErrorCode(secretName)},
	} {
		bsq.Send(state)
		require.Len(t, bsq.ch, 1, "every state is still queued")
		<-bsq.ch
	}
	var got []string
	for _, f := range rec.got {
		got = append(got, f.event+"/"+f.attrs["step"]+"/"+f.attrs["errcode"])
		require.False(t, f.warning)
		require.Nil(t, f.err)
	}
	require.Equal(t, []string{
		"login_lost/bad_credentials/IG_CHALLENGE_REQUIRED",
		"login_lost/bad_credentials/META_COOKIE_REMOVED",
		"login_error/unknown_error/WA_CONNECT_FAILURE_405",
		"login_lost/logged_out/WA_LOGGED_OUT",
		"login_lost/bad_credentials/",
	}, got)
	// The transient disconnect is logged, never reported.
	require.Contains(t, rec.logs.String(), `"failure_event":"login_disconnected"`)
	require.Contains(t, rec.logs.String(), `"failure_errcode":"IG_DGW_CONNECTION_ERROR"`)
	rec.assertClean(t)
}

// One cause, one report: the line next to a ReportFailure is an error only when
// nothing reports the failure.
func TestFailureLogLevelFollowsTheReporter(t *testing.T) {
	SetFailureReporter(nil)
	require.Equal(t, zerolog.ErrorLevel, FailureLogLevel())
	recordFailures(t)
	require.Equal(t, zerolog.WarnLevel, FailureLogLevel())
}
