package appservice

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func knowsGetReady(as *AppService, url string) (int, map[string]any) {
	w := httptest.NewRecorder()
	as.GetReady(w, httptest.NewRequest(http.MethodGet, url, nil))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func TestKnowsReadyWithLogins(t *testing.T) {
	as := &AppService{}
	code, body := knowsGetReady(as, "/_matrix/mau/ready")
	require.Equal(t, http.StatusInternalServerError, code)
	require.Empty(t, body)

	as.Ready = true
	code, body = knowsGetReady(as, "/_matrix/mau/ready")
	require.Equal(t, http.StatusOK, code, "the plain readiness does not wait for the logins")
	require.Empty(t, body, "and its answer is unchanged")

	code, body = knowsGetReady(as, "/_matrix/mau/ready?logins=1")
	require.Equal(t, http.StatusInternalServerError, code)
	require.Equal(t, false, body["logins_ready"])

	as.KnowsLogins.Set(3, 10, false)
	code, body = knowsGetReady(as, "/_matrix/mau/ready?logins=1")
	require.Equal(t, http.StatusInternalServerError, code)
	require.Equal(t, float64(3), body["logins_started"])
	require.Equal(t, float64(10), body["logins_total"])

	as.KnowsLogins.Set(10, 10, true)
	code, body = knowsGetReady(as, "/_matrix/mau/ready?logins=1")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, true, body["logins_ready"])

	as.Ready = false
	code, _ = knowsGetReady(as, "/_matrix/mau/ready?logins=1")
	require.Equal(t, http.StatusInternalServerError, code, "logins loaded but appservice not ready")
}
