// Copyright (c) 2026 Knows
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
)

// Failure event names. Each failure bridgev2 handles itself (it turns it into a
// message status, a bridge state or an HTTP error and carries on) is reported
// under one of these, so every bridge built on this fork reports the same
// causes under the same names.
const (
	// FailureSend is a Matrix event the bridge could not deliver to the remote
	// network: every failed message status bridgev2 sends.
	FailureSend = "send_failed"
	// FailureReceive is a remote event that could not be bridged into its room.
	FailureReceive = "receive_failed"
	// FailureBackfill is a history fetch or send that failed.
	FailureBackfill = "backfill_failed"
	// FailureLoginStep is a provisioning login start or step that failed.
	FailureLoginStep = "login_step_failed"
	// FailureLoginLost is a login the remote network logged out or no longer
	// accepts (BAD_CREDENTIALS, or LOGGED_OUT carrying an error code).
	FailureLoginLost = "login_lost"
	// FailureLoginError is a login that hit an unknown error (UNKNOWN_ERROR);
	// bridgev2 may still reconnect it on its own.
	FailureLoginError = "login_error"
	// FailureLoginDisconnected is a transient disconnect. It is only logged,
	// never passed to the FailureReporter: it usually recovers by itself and
	// would use up the reporter's budget.
	FailureLoginDisconnected = "login_disconnected"
)

// FailureReporterFunc receives one failure. event is one of the Failure*
// names, warning marks expected outcomes that should not page, err is the
// underlying error (may be nil) and attrs holds only the keys step, errcode,
// outcome and status, each already checked to be a code word: never message
// text, a name, a phone number, a username, an ID or a token.
//
// Its signature is knows-errorreport's HandledErr, so a bridge wires it with
//
//	bridgev2.SetFailureReporter(errorreport.HandledErr)
//
// The reporter must not log at error level about the failure itself (bridgev2
// already logs it) and must not block.
type FailureReporterFunc func(event string, warning bool, err error, attrs map[string]string)

var failureReporter atomic.Pointer[FailureReporterFunc]

// SetFailureReporter installs the function every handled failure is passed
// to, in addition to the structured log line. nil removes it.
func SetFailureReporter(fn FailureReporterFunc) {
	if fn == nil {
		failureReporter.Store(nil)
		return
	}
	failureReporter.Store(&fn)
}

var (
	reFailureStep    = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	reFailureCode    = regexp.MustCompile(`^[A-Z][A-Z0-9_]*(?:\.[A-Z0-9_]+)*$`)
	reFailureOutcome = regexp.MustCompile(`^[a-z_]{1,24}$`)
	reFailureStatus  = regexp.MustCompile(`^[1-5][0-9]{2}$`)
)

// failureCodeWord turns a bridge's error code into the code word form a
// reporter accepts: upper case, dashes as underscores (ig-dgw-connection-error
// becomes IG_DGW_CONNECTION_ERROR, m.foreign_network_error becomes
// M.FOREIGN_NETWORK_ERROR). Anything that is still not a code word, or longer
// than 64 characters, is dropped, so free text never passes as a code.
func failureCodeWord(code string) string {
	code = strings.ReplaceAll(strings.ToUpper(strings.TrimSpace(code)), "-", "_")
	if len(code) == 0 || len(code) > 64 || !reFailureCode.MatchString(code) {
		return ""
	}
	return code
}

