package matrix

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix/bridgev2"
)

func TestKnowsCreateDMFirstBatchIsOptIn(t *testing.T) {
	for _, tc := range []struct {
		url        string
		createChat bool
		want       bool
	}{
		{"/v3/create_dm/someone", true, false},
		{"/v3/create_dm/someone?first_batch=1", true, true},
		{"/v3/create_dm/someone?first_batch=0", true, false},
		{"/v3/resolve_identifier/someone?first_batch=1", false, false},
	} {
		r := httptest.NewRequest("POST", tc.url, nil)
		require.Equal(t, tc.want, bridgev2.KnowsWantsFirstBatch(knowsResolveContext(r, tc.createChat)), tc.url)
	}
}
