package bridgev2

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

type knowsCallbackHit struct {
	path string
	auth string
	body map[string]any
}

type knowsCallbackServer struct {
	*httptest.Server
	mu     sync.Mutex
	hits   []knowsCallbackHit
	status []int
	hang   chan struct{}
}

func newKnowsCallbackServer(t *testing.T, status ...int) *knowsCallbackServer {
	s := &knowsCallbackServer{status: status}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(data, &body)
		s.mu.Lock()
		s.hits = append(s.hits, knowsCallbackHit{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
		code := http.StatusOK
		if len(s.status) > 0 {
			code = s.status[0]
			if len(s.status) > 1 {
				s.status = s.status[1:]
			}
		}
		hang := s.hang
		s.mu.Unlock()
		if hang != nil {
			<-hang
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *knowsCallbackServer) got() []knowsCallbackHit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]knowsCallbackHit(nil), s.hits...)
}

func newTestKnowsCallbacks(t *testing.T, srv *knowsCallbackServer) *KnowsCallbacks {
	t.Setenv(KnowsCallbacksEnv, "1")
	oldDelay := KnowsCallbackRetryDelay
	KnowsCallbackRetryDelay = time.Millisecond
	t.Cleanup(func() { KnowsCallbackRetryDelay = oldDelay })
	kc := NewKnowsCallbacks(srv.URL+"/internal/bridge-status?platform=x&shard=x00", "as-token-for-test", zerolog.Nop())
	require.NotNil(t, kc)
	return kc
}

func TestKnowsCallbacksAreOffByDefault(t *testing.T) {
	t.Setenv(KnowsCallbacksEnv, "")
	require.Nil(t, NewKnowsCallbacks("http://api.test/internal/bridge-status", "tok", zerolog.Nop()))
	var kc *KnowsCallbacks
	// A nil client is safe to use and sends nothing.
	kc.PortalReady("login", "thread", "!room:test", true)
	kc.ContactsStaged("login", 3, false)
	kc.ContactChanged("login", KnowsContactChange{ContactID: "c", Change: KnowsContactAdded})
	kc.FlushContactChanges()
	kc.Wait()

	t.Setenv(KnowsCallbacksEnv, "1")
	require.Nil(t, NewKnowsCallbacks("", "tok", zerolog.Nop()), "no status endpoint, no callbacks")
	require.Nil(t, NewKnowsCallbacks("unix:///tmp/sock", "tok", zerolog.Nop()))
}

func TestKnowsPortalReadyPostsTheContractBody(t *testing.T) {
	srv := newKnowsCallbackServer(t)
	kc := newTestKnowsCallbacks(t, srv)
	kc.PortalReady("login-1", "thread-1", "!room:test", true)
	kc.ContactsStaged("login-1", 12, false)
	kc.Wait()
	hits := srv.got()
	require.Len(t, hits, 2)
	byPath := map[string]knowsCallbackHit{}
	for _, h := range hits {
		byPath[h.path] = h
	}
	ready := byPath[KnowsPortalReadyPath]
	require.Equal(t, "Bearer as-token-for-test", ready.auth)
	require.Equal(t, map[string]any{"login_id": "login-1", "thread_id": "thread-1", "room_id": "!room:test", "first_batch": true}, ready.body)
	staged := byPath[KnowsContactsStagedPath]
	require.Equal(t, map[string]any{"login_id": "login-1", "count": float64(12), "complete": false}, staged.body)
	require.NotContains(t, ready.body, "platform", "the platform comes from the token")
}

