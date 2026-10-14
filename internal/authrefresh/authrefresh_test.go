package authrefresh

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/authstore"
	"github.com/ZicongCheung/CodeNexus/internal/crypto"
	"github.com/ZicongCheung/CodeNexus/internal/pool"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// countingUpstream records how many refresh calls it receives and rotates the
// token on each call.
type countingUpstream struct {
	mu    sync.Mutex
	n     int
	block chan struct{}
}

func (u *countingUpstream) Source() upstream.Source { return upstream.SourceWorkBuddy }
func (u *countingUpstream) ListModels(context.Context, *account.Account) ([]upstream.ModelSpec, error) {
	return nil, nil
}
func (u *countingUpstream) Chat(context.Context, *account.Account, *upstream.ChatRequest) (*upstream.ChatResponse, error) {
	return nil, nil
}
func (u *countingUpstream) ChatStream(context.Context, *account.Account, *upstream.ChatRequest, upstream.EmitFunc) error {
	return nil
}
func (u *countingUpstream) Probe(context.Context, *account.Account) (*upstream.Probe, error) {
	return nil, nil
}
func (u *countingUpstream) calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.n
}

func (u *countingUpstream) Refresh(_ context.Context, acc *account.Account) error {
	u.mu.Lock()
	u.n++
	block := u.block
	u.mu.Unlock()
	if block != nil {
		<-block // hold the call open so concurrent callers can pile up
	}
	acc.SetCredentials("tok-"+time.Now().Format("150405.000000000"), "", "", 0)
	return nil
}

func withUpstream(t *testing.T, u upstream.Upstream) {
	t.Helper()
	upstream.Register(u)
	t.Cleanup(func() { upstream.Unregister(upstream.SourceWorkBuddy) })
}

func workAccount() *account.Account {
	acc := &account.Account{Source: account.SourceWorkBuddy, UID: "u1", Enabled: true}
	acc.SetCredentials("stale", "rt", "", time.Now().Add(-time.Hour).Unix())
	return acc
}

func TestRefreshNowDeduplicatesConcurrentCalls(t *testing.T) {
	gate := make(chan struct{})
	u := &countingUpstream{block: gate}
	withUpstream(t, u)

	r := New(nil, nil, Options{})
	acc := workAccount()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.RefreshNow(context.Background(), acc)
		}()
	}
	// Give the goroutines time to converge on the single in-flight call.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := u.calls(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1 (singleflight)", got)
	}
	if acc.Token() == "stale" {
		t.Fatal("token was not updated")
	}
}

func TestEnsureFreshSkipsValidToken(t *testing.T) {
	u := &countingUpstream{}
	withUpstream(t, u)

	r := New(nil, nil, Options{Within: 30 * time.Minute})
	acc := &account.Account{Source: account.SourceWorkBuddy, UID: "u1", Enabled: true}
	acc.SetCredentials("good", "rt", "", time.Now().Add(4*time.Hour).Unix())

	if err := r.EnsureFresh(context.Background(), acc); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if got := u.calls(); got != 0 {
		t.Fatalf("refresh calls = %d, want 0 for a valid token", got)
	}
}

func TestEnsureFreshRefreshesExpiringToken(t *testing.T) {
	u := &countingUpstream{}
	withUpstream(t, u)

	r := New(nil, nil, Options{Within: 30 * time.Minute})
	acc := &account.Account{Source: account.SourceWorkBuddy, UID: "u1", Enabled: true}
	acc.SetCredentials("old", "rt", "", time.Now().Add(2*time.Minute).Unix())

	if err := r.EnsureFresh(context.Background(), acc); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if got := u.calls(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func TestRefreshPersistsRotatedToken(t *testing.T) {
	u := &countingUpstream{}
	withUpstream(t, u)

	dir := t.TempDir()
	store := authstore.New(dir, authstore.WithKeeper(crypto.NewMemoryKeeper()))
	acc := workAccount()
	if err := store.Save(acc); err != nil {
		t.Fatal(err)
	}

	r := New(nil, store, Options{})
	if err := r.RefreshNow(context.Background(), acc); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	loaded, err := store.Load()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load: %v, %d", err, len(loaded))
	}
	if loaded[0].Token() == "stale" {
		t.Fatal("rotated token was not persisted")
	}
}

func TestRefreshSkipsEphemeralAccountsForPersistence(t *testing.T) {
	u := &countingUpstream{}
	withUpstream(t, u)

	dir := t.TempDir()
	store := authstore.New(dir, authstore.WithKeeper(crypto.NewMemoryKeeper()))
	acc := workAccount()
	acc.Ephemeral = true

	r := New(nil, store, Options{})
	if err := r.RefreshNow(context.Background(), acc); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	if loaded, _ := store.Load(); len(loaded) != 0 {
		t.Fatal("ephemeral account must not be written to disk")
	}
}

func TestSweepRefreshesOnlyExpiringAccounts(t *testing.T) {
	u := &countingUpstream{}
	withUpstream(t, u)

	p := pool.New(pool.Options{})
	soon := &account.Account{Source: account.SourceWorkBuddy, UID: "soon", Enabled: true}
	soon.SetCredentials("a", "r1", "", time.Now().Add(time.Minute).Unix())
	later := &account.Account{Source: account.SourceWorkBuddy, UID: "later", Enabled: true}
	later.SetCredentials("b", "r2", "", time.Now().Add(6*time.Hour).Unix())
	p.Replace([]*account.Account{soon, later})

	r := New(p, nil, Options{Within: 30 * time.Minute})
	r.sweep(context.Background())

	if got := u.calls(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1 (only the expiring account)", got)
	}
}
