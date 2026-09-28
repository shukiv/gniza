package webui_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
)

// overviewData reads the overview the way a program would.
func overviewData(t *testing.T, client *http.Client) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://ui/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var page struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("the overview is not JSON: %v", err)
	}
	return page.Data
}

// The finding from the fleet check of 2026-09-22: on three servers the
// backup of the server's own configuration had failed every night since
// it was installed, one repository could not have anything removed from
// it, and each overview said "ok".
func TestTheOverviewSaysWhatIsWrongWithTheServerItself(t *testing.T) {
	client, _, engine := newUI(t)
	store := engine.Store()
	_, repo, err := engine.AddDestination(nodestore.Destination{
		Name: "offsite", Type: "local", Config: map[string]string{"root": t.TempDir()},
	}, nil, "backups")
	if err != nil {
		t.Fatal(err)
	}
	made := time.Now().Add(-30 * 24 * time.Hour).UTC()
	repo.InitialisedAt = &made
	if _, err := store.PutRepository(repo); err != nil {
		t.Fatal(err)
	}
	policy, err := store.PutPolicy(nodestore.Policy{
		Name: "Nightly", ScheduleCron: "0 2 * * *", Enabled: true, IncludeSystem: true,
		RepositoryIDs: []string{repo.ID}, Retention: nodestore.Retention{KeepDaily: 7},
	})
	if err != nil {
		t.Fatal(err)
	}

	if data := overviewData(t, client); len(asList(data["Concerns"])) != 0 {
		t.Fatalf("a server with nothing wrong has concerns: %v", data["Concerns"])
	}

	finished := time.Now().Add(-3 * time.Hour).UTC()
	if _, err := store.PutJob(nodestore.Job{
		PolicyID: policy.ID, Account: cpanel.SystemAccount, Status: job.StatusFailed,
		StagingErr: "directadmin: not established against a running DirectAdmin server",
		FinishedAt: &finished,
	}); err != nil {
		t.Fatal(err)
	}
	attempted := time.Now().Add(-time.Hour).UTC()
	checked := time.Now().Add(-2 * time.Hour).UTC()
	repo.Retention = nodestore.RetentionState{AttemptedAt: &attempted,
		LastError: "resticrun: the repository is locked: restic exited 11"}
	repo.Check = nodestore.RepositoryCheck{CheckedAt: &checked, Passed: false,
		Problem: "pack 3f2a1c9e: not referenced in any index"}
	if _, err := store.PutRepository(repo); err != nil {
		t.Fatal(err)
	}

	data := overviewData(t, client)
	if data["Severity"] != "bad" {
		t.Errorf("severity = %v with a repository that failed its check", data["Severity"])
	}
	if got := len(asList(data["Concerns"])); got != 3 {
		t.Errorf("%d concerns, want the check, the lock and the configuration", got)
	}
	_, page := get(t, client, "/")
	for _, want := range []string{
		"did not pass its integrity check", "is locked",
		"own configuration was not backed up", `href="?p=logs&amp;tab=system"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the overview does not say %q", want)
		}
	}
}

func asList(value any) []any {
	list, _ := value.([]any)
	return list
}
