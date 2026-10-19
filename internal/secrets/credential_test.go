package secrets

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStore is an in-memory SecretStore for credential tests.
type fakeStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newFakeStore() *fakeStore { return &fakeStore{data: map[string][]byte{}} }

func (s *fakeStore) Get(_ context.Context, ref string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.data[ref]
	if !ok {
		return nil, errors.New("not found")
	}
	out := make([]byte, len(d))
	copy(out, d)
	return out, nil
}

func (s *fakeStore) Put(_ context.Context, ref string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := make([]byte, len(data))
	copy(d, data)
	s.data[ref] = d
	return nil
}

func (s *fakeStore) Delete(_ context.Context, ref string) error { return nil }
func (s *fakeStore) Health(_ context.Context) error             { return nil }

// fakeRefresher counts calls and rotates tokens.
type fakeRefresher struct {
	calls int32
}

func (f *fakeRefresher) RefreshCredential(_ context.Context, cred *Credential) (*Credential, error) {
	atomic.AddInt32(&f.calls, 1)
	out := *cred
	out.AccessToken = "refreshed-access"
	out.RefreshToken = "refreshed-rotate"
	out.ExpiresAt = time.Now().Add(10 * 24 * time.Hour)
	return &out, nil
}

type fakePersister struct {
	mu    sync.Mutex
	saved map[string]*Credential
}

func newFakePersister() *fakePersister { return &fakePersister{saved: map[string]*Credential{}} }

func (p *fakePersister) PersistRefreshed(_ context.Context, ref string, cred *Credential) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := *cred
	p.saved[ref] = &c
	return nil
}

func TestGetCredential_RefreshesExpiredOAuthToken(t *testing.T) {
	store := newFakeStore()
	ref := "provider-credential:pcr_1"
	store.data[ref] = []byte(`{"access_token":"old-access","refresh_token":"old-refresh","account_id":"acct","expires_at":"2000-01-01T00:00:00Z"}`)

	rf := &fakeRefresher{}
	ps := newFakePersister()
	cs := NewCredentialStore(store)
	cs.SetOAuthRefresher(rf)
	cs.SetPersister(ps)

	cred, err := cs.GetCredential(context.Background(), ref)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if cred.AccessToken != "refreshed-access" {
		t.Errorf("access_token = %q, want refreshed-access", cred.AccessToken)
	}
	if cred.RefreshToken != "refreshed-rotate" {
		t.Errorf("refresh_token = %q, want rotated", cred.RefreshToken)
	}
	if atomic.LoadInt32(&rf.calls) != 1 {
		t.Errorf("refresh called %d times, want 1", rf.calls)
	}
	saved := ps.saved[ref]
	if saved == nil || saved.RefreshToken != "refreshed-rotate" {
		t.Errorf("persisted cred = %+v, want rotated refresh token", saved)
	}
}

func TestGetCredential_DoesNotRefreshValidToken(t *testing.T) {
	store := newFakeStore()
	ref := "provider-credential:pcr_2"
	store.data[ref] = []byte(`{"access_token":"good","refresh_token":"rt","expires_at":"2099-01-01T00:00:00Z"}`)
	rf := &fakeRefresher{}
	cs := NewCredentialStore(store)
	cs.SetOAuthRefresher(rf)
	cs.SetPersister(newFakePersister())

	cred, err := cs.GetCredential(context.Background(), ref)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if cred.AccessToken != "good" {
		t.Errorf("access_token = %q, want good (no refresh)", cred.AccessToken)
	}
	if rf.calls != 0 {
		t.Errorf("refresh called %d times, want 0", rf.calls)
	}
}

func TestGetCredential_SingleflightsConcurrentRefresh(t *testing.T) {
	store := newFakeStore()
	ref := "provider-credential:pcr_3"
	store.data[ref] = []byte(`{"access_token":"old","refresh_token":"rt","expires_at":"2000-01-01T00:00:00Z"}`)

	// Blocking refresher: waits for the test to release the gate so every
	// concurrent goroutine is guaranteed to be inside sf.Do before refresh runs.
	gate := make(chan struct{})
	br := &blockingRefresher{gate: gate}
	cs := NewCredentialStore(store)
	cs.SetOAuthRefresher(br)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = cs.GetCredential(context.Background(), ref)
		}()
	}
	// Give all goroutines a moment to enter sf.Do, then release the single
	// coalesced refresh.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	if br.calls() != 1 {
		t.Errorf("refresh called %d times, want 1 (singleflight)", br.calls())
	}
}

type blockingRefresher struct {
	gate chan struct{}
	n    int32
}

func (b *blockingRefresher) RefreshCredential(_ context.Context, cred *Credential) (*Credential, error) {
	atomic.AddInt32(&b.n, 1)
	<-b.gate
	out := *cred
	out.AccessToken = "refreshed"
	out.ExpiresAt = time.Now().Add(10 * 24 * time.Hour)
	return &out, nil
}

func (b *blockingRefresher) calls() int32 { return atomic.LoadInt32(&b.n) }

func TestGetCredential_RefreshFailureCachesStaleAndBacksOff(t *testing.T) {
	store := newFakeStore()
	ref := "provider-credential:pcr_4"
	store.data[ref] = []byte(`{"access_token":"old","refresh_token":"rt","expires_at":"2000-01-01T00:00:00Z"}`)

	var calls int32
	failing := failingRefresher{calls: &calls}
	cs := NewCredentialStore(store)
	cs.SetOAuthRefresher(&failing)

	// First call attempts refresh and fails, returning the stale cred.
	cred, err := cs.GetCredential(context.Background(), ref)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if cred.AccessToken != "old" {
		t.Errorf("access_token = %q, want stale 'old' on refresh failure", cred.AccessToken)
	}
	first := atomic.LoadInt32(&calls)
	if first != 1 {
		t.Fatalf("refresh called %d times, want 1", first)
	}
	// Second call within the failure backoff window must NOT re-attempt refresh.
	_, _ = cs.GetCredential(context.Background(), ref)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("refresh called %d times after backoff, want 1", got)
	}
}

type failingRefresher struct{ calls *int32 }

func (f *failingRefresher) RefreshCredential(_ context.Context, _ *Credential) (*Credential, error) {
	atomic.AddInt32(f.calls, 1)
	return nil, errors.New("token endpoint down")
}

func TestNeedsRefresh(t *testing.T) {
	cases := []struct {
		name string
		cred *Credential
		want bool
	}{
		{"no refresh token", &Credential{AccessToken: "a"}, false},
		{"valid far future", &Credential{AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(24 * time.Hour)}, false},
		{"expired", &Credential{AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(-time.Minute)}, true},
		{"near expiry within skew", &Credential{AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(2 * time.Minute)}, true},
		{"missing access token but has refresh", &Credential{RefreshToken: "r"}, true},
		{"unknown expiry ignored", &Credential{AccessToken: "a", RefreshToken: "r"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cred.NeedsRefresh(); got != c.want {
				t.Errorf("NeedsRefresh=%v, want %v", got, c.want)
			}
		})
	}
}
