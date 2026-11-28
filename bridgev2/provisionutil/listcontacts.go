// Copyright (c) 2025 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provisionutil

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
)

type RespGetContactList struct {
	Contacts []*RespResolveIdentifier `json:"contacts"`
	// Knows: present only when the request asked for wait_ms and the bridge can tell when its
	// first contact list is ready: true once it is, false when the wait ran out first.
	KnowsListReady *bool `json:"knows_list_ready,omitempty"`
	// Knows: present only while the network rate limits the login
	// (bridgev2.KnowsContactsRateLimitedNetworkAPI).
	RateLimited *bridgev2.KnowsRateLimit `json:"rate_limited,omitempty"`
}

// KnowsContactsMaxWait caps the wait_ms of a contact list request.
var KnowsContactsMaxWait = 12 * time.Second

// GetContactListWait is GetContactList for a caller that can wait up to wait
// (at most KnowsContactsMaxWait) for the login's first contact list (Knows,
// wait_ms). It answers as soon as the connector says the list is ready
// (bridgev2.KnowsContactListReadyNetworkAPI); a connector that cannot tell
// (no interface, or a nil channel) is answered at once, as GetContactList,
// without knows_list_ready, and so is a login the network rate
// limits (bridgev2.KnowsContactsRateLimitedNetworkAPI), whose answer carries
// rate_limited. A login that is still connecting is waited for first
// (bridgev2.UserLogin.KnowsWaitLoggedIn), and that time counts in wait. Each
// answer is logged with its timing.
func GetContactListWait(ctx context.Context, login *bridgev2.UserLogin, wait time.Duration) (*RespGetContactList, error) {
	start := time.Now()
	if !knowsLoggedIn(ctx, login) {
		return nil, bridgev2.ErrNotLoggedIn
	}
	var listReady *bool
	if wait > 0 {
		// A nil channel means the connector cannot tell (e.g. its switch is off): answered
		// at once without knows_list_ready, as a connector without the interface.
		if readyAPI, ok := login.Client.(bridgev2.KnowsContactListReadyNetworkAPI); ok {
			if readyChan := readyAPI.KnowsContactListReady(); readyChan != nil {
				// The time spent waiting for the login to connect counts in the caller's wait.
				capped := min(wait, KnowsContactsMaxWait) - time.Since(start)
				if knowsContactsRateLimited(login) != nil {
					capped = 0
				}
				ready := knowsWaitContactList(ctx, readyChan, capped)
				listReady = &ready
			}
		}
	}
	waited := time.Since(start)
	resp, err := GetContactList(ctx, login)
	if err != nil {
		return nil, err
	}
	resp.KnowsListReady = listReady
	resp.RateLimited = knowsContactsRateLimited(login)
	logContactListAnswer(ctx, login, resp, start, waited, wait)
	return resp, nil
}

// knowsLoggedIn is whether the login's client is logged in, waiting up to
// bridgev2.KnowsLoginStartWait for a login that is still connecting (Knows:
// the app asks for the contacts right after the last login step, before the
// client is connected). The wait is logged when there was one.
func knowsLoggedIn(ctx context.Context, login *bridgev2.UserLogin) bool {
	if login.Client != nil && login.Client.IsLoggedIn() {
		return true
	}
	start := time.Now()
	loggedIn := login.KnowsWaitLoggedIn(ctx, bridgev2.KnowsLoginStartWait())
	if waited := time.Since(start); waited >= time.Millisecond {
		zerolog.Ctx(ctx).Info().
			Int64("login_waited_ms", waited.Milliseconds()).
			Bool("logged_in", loggedIn).
			Msg("Waited for a connecting login")
	}
	return loggedIn
}

// knowsContactsRateLimited is the login's current rate limit, nil when the
// connector cannot tell or knows none.
func knowsContactsRateLimited(login *bridgev2.UserLogin) *bridgev2.KnowsRateLimit {
	if limitedAPI, ok := login.Client.(bridgev2.KnowsContactsRateLimitedNetworkAPI); ok {
		return limitedAPI.KnowsContactsRateLimited()
	}
	return nil
}

