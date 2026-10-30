package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/shukiv/gniza/internal/store"
)

// Scheduler turns policy schedules into queued jobs and returns abandoned
// jobs to the queue.
type Scheduler struct {
	service *Service
	log     *slog.Logger

	// Tick is how often schedules are evaluated. A minute is enough
	// resolution for cron expressions and keeps the query load trivial.
	Tick time.Duration
	// CatchUpWindow bounds how far back a restart will look. A controller
	// that was down for a week should run tonight's backup, not seven of
	// them at once.
	CatchUpWindow time.Duration
}

// NewScheduler builds a Scheduler.
func NewScheduler(service *Service, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{
		service:       service,
		log:           log,
		Tick:          time.Minute,
		CatchUpWindow: 2 * time.Hour,
	}
}

// Run evaluates schedules until the context is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.Tick)
	defer ticker.Stop()

	for {
		if err := s.RunOnce(ctx, time.Now()); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Error("scheduler tick", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// RunOnce performs one scheduling pass: reclaim abandoned leases, then
// queue any policy whose next fire time has passed.
func (s *Scheduler) RunOnce(ctx context.Context, now time.Time) error {
	if _, err := s.service.ReclaimLeases(ctx); err != nil {
		return err
	}

	policies, err := s.service.store.ListPolicies(ctx)
	if err != nil {
		return err
	}
	for _, policy := range policies {
		if err := s.runPolicy(ctx, policy, now); err != nil {
			// One malformed schedule must not stop the others.
			s.log.Error("schedule policy", "policy", policy.Name, "error", err)
		}
	}
	return nil
}

func (s *Scheduler) runPolicy(ctx context.Context, policy store.Policy, now time.Time) error {
	schedule, err := cron.ParseStandard(policy.ScheduleCron)
	if err != nil {
		return fmt.Errorf("controller: parse schedule %q: %w", policy.ScheduleCron, err)
	}

	lastRun, err := s.service.store.PolicyLastRun(ctx, policy.ID)
	if err != nil {
		return err
	}
	// A policy that has never run, or has been dormant longer than the
	// catch-up window, starts from the edge of that window: we want the
	// next scheduled backup, not a backlog of missed ones.
	earliest := now.Add(-s.CatchUpWindow)
	if lastRun.IsZero() || lastRun.Before(earliest) {
		lastRun = earliest
	}
	// In the scheduler's own zone: a cron line is read in the zone of the time
	// it is handed, and one read in UTC comes due at the wrong hour as well.
	if next := schedule.Next(lastRun.In(now.Location())); next.After(now) {
		return nil
	}

	accounts, err := s.service.store.AccountsForPolicy(ctx, policy.ID)
	if err != nil {
		return err
	}
	if err := s.service.store.SetPolicyLastRun(ctx, policy.ID, now); err != nil {
		return err
	}

	var queued, skipped int
	for _, account := range accounts {
		_, err := s.service.store.CreateJob(ctx, account.ID, policy.ID)
		switch {
		case errors.Is(err, store.ErrNoWork):
			// The previous run is still going. Skipping is correct:
			// two concurrent pkgacct runs for one account would
			// double its staging footprint.
			skipped++
		case err != nil:
			s.log.Error("queue job", "policy", policy.Name,
				"account", account.CPanelUser, "error", err)
		default:
			queued++
		}
	}
	s.log.Info("policy fired", "policy", policy.Name,
		"queued", queued, "skipped_still_running", skipped)
	return nil
}
