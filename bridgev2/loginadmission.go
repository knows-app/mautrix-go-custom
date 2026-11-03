// Copyright (c) 2026 Knows contributors
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

// LoginAdmissionRequest is emitted only by NewLogin, after a connector proves
// the provider login. The deployment credential binds the platform and shard;
// client-supplied handles and Matrix user IDs are never ownership proof.
type LoginAdmissionRequest struct {
	Version        int                   `json:"version"`
	Platform       string                `json:"platform"`
	ShardID        string                `json:"shard_id"`
	CandidateMXID  id.UserID             `json:"candidate_mxid"`
	LoginID        networkid.UserLoginID `json:"login_id"`
	ProviderUserID string                `json:"provider_user_id,omitempty"`
}

type LoginAdmission struct {
	Version       int                   `json:"version"`
	CandidateMXID id.UserID             `json:"candidate_mxid"`
	LoginID       networkid.UserLoginID `json:"login_id"`
	OwnerMXID     id.UserID             `json:"owner_mxid"`
	ShardID       string                `json:"shard_id"`
	Proof         string                `json:"proof"`
}

// LoginAdmissionResolver selects a permanent Matrix owner and original shard.
// It must never merge customers or move existing logins between shards.
type LoginAdmissionResolver interface {
	ResolveLogin(context.Context, id.UserID, networkid.UserLoginID, string) (*LoginAdmission, error)
}

type httpLoginAdmission struct {
	config bridgeconfig.LoginAdmissionConfig
	client *http.Client
}

var ErrLoginAdmissionUnavailable = RespError{
	ErrCode:    "FI.MAU.BRIDGE.LOGIN_ADMISSION_UNAVAILABLE",
	Err:        "Could not confirm the account owner. Please retry.",
	StatusCode: http.StatusServiceUnavailable,
}
var ErrLoginAdmissionConflict = RespError{
	ErrCode:    "FI.MAU.BRIDGE.LOGIN_ADMISSION_CONFLICT",
	Err:        "The existing connection does not match the confirmed account owner.",
	StatusCode: http.StatusConflict,
}

// NewHTTPLoginAdmission uses a deployment-configured callback without forwarding redirects.
func NewHTTPLoginAdmission(config bridgeconfig.LoginAdmissionConfig) *httpLoginAdmission {
	return &httpLoginAdmission{config: config, client: &http.Client{
		Timeout: 8 * time.Second,
		// Never forward the service credential to a redirect destination.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (resolver *httpLoginAdmission) ResolveLogin(ctx context.Context, candidate id.UserID, loginID networkid.UserLoginID, providerUserID string) (*LoginAdmission, error) {
	cfg := resolver.config
	endpoint, err := url.Parse(cfg.URL)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		cfg.Secret == "" || cfg.Platform == "" || cfg.ShardID == "" || loginID == "" {
		return nil, ErrLoginAdmissionUnavailable
	}
	host := net.ParseIP(endpoint.Hostname())
	if endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && (cfg.AllowInsecureHTTP || endpoint.Hostname() == "localhost" || host != nil && host.IsLoopback())) {
		return nil, ErrLoginAdmissionUnavailable
	}
	body, err := json.Marshal(LoginAdmissionRequest{1, cfg.Platform, cfg.ShardID, candidate, loginID, providerUserID})
	if err != nil {
		return nil, ErrLoginAdmissionUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, ErrLoginAdmissionUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+cfg.Secret)
	request.Header.Set("Content-Type", "application/json")
	response, err := resolver.client.Do(request)
	if err != nil {
		return nil, ErrLoginAdmissionUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrLoginAdmissionUnavailable
	}
	var decision LoginAdmission
	decoder := json.NewDecoder(io.LimitReader(response.Body, 16*1024))
	if err = decoder.Decode(&decision); err != nil {
		return nil, ErrLoginAdmissionUnavailable
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return nil, ErrLoginAdmissionUnavailable
	}
	if !validLoginAdmission(&decision, candidate, loginID) {
		return nil, ErrLoginAdmissionUnavailable
	}
	if decision.ShardID != cfg.ShardID {
		// The gateway may use the proof to recover the still-active original
		// connection. This process must not create/delete a login on this shard.
		return nil, RespError{
			ErrCode:    "FI.MAU.BRIDGE.LOGIN_ADMISSION_OTHER_SHARD",
			Err:        "Continue signing in on the account's original server.",
			StatusCode: http.StatusConflict,
			ExtraData:  map[string]any{"login_admission": &decision},
		}
	}
	return &decision, nil
}

func validLoginAdmission(decision *LoginAdmission, candidate id.UserID, loginID networkid.UserLoginID) bool {
	if decision == nil || decision.Version != 1 || decision.CandidateMXID != candidate ||
		decision.LoginID != loginID || decision.ShardID == "" || decision.Proof == "" || len(decision.Proof) > 8192 {
		return false
	}
	_, candidateServer, candidateErr := candidate.ParseAndValidateRelaxed()
	ownerLocalpart, ownerServer, ownerErr := decision.OwnerMXID.ParseAndValidateRelaxed()
	return candidateErr == nil && ownerErr == nil && ownerLocalpart != "" && candidateServer == ownerServer
}

func (user *User) resolveLoginAdmission(ctx context.Context, loginID networkid.UserLoginID, providerUserID string) (*User, *LoginAdmission, error) {
	resolver := user.Bridge.LoginAdmission
	if resolver == nil && user.Bridge.Config != nil && user.Bridge.Config.LoginAdmission != nil {
		resolver = NewHTTPLoginAdmission(*user.Bridge.Config.LoginAdmission)
	}
	if resolver == nil {
		return user, nil, nil
	}
	decision, err := resolver.ResolveLogin(ctx, user.MXID, loginID, providerUserID)
	if err != nil {
		return nil, nil, err
	}
	if !validLoginAdmission(decision, user.MXID, loginID) {
		return nil, nil, ErrLoginAdmissionUnavailable
	}
	if err = ctx.Err(); err != nil {
		return nil, nil, err
	}
	if decision.OwnerMXID == user.MXID {
		return user, decision, nil
	}
	owner, err := user.Bridge.GetUserByMXID(ctx, decision.OwnerMXID)
	if err != nil {
		return nil, nil, ErrLoginAdmissionUnavailable
	}
	return owner, decision, nil
}

// Capture proof in the login process context, not the shared UserLogin cache:
// concurrent attempts for the same provider ID must not exchange credentials.
type loginAdmissionContextKey struct{}
type LoginAdmissionCapture struct {
	lock      sync.Mutex
	decisions map[networkid.UserLoginID]LoginAdmission
}

func WithLoginAdmissionCapture(ctx context.Context) (context.Context, *LoginAdmissionCapture) {
	capture := &LoginAdmissionCapture{decisions: make(map[networkid.UserLoginID]LoginAdmission)}
	return context.WithValue(ctx, loginAdmissionContextKey{}, capture), capture
}

func (capture *LoginAdmissionCapture) ForLogin(loginID networkid.UserLoginID) *LoginAdmission {
	if capture == nil {
		return nil
	}
	capture.lock.Lock()
	defer capture.lock.Unlock()
	decision, ok := capture.decisions[loginID]
	if !ok {
		return nil
	}
	return &decision
}

func captureLoginAdmission(ctx context.Context, decision *LoginAdmission) {
	capture, _ := ctx.Value(loginAdmissionContextKey{}).(*LoginAdmissionCapture)
	if capture == nil || decision == nil {
		return
	}
	capture.lock.Lock()
	defer capture.lock.Unlock()
	capture.decisions[decision.LoginID] = *decision
}