// knowsWaitContactList waits up to wait for ready to be closed. A nil ready
// (the connector cannot tell) is never waited on.
func knowsWaitContactList(ctx context.Context, ready <-chan struct{}, wait time.Duration) bool {
	if ready == nil {
		return false
	}
	select {
	case <-ready:
		return true
	default:
	}
	if wait <= 0 {
		return false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ready:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// logContactListAnswer is the contacts.first_answer_ms measure: the time a
// contact list request took (wait included), with the number of contacts and,
// when the connector says, whether the list is complete. Counts only.
func logContactListAnswer(ctx context.Context, login *bridgev2.UserLogin, resp *RespGetContactList, start time.Time, waited, wait time.Duration) {
	answerMS := time.Since(start).Milliseconds()
	evt := zerolog.Ctx(ctx).Info().
		Int64("contacts_answer_ms", answerMS).
		Int64("contacts_waited_ms", waited.Milliseconds()).
		Int64("contacts_wait_ms", wait.Milliseconds()).
		Int("contacts_count", len(resp.Contacts))
	if resp.KnowsListReady != nil {
		evt = evt.Bool("contacts_list_ready", *resp.KnowsListReady)
	}
	if resp.RateLimited != nil {
		evt = evt.Int("contacts_rate_limited_s", resp.RateLimited.RetryAfterS)
	}
	if staged, ok := login.Client.(interface {
		KnowsStaged() (int, bool, bool)
	}); ok {
		if _, complete, ready := staged.KnowsStaged(); ready {
			evt = evt.Bool("contacts_complete", complete)
		}
	}
	if login.KnowsFirstContactsAnswer() {
		evt = evt.Int64("contacts_first_answer_ms", answerMS)
	}
	evt.Msg("Contact list answered")
}

type RespSearchUsers struct {
	Results []*RespResolveIdentifier `json:"results"`
}

func GetContactList(ctx context.Context, login *bridgev2.UserLogin) (*RespGetContactList, error) {
	if !knowsLoggedIn(ctx, login) {
		return nil, bridgev2.ErrNotLoggedIn
	}
	api, ok := login.Client.(bridgev2.ContactListingNetworkAPI)
	if !ok {
		return nil, bridgev2.RespError(mautrix.MUnrecognized.WithMessage("This bridge does not support listing contacts"))
	}
	resp, err := api.GetContactList(ctx)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to get contact list")
		return nil, err
	}
	contacts, err := processResolveIdentifiers(ctx, login.Bridge, resp, false)
	if err != nil {
		return nil, err
	}
	return &RespGetContactList{
		Contacts: contacts,
	}, nil
}

func SearchUsers(ctx context.Context, login *bridgev2.UserLogin, query string) (*RespSearchUsers, error) {
	if !knowsLoggedIn(ctx, login) {
		return nil, bridgev2.ErrNotLoggedIn
	}
	api, ok := login.Client.(bridgev2.UserSearchingNetworkAPI)
	if !ok {
		return nil, bridgev2.RespError(mautrix.MUnrecognized.WithMessage("This bridge does not support searching for users"))
	}
	resp, err := api.SearchUsers(ctx, query)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to search for users")
		return nil, err
	}
	results, err := processResolveIdentifiers(ctx, login.Bridge, resp, true)
	if err != nil {
		return nil, err
	}
	return &RespSearchUsers{
		Results: results,
	}, nil
}

func processResolveIdentifiers(ctx context.Context, br *bridgev2.Bridge, resp []*bridgev2.ResolveIdentifierResponse, syncInfo bool) ([]*RespResolveIdentifier, error) {
	apiResp := make([]*RespResolveIdentifier, len(resp))
	for i, contact := range resp {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		apiContact := &RespResolveIdentifier{
			ID: contact.UserID,
		}
		apiResp[i] = apiContact
		if contact.UserInfo != nil {
			if contact.UserInfo.Name != nil {
				apiContact.Name = *contact.UserInfo.Name
			}
			if contact.UserInfo.Identifiers != nil {
				apiContact.Identifiers = contact.UserInfo.Identifiers
			}
		}
		if contact.Ghost != nil {
			if syncInfo && contact.UserInfo != nil {
				contact.Ghost.UpdateInfo(zerolog.Ctx(ctx).WithContext(br.BackgroundCtx), contact.UserInfo)
			}
			if contact.Ghost.Name != "" {
				apiContact.Name = contact.Ghost.Name
			}
			if len(contact.Ghost.Identifiers) >= len(apiContact.Identifiers) {
				apiContact.Identifiers = contact.Ghost.Identifiers
			}
			apiContact.AvatarURL = contact.Ghost.AvatarMXC
			apiContact.MXID = contact.Ghost.Intent.GetMXID()
		}
		if contact.Chat != nil {
			if contact.Chat.Portal == nil {
				var err error
				contact.Chat.Portal, err = br.GetExistingPortalByKey(ctx, contact.Chat.PortalKey)
				if err != nil {
					zerolog.Ctx(ctx).Err(err).Msg("Failed to get portal")
				}
			}
			if contact.Chat.Portal != nil {
				apiContact.DMRoomID = contact.Chat.Portal.MXID
			}
		}
	}
	return apiResp, nil
}
