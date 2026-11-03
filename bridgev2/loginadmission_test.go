package bridgev2

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

type admissionResolverFunc func(context.Context, id.UserID, networkid.UserLoginID, string) (*LoginAdmission, error)

func (f admissionResolverFunc) ResolveLogin(ctx context.Context, user id.UserID, login networkid.UserLoginID, providerUserID string) (*LoginAdmission, error) {
	return f(ctx, user, login, providerUserID)
}

type admissionTestClient struct {
	NetworkAPI
	disconnects int
}

func (client *admissionTestClient) Disconnect() { client.disconnects++ }

func admissionFixture(t *testing.T) (*User, *UserLogin, *sql.DB) {
	t.Helper()
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = raw.Close() })
	_, err = raw.Exec(`CREATE TABLE user_login (
        bridge_id TEXT, user_mxid TEXT, id TEXT, remote_name TEXT, remote_profile TEXT,
        space_room TEXT, metadata TEXT, PRIMARY KEY(bridge_id,id));
        CREATE TABLE user_portal(login_id TEXT, room_id TEXT);
        INSERT INTO user_login VALUES('test','@user_7:localhost','remote-1','old','{}','!space:localhost','{}');
        INSERT INTO user_portal VALUES('remote-1','!encrypted-history:localhost');`)
	require.NoError(t, err)
	db, err := dbutil.NewWithDB(raw, "sqlite3")
	require.NoError(t, err)
	br := &Bridge{ID: "test", DB: database.New("test", database.MetaTypes{}, db),
		Config: &bridgeconfig.BridgeConfig{}, BackgroundCtx: context.Background(),
		usersByMXID: map[id.UserID]*User{}, userLoginsByID: map[networkid.UserLoginID]*UserLogin{}}
	owner := &User{User: &database.User{BridgeID: "test", MXID: "@user_7:localhost"}, Bridge: br,
		Log: zerolog.Nop(), logins: map[networkid.UserLoginID]*UserLogin{}}
	candidate := &User{User: &database.User{BridgeID: "test", MXID: "@user_99:localhost"}, Bridge: br,
		Log: zerolog.Nop(), logins: map[networkid.UserLoginID]*UserLogin{}}
	login := &UserLogin{UserLogin: &database.UserLogin{BridgeID: "test", UserMXID: owner.MXID, ID: "remote-1",
		RemoteName: "old", SpaceRoom: "!space:localhost", Metadata: map[string]any{}},
		Bridge: br, User: owner, Log: zerolog.Nop(), Client: &admissionTestClient{}}
	owner.logins[login.ID] = login
	br.usersByMXID[owner.MXID] = owner
	br.usersByMXID[candidate.MXID] = candidate
	br.userLoginsByID[login.ID] = login
	return candidate, login, raw
}

func decisionFor(candidate id.UserID, loginID networkid.UserLoginID) *LoginAdmission {
	return &LoginAdmission{Version: 1, CandidateMXID: candidate, LoginID: loginID,
		OwnerMXID: "@user_7:localhost", ShardID: "0", Proof: "opaque-proof"}
}

func TestLoginAdmissionReusesPermanentOwnerWithoutDeletingRooms(t *testing.T) {
	candidate, existing, raw := admissionFixture(t)
	candidate.Bridge.LoginAdmission = admissionResolverFunc(func(ctx context.Context, user id.UserID, login networkid.UserLoginID, providerUserID string) (*LoginAdmission, error) {
		// Network work must happen outside the shared cache lock.
		require.True(t, candidate.Bridge.cacheLock.TryLock())
		candidate.Bridge.cacheLock.Unlock()
		return decisionFor(user, login), nil
	})
	ctx, capture := WithLoginAdmissionCapture(context.Background())
	oldClient := existing.Client.(*admissionTestClient)
	result, err := candidate.NewLogin(ctx, &database.UserLogin{ID: existing.ID, RemoteName: "new", Metadata: map[string]any{"session": "new"}},
		&NewLoginParams{DeleteOnConflict: true, LoadUserLogin: func(ctx context.Context, login *UserLogin) error {
			require.Equal(t, id.UserID("@user_7:localhost"), login.UserMXID)
			login.Client = &admissionTestClient{}
			return nil
		}})
	require.NoError(t, err)
	require.Same(t, existing, result)
	require.Equal(t, 1, oldClient.disconnects)
	require.Empty(t, candidate.logins)
	require.Equal(t, id.RoomID("!space:localhost"), result.SpaceRoom)
	var owner, room string
	require.NoError(t, raw.QueryRow("SELECT user_mxid FROM user_login WHERE id='remote-1'").Scan(&owner))
	require.Equal(t, "@user_7:localhost", owner)
	require.NoError(t, raw.QueryRow("SELECT room_id FROM user_portal WHERE login_id='remote-1'").Scan(&room))
	require.Equal(t, "!encrypted-history:localhost", room)
	require.Equal(t, "opaque-proof", capture.ForLogin(existing.ID).Proof)
}