// cleanFailureAttrs keeps the allowlisted keys whose values have the right shape.
func cleanFailureAttrs(attrs map[string]string) map[string]string {
	out := make(map[string]string, len(attrs))
	for k, v := range attrs {
		if v == "" {
			continue
		}
		switch k {
		case "step":
			if !reFailureStep.MatchString(v) {
				v = "other"
			}
		case "errcode":
			v = failureCodeWord(v)
		case "outcome":
			if !reFailureOutcome.MatchString(v) {
				v = ""
			}
		case "status":
			if !reFailureStatus.MatchString(v) {
				v = ""
			}
		default:
			v = ""
		}
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// ReportFailure records one handled failure: a structured log line at warn
// level (message "Bridge failure", field failure_event and the code-word
// attributes, plus whatever the context logger already carries), and a call to
// the FailureReporter when one is set. event and the step must be fixed values
// from code (a constant or an allowlist), never text from a message, a user or
// a remote answer; errcode is the bridge's own error code. The log line is at
// warn level so a logger that forwards error lines does not report the failure
// twice; a line logged right next to the call uses FailureLogLevel. A failure caused
// by cancellation (shutdown, a closed request) is not a failure and is skipped.
func ReportFailure(ctx context.Context, event string, warning bool, err error, attrs map[string]string) {
	if err != nil && errors.Is(err, context.Canceled) {
		return
	}
	clean := cleanFailureAttrs(attrs)
	logFailure(ctx, event, warning, err, clean)
	if fn := failureReporter.Load(); fn != nil {
		(*fn)(event, warning, err, clean)
	}
}

// FailureLogLevel is the level of a log line that stands right next to a
// ReportFailure call for the same failure: error when nothing reports it, warn
// when a FailureReporter is installed, so a logger that forwards error lines to
// the same place as the reporter does not open a second issue for one cause.
func FailureLogLevel() zerolog.Level {
	if failureReporter.Load() != nil {
		return zerolog.WarnLevel
	}
	return zerolog.ErrorLevel
}

func logFailure(ctx context.Context, event string, warning bool, err error, clean map[string]string) {
	evt := zerolog.Ctx(ctx).Warn().
		Str("failure_event", event).
		Bool("failure_expected", warning)
	for _, k := range []string{"step", "errcode", "outcome", "status"} {
		if v, ok := clean[k]; ok {
			evt = evt.Str("failure_"+k, v)
		}
	}
	if err != nil {
		evt = evt.Str("failure_error_type", fmt.Sprintf("%T", err))
	}
	evt.Msg("Bridge failure")
}

// failureErrcode is the error code an error carries, if any: a Matrix or
// bridgev2 errcode, or a message status reason.
func failureErrcode(err error) string {
	if err == nil {
		return ""
	}
	if re, ok := errors.AsType[RespError](err); ok {
		return re.ErrCode
	}
	if re, ok := errors.AsType[mautrix.RespError](err); ok {
		return re.ErrCode
	}
	if ms, ok := errors.AsType[MessageStatus](err); ok {
		return string(ms.ErrorReason)
	}
	return ""
}

// failureHTTPStatus is the HTTP status an error carries, if any.
func failureHTTPStatus(err error) int {
	if re, ok := errors.AsType[RespError](err); ok {
		return re.StatusCode
	}
	if re, ok := errors.AsType[mautrix.RespError](err); ok {
		return re.StatusCode
	}
	return 0
}

// failureMessageTypes are the message types a send_failed step may name.
var failureMessageTypes = map[event.MessageType]bool{
	event.MsgText: true, event.MsgImage: true, event.MsgVideo: true, event.MsgAudio: true, event.MsgFile: true,
	event.MsgNotice: true, event.MsgEmote: true, event.MsgLocation: true, "m.sticker": true,
}

// failureEventTypes are the other Matrix event types a send_failed step may name.
var failureEventTypes = map[string]bool{
	event.EventReaction.Type:             true,
	event.EventRedaction.Type:            true,
	event.EventSticker.Type:              true,
	event.StateRoomName.Type:             true,
	event.StateTopic.Type:                true,
	event.StateRoomAvatar.Type:           true,
	event.StateMember.Type:               true,
	event.StatePowerLevels.Type:          true,
	event.StateJoinRules.Type:            true,
	event.EventUnstablePollStart.Type:    true,
	event.EventUnstablePollResponse.Type: true,
}

// sendFailureStep is the step of a send_failed report: the message type of a
// message when it is a standard one, the event type when it is one bridgev2
// handles, else "other". Both come from the event the sender wrote, so any
// other value could carry text or split one issue per sender.
func sendFailureStep(evt *event.Event) string {
	if evt == nil {
		return "other"
	}
	if evt.Type == event.EventMessage {
		msgType := evt.Content.AsMessage().MsgType
		if raw, ok := evt.Content.Raw["msgtype"].(string); ok && msgType == "" {
			msgType = event.MessageType(raw)
		}
		if failureMessageTypes[msgType] {
			return string(msgType)
		}
		return "other"
	}
	if failureEventTypes[evt.Type.Type] {
		return evt.Type.Type
	}
	return "other"
}

// reportSendFailure reports a failed message status.
func reportSendFailure(ctx context.Context, evt *event.Event, err error, ms *MessageStatus) {
	if ms.Status == event.MessageStatusSuccess || ms.Status == event.MessageStatusPending {
		return
	}
	outcome := strings.ToLower(string(ms.Status))
	if errors.Is(err, context.DeadlineExceeded) {
		outcome = "timeout"
	}
	// An unsupported event or one the user may not send is the expected answer
	// to what the user did, not a fault of the bridge.
	warning := ms.ErrorReason == event.MessageStatusUnsupported || ms.ErrorReason == event.MessageStatusNoPermission
	errcode := failureErrcode(err)
	if errcode == "" {
		errcode = string(ms.ErrorReason)
	}
	ReportFailure(ctx, FailureSend, warning, err, map[string]string{
		"step":    sendFailureStep(evt),
		"errcode": errcode,
		"outcome": outcome,
	})
}

// remoteEventStep is the step of a receive_failed report: the remote event
// type in snake case (message, edit, reaction_remove, ...).
func remoteEventStep(evtType RemoteEventType) string {
	name := strings.TrimPrefix(evtType.String(), "RemoteEvent")
	var b strings.Builder
	for i, c := range name {
		if c >= 'A' && c <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			c += 'a' - 'A'
		}
		b.WriteRune(c)
	}
	if step := b.String(); reFailureStep.MatchString(step) {
		return step
	}
	return "other"
}

// receiveFailureIsMinor marks remote events whose loss loses no content.
func receiveFailureIsMinor(evtType RemoteEventType) bool {
	switch evtType {
	case RemoteEventTyping, RemoteEventReadReceipt, RemoteEventDeliveryReceipt, RemoteEventMarkUnread:
		return true
	}
	return false
}

// reportReceiveFailure reports a remote event whose handling failed.
func reportReceiveFailure(ctx context.Context, evtType RemoteEventType, res EventHandlingResult) {
	if res.Success || res.Error == nil {
		return
	}
	ReportFailure(ctx, FailureReceive, receiveFailureIsMinor(evtType), res.Error, map[string]string{
		"step":    remoteEventStep(evtType),
		"errcode": failureErrcode(res.Error),
	})
}

// reportBackfillFailure reports a failed history fetch or send.
func reportBackfillFailure(ctx context.Context, step string, err error) {
	if err == nil || errors.Is(err, errNoMessagesLeftAfterCutoff) {
		return
	}
	ReportFailure(ctx, FailureBackfill, false, err, map[string]string{
		"step":    step,
		"errcode": failureErrcode(err),
	})
}

// ReportLoginStepFailure reports a login start or step that failed. A 4xx
// answer (wrong password, invalid code, too many logins) is the expected
// answer to the user's input and is reported as a warning.
func ReportLoginStepFailure(ctx context.Context, step string, err error) {
	if err == nil {
		return
	}
	attrs := map[string]string{
		"step":    step,
		"errcode": failureErrcode(err),
	}
	httpStatus := failureHTTPStatus(err)
	if httpStatus != 0 {
		attrs["status"] = fmt.Sprintf("%d", httpStatus)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		attrs["outcome"] = "timeout"
	}
	ReportFailure(ctx, FailureLoginStep, httpStatus >= 400 && httpStatus < 500, err, attrs)
}

// reportBridgeState reports a login that was lost or hit an error, once per
// change of state event or error code (prev is the previous state queued for
// this login, nil if none).
func reportBridgeState(ctx context.Context, state status.BridgeState, prev *status.BridgeState) {
	var failureEvent string
	switch state.StateEvent {
	case status.StateBadCredentials:
		failureEvent = FailureLoginLost
	case status.StateLoggedOut:
		// A logout the user asked for carries no error code: not a failure.
		if state.Error == "" {
			return
		}
		failureEvent = FailureLoginLost
	case status.StateUnknownError:
		failureEvent = FailureLoginError
	case status.StateTransientDisconnect:
		failureEvent = FailureLoginDisconnected
	default:
		return
	}
	if prev != nil && prev.StateEvent == state.StateEvent && prev.Error == state.Error {
		return
	}
	attrs := cleanFailureAttrs(map[string]string{
		"step":    strings.ToLower(string(state.StateEvent)),
		"errcode": string(state.Error),
	})
	if failureEvent == FailureLoginDisconnected {
		logFailure(ctx, failureEvent, true, nil, attrs)
		return
	}
	ReportFailure(ctx, failureEvent, false, nil, attrs)
}
