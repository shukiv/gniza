package webui

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const featureCacheTTL = 30 * time.Second

type featureDecision struct {
	allowed bool
	until   time.Time
}

// accountFeatureGate enforces the panel's switch -- Feature Manager on
// cPanel, the self-service setting on DirectAdmin -- behind the public
// Unix socket. The page in the panel also checks it for a useful
// page-level refusal, but an account owns processes that can connect to
// the socket directly.
type accountFeatureGate struct {
	mu        sync.Mutex
	decisions map[string]featureDecision
	check     func(context.Context, string) (bool, error)
	slots     chan struct{}
}

func newAccountFeatureGate(check func(context.Context, string) (bool, error)) accountFeatureGate {
	return accountFeatureGate{
		decisions: map[string]featureDecision{},
		check:     check,
		// Do not let many distinct local UIDs turn the root service into an
		// unbounded whmapi1 process launcher on a large shared server.
		slots: make(chan struct{}, 8),
	}
}

func (g *accountFeatureGate) allowed(ctx context.Context, account string) (bool, error) {
	now := time.Now()
	g.mu.Lock()
	decision, found := g.decisions[account]
	g.mu.Unlock()
	if found && now.Before(decision.until) {
		return decision.allowed, nil
	}

	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	case <-ctx.Done():
		return false, ctx.Err()
	}

	allowed, err := g.check(ctx, account)
	if err != nil {
		return false, err
	}
	g.mu.Lock()
	g.decisions[account] = featureDecision{allowed: allowed, until: now.Add(featureCacheTTL)}
	g.mu.Unlock()
	return allowed, nil
}

// selfServiceCheck is the question behind the gate: the panel's own
// switch where it has one (cPanel's Feature Manager), the self-service
// setting where it has none (DirectAdmin), and no where there is no panel
// to ask. The setting is read from the store, not from the copy the
// engine loaded at startup, so a host who turns it on sees it take
// effect without a restart.
func (s *Server) selfServiceCheck(ctx context.Context, account string) (bool, error) {
	if s.engine == nil {
		return false, errors.New("webui: no panel to ask")
	}
	if gate, ok := s.engine.SelfServiceGate(); ok {
		return gate.SelfServiceAllowed(ctx, account)
	}
	if s.holder == nil {
		return false, nil
	}
	settings, err := s.engine.Store().Settings()
	if err != nil {
		return false, fmt.Errorf("webui: read the self-service setting: %w", err)
	}
	return settings.SelfServiceAllows(s.holder.Package(account)), nil
}