func TestKnowsCallbackRetriesOnceOn5xxAndNeverOn4xx(t *testing.T) {
	srv := newKnowsCallbackServer(t, 503, 503, 503)
	kc := newTestKnowsCallbacks(t, srv)
	kc.PortalReady("login-1", "thread-1", "!room:test", false)
	kc.Wait()
	require.Len(t, srv.got(), 2, "one retry at most")

	srv404 := newKnowsCallbackServer(t, 404)
	kc = newTestKnowsCallbacks(t, srv404)
	kc.PortalReady("login-1", "thread-1", "!room:test", false)
	kc.Wait()
	require.Len(t, srv404.got(), 1, "an API without the route is not retried")

	srvOK := newKnowsCallbackServer(t, 500, 200)
	kc = newTestKnowsCallbacks(t, srvOK)
	kc.PortalReady("login-1", "thread-1", "!room:test", false)
	kc.Wait()
	require.Len(t, srvOK.got(), 2)
}

func TestKnowsCallbackNeverBlocksTheCaller(t *testing.T) {
	srv := newKnowsCallbackServer(t)
	srv.hang = make(chan struct{})
	kc := newTestKnowsCallbacks(t, srv)
	oldTimeout := KnowsCallbackTimeout
	KnowsCallbackTimeout = 50 * time.Millisecond
	t.Cleanup(func() { KnowsCallbackTimeout = oldTimeout })
	start := time.Now()
	kc.PortalReady("login-1", "thread-1", "!room:test", false)
	require.Less(t, time.Since(start), 20*time.Millisecond)
	done := make(chan struct{})
	go func() { kc.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a hung API kept the callback past its timeouts")
	}
	close(srv.hang)
	require.Len(t, srv.got(), 2, "timed out once, retried once")
}

func TestKnowsContactChangedGroupsPerLogin(t *testing.T) {
	srv := newKnowsCallbackServer(t)
	kc := newTestKnowsCallbacks(t, srv)
	oldWindow := KnowsContactChangedWindow
	KnowsContactChangedWindow = 50 * time.Millisecond
	t.Cleanup(func() { KnowsContactChangedWindow = oldWindow })

	kc.ContactChanged("login-1", KnowsContactChange{ContactID: "c1", Change: KnowsContactName, TimestampMS: 10})
	kc.ContactChanged("login-1",
		KnowsContactChange{ContactID: "c1", Change: KnowsContactName, TimestampMS: 30},
		KnowsContactChange{ContactID: "c2", Change: KnowsContactAdded, TimestampMS: 20})
	kc.ContactChanged("login-2", KnowsContactChange{ContactID: "c9", Change: KnowsContactAvatar, TimestampMS: 5})
	require.Empty(t, srv.got(), "nothing is sent before the window ends")
	require.Eventually(t, func() bool { return len(srv.got()) == 2 }, 2*time.Second, 5*time.Millisecond)
	kc.Wait()
	bodies := map[string][]any{}
	for _, h := range srv.got() {
		require.Equal(t, KnowsContactChangedPath, h.path)
		bodies[h.body["login_id"].(string)] = h.body["changes"].([]any)
	}
	require.Equal(t, []any{
		map[string]any{"contact_id": "c1", "change": "name", "ts_ms": float64(30)},
		map[string]any{"contact_id": "c2", "change": "added", "ts_ms": float64(20)},
	}, bodies["login-1"])
	require.Len(t, bodies["login-2"], 1)
}

func TestKnowsContactChangedSendsEarlyAtTheBatchCap(t *testing.T) {
	srv := newKnowsCallbackServer(t)
	kc := newTestKnowsCallbacks(t, srv)
	oldMax := KnowsContactChangedMaxBatch
	KnowsContactChangedMaxBatch = 3
	t.Cleanup(func() { KnowsContactChangedMaxBatch = oldMax })
	for _, c := range []string{"a", "b", "c"} {
		kc.ContactChanged("login-1", KnowsContactChange{ContactID: c, Change: KnowsContactAdded, TimestampMS: 1})
	}
	require.Eventually(t, func() bool { return len(srv.got()) == 1 }, time.Second, 5*time.Millisecond,
		"sent before the 2 s window once the cap is reached")
	kc.ContactChanged("login-1", KnowsContactChange{ContactID: "d", Change: KnowsContactRemoved, TimestampMS: 1})
	kc.FlushContactChanges()
	kc.Wait()
	require.Len(t, srv.got(), 2)
}
