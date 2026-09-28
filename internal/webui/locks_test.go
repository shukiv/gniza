package webui_test

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/resticrun"
)

// staleRestic is a repository carrying one lock from 2026-09-09 that
// nothing holds, until restic is asked to unlock it.
type staleRestic struct {
	mu       sync.Mutex
	unlocked bool
}

func (s *staleRestic) Exec(_ context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := strings.Join(cmd.Args, " ")
	switch {
	case strings.Contains(line, "list locks"):
		if s.unlocked {
			return resticrun.CommandResult{}, nil
		}
		return resticrun.CommandResult{Stdout: []byte(
			"553a8a7a00000000000000000000000000000000000000000000000000000001\n")}, nil
	case strings.Contains(line, "cat lock"):
		return resticrun.CommandResult{Stdout: []byte(`{"time":"2026-09-09T02:08:11+03:00",` +
			`"exclusive":false,"hostname":"uscp","username":"root","pid":338155}`)}, nil
	case strings.HasSuffix(line, "unlock"):
		s.unlocked = true
		return resticrun.CommandResult{}, nil
	case strings.Contains(line, "forget"):
		return resticrun.CommandResult{Stdout: []byte("[]")}, nil
	}
	return resticrun.CommandResult{}, nil
}

// The page says why retention stopped and offers the one thing that gets
// it going again; pressing it removes the lock and shows a fresh plan.
func TestALockedRepositoryOffersToRemoveItsStaleLocks(t *testing.T) {
	restic := &staleRestic{}
	client, _, engine := newUIWithExec(t, restic)
	store := engine.Store()

	// A disk, which has no key to show: what is kept there is decided on
	// the same page as anywhere else.
	_, repo, err := engine.AddDestination(nodestore.Destination{
		Name: "offsite", Type: "local", Config: map[string]string{"root": t.TempDir()},
	}, nil, "backups")
	if err != nil {
		t.Fatal(err)
	}
	planned := time.Now().Add(-20 * 24 * time.Hour).UTC()
	failed := time.Now().Add(-time.Hour).UTC()
	keeps := nodestore.Retention{KeepDaily: 7}
	repo.InitialisedAt = &planned
	repo.Retention = nodestore.RetentionState{
		PlannedAt: &planned, PlannedKeeps: keeps, AttemptedAt: &failed,
		LastError: "resticrun: restic exited 11: repository is already locked by PID 338155 on uscp",
	}
	if _, err := store.PutRepository(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutPolicy(nodestore.Policy{
		Name: "Nightly", ScheduleCron: "0 2 * * *", Enabled: true,
		RepositoryIDs: []string{repo.ID}, Retention: keeps,
	}); err != nil {
		t.Fatal(err)
	}

	_, page := get(t, client, "/destinations")
	for _, want := range []string{"already locked by PID 338155", "Remove stale locks",
		`action="?p=destinations/unlock"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not say %q", want)
		}
	}
	if strings.Contains(page, `action="?p=destinations/retention/approve"`) {
		t.Error("a plan older than the failure is still offered for approval")
	}

	resp, err := client.PostForm("http://ui/destinations/unlock", url.Values{
		"csrf": {csrfToken(t, page)}, "repository": {repo.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !restic.unlocked {
		t.Fatal("restic was not asked to unlock")
	}
	said, err := url.QueryUnescape(resp.Header.Get("Location"))
	if err != nil || !strings.Contains(said, "1 stale lock removed") {
		t.Errorf("the answer does not say what was removed: %q", said)
	}
	_, after := get(t, client, "/destinations")
	if strings.Contains(after, "Remove stale locks") {
		t.Error("the button is still offered for a repository with no lock on it")
	}
	stored, err := store.Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Retention.LastError != "" || !stored.Retention.PlannedAt.After(failed) {
		t.Errorf("no fresh plan was taken: %+v", stored.Retention)
	}
}
