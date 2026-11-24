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
}

// KnowsContactsMaxWait caps the wait_ms of a contact list request.
var KnowsContactsMaxWait = 12 * time.Second

// GetContactListWait is GetContactList for a caller that can wait up to wait
// (at most KnowsContactsMaxWait) for the login's first contact list (Knows,
// wait_ms). It answers as soon as the connector says the list is ready
// (bridgev2.KnowsContactListReadyNetworkAPI); a connector that cannot tell is
// answered at once, as GetContactList. Each answer is logged with its timing.
func GetContactListWait(ctx context.Context, login *bridgev2.UserLogin, wait time.Duration) (*RespGetContactList, error) {
	start := time.Now()
	var listReady *bool
	if wait > 0 && login.Client != nil && login.Client.IsLoggedIn() {
		if readyAPI, ok := login.Client.(bridgev2.KnowsContactListReadyNetworkAPI); ok {
			ready := knowsWaitContactList(ctx, readyAPI, min(wait, KnowsContactsMaxWait))
			listReady = &ready
		}
	}
	waited := time.Since(start)
	resp, err := GetContactList(ctx, login)
	if err != nil {
		return nil, err
	}
	resp.KnowsListReady = listReady
	logContactListAnswer(ctx, login, resp, start, waited, wait)
	return resp, nil
}

func knowsWaitContactList(ctx context.Context, api bridgev2.KnowsContactListReadyNetworkAPI, wait time.Duration) bool {
	ready := api.KnowsContactListReady()
	select {
	case <-ready:
		return true
	default:
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
	if login.Client == nil || !login.Client.IsLoggedIn() {
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
	if login.Client == nil || !login.Client.IsLoggedIn() {
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
