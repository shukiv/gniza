package resticrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ForgetPlan is what a retention run would remove, or did.
//
// It exists so an operator can read the answer before agreeing to it.
// Deleting backups is the one thing this program does that cannot be
// undone, and "keep seven daily" is easy to write and hard to picture.
type ForgetPlan struct {
	Groups []ForgetGroup
	// Kept and Removed are the totals across every group.
	Kept    int
	Removed int
}

// ForgetGroup is one account's backups on one server: the unit a keep
// count actually counts.
type ForgetGroup struct {
	Host    string
	Account string
	Tags    []string
	Kept    int
	Removed int
	// Protected means unverified source reads kept this entire group. A
	// partial snapshot must never satisfy a keep count for a complete one.
	Protected bool
	// Oldest and Newest of what would go, so the operator can see the
	// span rather than only the count.
	Oldest time.Time
	Newest time.Time
}

type retentionGroupJSON struct {
	Host      string     `json:"host"`
	Tags      []string   `json:"tags"`
	Keep      []Snapshot `json:"keep"`
	Remove    []Snapshot `json:"remove"`
	Protected bool       `json:"cprest_protected,omitempty"`
}

// ParseForgetPlan reads what "restic forget --json" said.
//
// restic writes two different things to stdout under --json: the plan, as
// an array of groups, or a single object describing why it could not run
// at all. The second is what a stale lock looks like, and reading it as
// an empty plan would report "nothing to remove" for a repository whose
// retention has in fact never run.
func ParseForgetPlan(stdout []byte) (ForgetPlan, error) {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return ForgetPlan{}, nil
	}
	if trimmed[0] == '{' {
		var failure struct {
			MessageType string `json:"message_type"`
			Code        int    `json:"code"`
			Message     string `json:"message"`
		}
		if err := json.Unmarshal(trimmed, &failure); err == nil && failure.Message != "" {
			return ForgetPlan{}, fmt.Errorf("resticrun: forget: %s", failure.Message)
		}
		return ForgetPlan{}, fmt.Errorf("resticrun: forget said something unreadable")
	}

	var groups []retentionGroupJSON
	if err := json.Unmarshal(trimmed, &groups); err != nil {
		return ForgetPlan{}, fmt.Errorf("resticrun: read the retention plan: %w", err)
	}

	var plan ForgetPlan
	for _, group := range groups {
		if len(group.Remove) == 0 && len(group.Keep) == 0 {
			continue
		}
		summary := ForgetGroup{
			Host: group.Host, Tags: group.Tags,
			Kept: len(group.Keep), Removed: len(group.Remove),
			Protected: group.Protected,
		}
		for _, tag := range group.Tags {
			if account, found := strings.CutPrefix(tag, "account:"); found {
				summary.Account = account
				break
			}
		}
		for _, snapshot := range group.Remove {
			if summary.Oldest.IsZero() || snapshot.Time.Before(summary.Oldest) {
				summary.Oldest = snapshot.Time
			}
			if snapshot.Time.After(summary.Newest) {
				summary.Newest = snapshot.Time
			}
		}
		plan.Kept += summary.Kept
		plan.Removed += summary.Removed
		plan.Groups = append(plan.Groups, summary)
	}
	// Whatever would lose the most, first: that is what an operator is
	// deciding about.
	sort.Slice(plan.Groups, func(i, j int) bool {
		if plan.Groups[i].Removed != plan.Groups[j].Removed {
			return plan.Groups[i].Removed > plan.Groups[j].Removed
		}
		return plan.Groups[i].Account < plan.Groups[j].Account
	})
	return plan, nil
}

// ForgetPlanned runs forget and reports what it did, or in a dry run what
// it would do.
// ForgetSnapshots removes named snapshots, whatever any keep policy says.
//
// Retention decides what to keep out of what a schedule produced. This is
// the other thing: a customer has gone, and their backups are to go with
// them. Naming the snapshots is what makes that safe -- a forget by tag
// with no policy would take whatever happened to carry the tag at the
// moment it ran, and the caller has already shown the operator a count.
//
// Pruning is separate and is not done here: it walks the whole repository
// and takes the lock for as long as that takes. The space comes back with
// the next prune.
func (r *Runner) ForgetSnapshots(ctx context.Context, repo Repository, ids []string) error {
	if len(ids) == 0 {
		return fmt.Errorf("resticrun: no snapshot was named to forget")
	}
	args := []string{"forget"}
	for _, id := range ids {
		if err := validateSnapshotID(id); err != nil {
			return err
		}
		if id == "latest" {
			// "latest" is a lookup, not a name. Forgetting it would
			// delete whichever snapshot happened to be newest.
			return fmt.Errorf("resticrun: %q is not a snapshot to forget", id)
		}
		args = append(args, id)
	}
	result, err := r.run(ctx, repo, args, secondary{}, nil)
	if err != nil {
		return err
	}
	if err := classifyExit(result.ExitCode, result.Stderr, false); err != nil {
		return err
	}
	return refusedRemoval(result.Stderr)
}

