package webui_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/resticrun"
)

// TestAddingADestinationEndsOnItsRecoveryKey covers the one step of
// setting up a destination that cannot be done later.
//
// The repository password is generated on this server and, until somebody
// takes it off, the only copy is on the machine the backups exist to
// survive. It sat behind a menu item on a row an operator had no reason
// to open, so a server could be backing up for months with nothing
// anywhere that could read a single snapshot of it.
//
// Adding a destination now ends on that key, and a schedule that would
// write there waits until it has been taken away.
func TestAddingADestinationEndsOnItsRecoveryKey(t *testing.T) {
	client, _, engine := newUIWithExec(t, quietRestic())

	_, page := get(t, client, "/destinations")
	resp, err := client.PostForm("http://ui/?p=destinations/add", url.Values{
		"csrf": {csrfToken(t, page)}, "type": {"local"},
		"name": {"Spare disk"}, "root": {t.TempDir()}, "repo_path": {"cp01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	added := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("adding the destination answered %d", resp.StatusCode)
	}
	for _, want := range []string{
		"Spare disk is ready. Take its recovery",
		"cannot be done later",
		"Download it as a file",
		"I have stored it somewhere else",
	} {
		if !strings.Contains(added, want) {
			t.Errorf("the page after adding a destination does not say %q", want)
		}
	}

	repositories, err := engine.Store().Repositories()
	if err != nil || len(repositories) != 1 {
		t.Fatalf("repositories = %v, %v", repositories, err)
	}
	repo := repositories[0]
	if !strings.Contains(added, repo.ID) {
		t.Error("the card's buttons do not name the repository they are for")
	}

	// Until the key has been taken away, the schedule page says so before
	// the form is filled in -- the refusal alone landed as a message on
	// another page, and read as the schedule simply not working.
	_, schedulePage := get(t, client, "/schedule")
	if !strings.Contains(schedulePage, "data-recovery-first") || !strings.Contains(schedulePage, "<strong>Spare disk</strong> is still only on this server") {
		t.Error("the schedule page does not say the recovery key has to be taken off first")
	}

	// A schedule that would write there is refused, and the refusal says
	// what to do.
	policyForm := url.Values{
		"csrf": {csrfToken(t, added)}, "name": {"Nightly"}, "cron": {"0 2 * * *"},
		"mode": {"split"}, "repository": {repo.ID}, "scope": {"all"}, "enabled": {"1"},
	}
	resp, err = client.PostForm("http://ui/?p=schedule/save", policyForm)
	if err != nil {
		t.Fatal(err)
	}
	_ = readBody(t, resp)
	policies, err := engine.Store().Policies()
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != 0 {
		t.Fatalf("a schedule was enabled for a destination nothing can read: %+v", policies)
	}
	if location := resp.Header.Get("Location"); !strings.Contains(location, "Recovery+key") &&
		!strings.Contains(location, "recovery+key") {
		t.Errorf("the refusal does not say what to do: %s", location)
	}

	// Downloading the key is having it.
	resp, err = client.PostForm("http://ui/?p=destinations/recovery/card",
		url.Values{"csrf": {csrfToken(t, added)}, "repository": {repo.ID}})
	if err != nil {
		t.Fatal(err)
	}
	card := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(card, "Repository password") {
		t.Fatalf("downloading the recovery key answered %d: %s", resp.StatusCode, card)
	}
	stored, err := engine.Store().Repository(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RecoveryNotedAt == nil {
		t.Fatal("the operator downloaded the recovery key and the interface still " +
			"says it has never left this server")
	}

	// And now the schedule is allowed, and the page no longer warns.
	_, page = get(t, client, "/schedule")
	if strings.Contains(page, "data-recovery-first") {
		t.Error("the schedule page still says the recovery key is only here")
	}
	policyForm.Set("csrf", csrfToken(t, page))
	resp, err = client.PostForm("http://ui/?p=schedule/save", policyForm)
	if err != nil {
		t.Fatal(err)
	}
	_ = readBody(t, resp)
	policies, err = engine.Store().Policies()
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != 1 || !policies[0].Enabled {
		t.Fatalf("the schedule was still refused after the key was taken away: %+v", policies)
	}
}

// quietRestic answers every restic command the interface causes, so a
// destination can be added without one being installed.
func quietRestic() resticrun.Execer {
	return resticrun.ExecFunc(func(_ context.Context, cmd resticrun.Command) (resticrun.CommandResult, error) {
		for _, arg := range cmd.Args {
			if arg == "snapshots" {
				return resticrun.CommandResult{Stdout: []byte("[]")}, nil
			}
		}
		return resticrun.CommandResult{}, nil
	})
}

var _ = nodestore.Repository{}
