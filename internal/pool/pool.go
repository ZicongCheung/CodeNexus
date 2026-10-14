// Package pool holds the unified account pool and selects accounts for requests.
package pool

import (
	"errors"
	"sync"

	"github.com/ZicongCheung/CodeNexus/internal/account"
	"github.com/ZicongCheung/CodeNexus/internal/upstream"
)

// ErrNoAccount is returned when no enabled account matches the request.
var ErrNoAccount = errors.New("pool: no available account")

// Pool is a concurrency-safe holder of accounts with a simple round-robin pick.
type Pool struct {
	mu       sync.RWMutex
	accounts []*account.Account
	rr       int
}

// New returns an empty pool.
func New() *Pool { return &Pool{} }

// Replace swaps the whole account set.
func (p *Pool) Replace(accts []*account.Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accounts = accts
}

// List returns a copy of all accounts.
func (p *Pool) List() []*account.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*account.Account, len(p.accounts))
	copy(out, p.accounts)
	return out
}

// Pick selects the next enabled account for source using round-robin.
func (p *Pool) Pick(source upstream.Source) (*account.Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var cands []*account.Account
	for _, a := range p.accounts {
		if a.Enabled && a.Source == source {
			cands = append(cands, a)
		}
	}
	if len(cands) == 0 {
		return nil, ErrNoAccount
	}
	a := cands[p.rr%len(cands)]
	p.rr++
	return a, nil
}

// Stats reports enabled and total account counts.
func (p *Pool) Stats() (enabled, total int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		total++
		if a.Enabled {
			enabled++
		}
	}
	return
}

// HasSource reports whether any enabled account exists for source.
func (p *Pool) HasSource(source upstream.Source) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		if a.Enabled && a.Source == source {
			return true
		}
	}
	return false
}