func (r *Runner) ForgetPlanned(ctx context.Context, repo Repository, spec ForgetSpec) (ForgetPlan, error) {
	if _, err := ForgetArgs(spec); err != nil {
		return ForgetPlan{}, err
	}
	// Always preview first and delete only the exact safe IDs from that
	// preview. Re-running a policy could let an incomplete backup arriving
	// between these steps evict a known-good one.
	preview := spec
	preview.DryRun, preview.Prune = true, false
	args, err := ForgetArgs(preview)
	if err != nil {
		return ForgetPlan{}, err
	}
	result, err := r.run(ctx, repo, args, secondary{}, nil)
	if err != nil {
		return ForgetPlan{}, err
	}
	if err := classifyExit(result.ExitCode, result.Stderr, false); err != nil {
		return ForgetPlan{}, err
	}
	if result.Truncated {
		// The plan is what an operator agrees to and what gets applied.
		// A cut-off one reads as a smaller, complete answer.
		return ForgetPlan{}, fmt.Errorf(
			"resticrun: the retention plan was too large to read in full")
	}
	if _, err := ParseForgetPlan(result.Stdout); err != nil {
		return ForgetPlan{}, err
	}
	if len(bytes.TrimSpace(result.Stdout)) == 0 {
		return ForgetPlan{}, nil
	}
	var groups []retentionGroupJSON
	if err := json.Unmarshal(result.Stdout, &groups); err != nil {
		return ForgetPlan{}, err
	}
	needsReceipts := false
	for _, group := range groups {
		for _, snapshot := range append(append([]Snapshot(nil), group.Keep...), group.Remove...) {
			needsReceipts = needsReceipts || hasTag(snapshot.Tags, NeedsCompletionTag)
		}
	}
	var completed map[string]bool
	if needsReceipts {
		completed, err = r.completionReceipts(ctx, repo)
		if err != nil {
			return ForgetPlan{}, err
		}
	}
	var safe []retentionGroupJSON
	var remove []string
	for _, group := range groups {
		if hasTag(group.Tags, CompletionReceiptTag) {
			// Receipts are tiny, immutable evidence. They are not account
			// backups and must outlive every payload that relies on them.
			continue
		}
		for _, snapshot := range append(append([]Snapshot(nil), group.Keep...), group.Remove...) {
			if spec.ProtectedSnapshotIDs[snapshot.ID] || (hasTag(snapshot.Tags, NeedsCompletionTag) && !completed[snapshot.ID]) {
				group.Protected = true
			}
		}
		if group.Protected {
			// Conservatively pause expiry for this account/mode group until
			// its incomplete snapshots have been reviewed and removed. Other
			// accounts' retention can still run.
			group.Keep = append(group.Keep, group.Remove...)
			group.Remove = nil
		}
		for _, snapshot := range group.Remove {
			remove = append(remove, snapshot.ID)
		}
		safe = append(safe, group)
	}
	encoded, err := json.Marshal(safe)
	if err != nil {
		return ForgetPlan{}, err
	}
	plan, err := ParseForgetPlan(encoded)
	if err != nil {
		return ForgetPlan{}, err
	}
	if !spec.DryRun && len(remove) > 0 {
		if err := r.ForgetSnapshots(ctx, repo, remove); err != nil {
			return ForgetPlan{}, err
		}
		if spec.Prune {
			if err := r.Prune(ctx, repo); err != nil {
				return plan, err
			}
		}
	}
	return plan, nil
}

// Prune reclaims the space snapshots were holding.
//
// It is separate from forget because it is the expensive half: forget
// rewrites a little metadata, prune walks the whole repository. Running
// it only when forget actually removed something is the difference
// between a nightly job that costs nothing and one that reads every pack
// file for no reason.
func (r *Runner) Prune(ctx context.Context, repo Repository) error {
	result, err := r.run(ctx, repo, []string{"prune"}, secondary{}, nil)
	if err != nil {
		return err
	}
	if err := classifyExit(result.ExitCode, result.Stderr, false); err != nil {
		return err
	}
	return refusedRemoval(result.Stderr)
}
