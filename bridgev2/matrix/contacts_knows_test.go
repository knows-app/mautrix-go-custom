package matrix

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKnowsWaitMSIsOptInAndCapped(t *testing.T) {
	for url, want := range map[string]time.Duration{
		"/v3/contacts":                 0,
		"/v3/contacts?wait_ms=":        0,
		"/v3/contacts?wait_ms=abc":     0,
		"/v3/contacts?wait_ms=-5":      0,
		"/v3/contacts?wait_ms=1500":    1500 * time.Millisecond,
		"/v3/contacts?wait_ms=9999999": 12 * time.Second,
	} {
		require.Equal(t, want, knowsWaitMS(httptest.NewRequest(http.MethodGet, url, nil)), url)
	}
}