func TestLoginAdmissionFailureNeverReachesLoginReplacement(t *testing.T) {
	for _, mode := range []string{"unavailable", "owner_conflict", "foreign_domain", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			candidate, existing, raw := admissionFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			candidate.Bridge.LoginAdmission = admissionResolverFunc(func(ctx context.Context, user id.UserID, login networkid.UserLoginID, providerUserID string) (*LoginAdmission, error) {
				if mode == "unavailable" {
					return nil, errors.New("service down")
				}
				if mode == "cancelled" {
					cancel()
				}
				decision := decisionFor(user, login)
				if mode == "owner_conflict" {
					decision.OwnerMXID = user
				}
				if mode == "foreign_domain" {
					decision.OwnerMXID = "@user_7:another.example"
				}
				return decision, nil
			})
			_, err := candidate.NewLogin(ctx, &database.UserLogin{ID: existing.ID, Metadata: map[string]any{}},
				&NewLoginParams{DeleteOnConflict: true, LoadUserLogin: func(context.Context, *UserLogin) error { t.Fatal("unexpected connector load"); return nil }})
			require.Error(t, err)
			require.Equal(t, 0, existing.Client.(*admissionTestClient).disconnects)
			require.Equal(t, "old", existing.RemoteName)
			var count int
			require.NoError(t, raw.QueryRow("SELECT COUNT(*) FROM user_portal").Scan(&count))
			require.Equal(t, 1, count)
		})
	}
}

func TestLoginAdmissionHTTPIdentityAndShardBoundary(t *testing.T) {
	for _, mode := range []string{"ok", "wrong_candidate", "wrong_login", "other_shard", "denied", "invalid_json", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			targetHits := 0
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits++ }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "Bearer shard-secret", r.Header.Get("Authorization"))
				var request LoginAdmissionRequest
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				require.Equal(t, "telegram", request.Platform)
				require.Equal(t, "tg01", request.ShardID)
				decision := decisionFor(request.CandidateMXID, request.LoginID)
				decision.ShardID = "tg01"
				switch mode {
				case "wrong_candidate":
					decision.CandidateMXID = "@user_100:localhost"
				case "wrong_login":
					decision.LoginID = "another-login"
				case "other_shard":
					decision.ShardID = "tg02"
				case "denied":
					w.WriteHeader(http.StatusForbidden)
					return
				case "invalid_json":
					_, _ = w.Write([]byte("not-json"))
					return
				case "redirect":
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
					return
				}
				_ = json.NewEncoder(w).Encode(decision)
			}))
			defer server.Close()
			resolver := NewHTTPLoginAdmission(bridgeconfig.LoginAdmissionConfig{URL: server.URL, Secret: "shard-secret", Platform: "telegram", ShardID: "tg01"})
			result, err := resolver.ResolveLogin(context.Background(), "@user_99:localhost", "remote-1", "")
			if mode == "ok" {
				require.NoError(t, err)
				require.Equal(t, id.UserID("@user_7:localhost"), result.OwnerMXID)
			} else {
				require.Error(t, err)
				require.Nil(t, result)
				require.NotContains(t, err.Error(), "opaque-proof")
			}
			if mode == "other_shard" {
				var response RespError
				require.ErrorAs(t, err, &response)
				require.Equal(t, "FI.MAU.BRIDGE.LOGIN_ADMISSION_OTHER_SHARD", response.ErrCode)
			}
			require.Equal(t, 0, targetHits)
		})
	}
}

func TestLoginAdmissionCaptureIsPerAttemptAndProofIsNotLogged(t *testing.T) {
	first, c1 := WithLoginAdmissionCapture(context.Background())
	second, c2 := WithLoginAdmissionCapture(context.Background())
	d1 := decisionFor("@user_99:localhost", "remote-1")
	d2 := decisionFor("@user_100:localhost", "remote-1")
	d2.Proof = "other-proof"
	captureLoginAdmission(first, d1)
	captureLoginAdmission(second, d2)
	require.Equal(t, d1, c1.ForLogin("remote-1"))
	require.Equal(t, d2, c2.ForLogin("remote-1"))
	require.Nil(t, c1.ForLogin("unknown"))
	var log strings.Builder
	logger := zerolog.New(&log)
	logger.Info().Object("step", &LoginStep{Type: LoginStepTypeComplete, CompleteParams: &LoginCompleteParams{UserLoginID: "remote-1", LoginAdmission: d1}}).Msg("complete")
	require.NotContains(t, log.String(), "opaque-proof")
	require.Contains(t, log.String(), "remote-1")
}

func TestLoginAdmissionUsesVerifiedProviderIDWithoutChangingBridgeLoginID(t *testing.T) {
	candidate, existing, raw := admissionFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request LoginAdmissionRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.Equal(t, "instagram", request.Platform)
		require.Equal(t, "12345", request.ProviderUserID)
		require.Equal(t, existing.ID, request.LoginID)
		_ = json.NewEncoder(w).Encode(decisionFor(request.CandidateMXID, request.LoginID))
	}))
	defer server.Close()
	candidate.Bridge.LoginAdmission = NewHTTPLoginAdmission(bridgeconfig.LoginAdmissionConfig{
		URL: server.URL, Secret: "shard-secret", Platform: "instagram", ShardID: "0"})
	result, err := candidate.NewLogin(context.Background(),
		&database.UserLogin{ID: existing.ID, Metadata: map[string]any{}}, &NewLoginParams{
			ProviderUserID: "12345", DeleteOnConflict: true,
			LoadUserLogin: func(ctx context.Context, login *UserLogin) error {
				login.Client = &admissionTestClient{}
				return nil
			},
		})
	require.NoError(t, err)
	require.Same(t, existing, result)
	var room string
	require.NoError(t, raw.QueryRow("SELECT room_id FROM user_portal WHERE login_id='remote-1'").Scan(&room))
	require.Equal(t, "!encrypted-history:localhost", room)
}
