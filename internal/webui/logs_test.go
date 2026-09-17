package webui_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
)

// seedLogs puts a small history in the store: two runs of alice, the
// newer failed; one of bob with everything a run now records; an event.
func seedLogs(t *testing.T, store *nodestore.Store) (aliceGood, aliceFailed, bob nodestore.Job) {
	t.Helper()
	dest, err := store.PutDestination(nodestore.Destination{Name: "offsite", Type: "local", Config: map[string]string{"root": t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := store.PutRepository(nodestore.Repository{DestinationID: dest.ID, Path: "gniza"})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := store.PutPolicy(nodestore.Policy{Name: "Every night", Enabled: true, RepositoryIDs: []string{repo.ID}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	put := func(stored nodestore.Job, hoursAgo int) nodestore.Job {
		stored.ID = nodestore.NewID()
		stored.PolicyID = policy.ID
		stored.QueuedAt = now.Add(-time.Duration(hoursAgo) * time.Hour)
		started, finished := stored.QueuedAt.Add(time.Second), stored.QueuedAt.Add(101*time.Second)
		stored.StartedAt, stored.FinishedAt = &started, &finished
		saved, err := store.PutJob(stored)
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	aliceGood = put(nodestore.Job{Account: "alice", Status: job.StatusSuccess, CompleteAccount: true,
		Targets: []nodestore.JobTarget{{RepositoryID: repo.ID, Status: job.TargetSuccess, SnapshotID: "6fbc8afb5ec5aaaabbbbcccc",
			BytesAdded: 1 << 20, BytesProcessed: 50 << 20, DurationSecs: 95}}}, 48)
	put(nodestore.Job{Account: "alice", Status: job.StatusSuccess, CompleteAccount: true,
		Targets: []nodestore.JobTarget{{RepositoryID: repo.ID, Status: job.TargetSuccess, SnapshotID: "0ld0ld0ld0ld"}}}, 72)
	aliceFailed = put(nodestore.Job{Account: "alice", Status: job.StatusFailed, StagingErr: "pkgacct ran out of disk"}, 24)
	bob = put(nodestore.Job{Account: "bob", Status: job.StatusSuccess, CompleteAccount: true, PolicyName: "Nightly, as it was called",
		Staged: &nodestore.JobStaged{Mode: "split", Parts: []string{"metadata", "homedir"}, Paths: []string{"/var/lib/gniza/staging/bob/metadata", "/home/bob"},
			Databases: []string{"bob_shop", "bob_wp"}, Skipped: []string{"email"}},
		Targets: []nodestore.JobTarget{{RepositoryID: repo.ID, Status: job.TargetSuccess, SnapshotID: "b0bb0bb0bb0b", BytesAdded: 2 << 20,
			BytesProcessed: 80 << 20, DurationSecs: 61, FilesNew: 12, FilesChanged: 3, FilesUnmodified: 123441, FilesTotal: 123456}}}, 12)
	if _, err := store.PutLifecycleEvent(nodestore.LifecycleEvent{Event: "create", Account: "carol", OK: true}); err != nil {
		t.Fatal(err)
	}
	return aliceGood, aliceFailed, bob
}

// TestTheLogsSayWhatARunBackedUpAndCanBeSearched: a row says what was
// backed up, under which schedule and how long it took; its detail says
// when, where each copy went, what restic counted and the snapshot in
// full; a run from before that was recorded says so. A search reads the
// whole history by any word on a row, and the tabs carry it.
func TestTheLogsSayWhatARunBackedUpAndCanBeSearched(t *testing.T) {
	client, _, engine := newUI(t)
	_, _, bob := seedLogs(t, engine.Store())

	_, page := get(t, client, "/logs")
	for _, want := range []string{
		"<th data-nosort>What</th>", "<th>Took</th>", "settings, files, 2 databases",
		`<div class="cpr:sublabel">Nightly, as it was called</div>`, `<div class="cpr:sublabel">Every night</div>`,
		`data-dialog="report-` + bob.ID + `">Details</button>`,
		"The backup of bob on ", "<th>Schedule</th>", "<th>Started</th>", "<th>Finished</th>", "took 1m 40s",
		"/home/bob", "bob_shop, bob_wp", "<th>Left out by the schedule</th><td>mail</td>",
		"The copy at offsite", "Local disk or mounted NAS", "b0bb0bb0bb0b",
		"123,456 read: 12 new, 3 changed, 123,441 as they were", "80.0 MiB read, 2.0 MiB of it new to the destination",
		"This run is from before Gniza wrote down what a run backed up.",
		"Nothing was stored: pkgacct ran out of disk",
		`name="q"`, `data-dialog="clear-logs"`, `action="?p=logs/clear"`, `name="kind" value="backups" checked`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the Logs page lacks %q", want)
		}
	}

	// Every word has to be on the row; the search is over all of it.
	for query, rows := range map[string][]string{
		"bob_wp":           {"bob"},
		"OFFSITE b0bb":     {"bob"},
		"disk":             {"alice"},
		"alice 6fbc8afb":   {"alice"},
		"alice bob":        nil,
		"every night fail": {"alice"},
	} {
		_, found := get(t, client, "/logs?q="+url.QueryEscape(query))
		body := found
		if at := strings.Index(found, "<tbody"); at >= 0 {
			body = found[at:]
		} else if rows != nil {
			t.Errorf("searching %q shows no table", query)
		}
		for _, account := range []string{"alice", "bob"} {
			has := strings.Contains(body, `<span class="cpr:font-semibold">`+account+`</span>`)
			if has != contains(rows, account) {
				t.Errorf("searching %q: %s shown = %v", query, account, has)
			}
		}
		if rows == nil && !strings.Contains(found, "Nothing here matches that.") {
			t.Errorf("searching %q does not say nothing matched", query)
		}
	}
	_, found := get(t, client, "/logs?q=bob")
	for _, want := range []string{
		`href="?p=logs&amp;tab=restores&amp;q=bob"`, `value="bob"`, "1 of 4 here", `href="?p=logs&amp;tab=backups">Show everything</a>`,
	} {
		if !strings.Contains(found, want) {
			t.Errorf("a search for bob lacks %q", want)
		}
	}
	if _, events := get(t, client, "/logs?tab=lifecycle&q=carol"); !strings.Contains(events, "carol") {
		t.Error("the account events are not searched")
	}
	if _, events := get(t, client, "/logs?tab=lifecycle&q=nobody"); strings.Contains(events, "<td class=\"cpr:font-semibold\">carol") {
		t.Error("an account event that does not match is shown")
	}
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// TestClearingTheLogsKeepsWhatSaysAnAccountIsProtected: clearing removes
// the history that was ticked and keeps the last run of each account and
// its last good copy, says how many of each, and clears nothing when
// nothing is ticked.
func TestClearingTheLogsKeepsWhatSaysAnAccountIsProtected(t *testing.T) {
	client, _, engine := newUI(t)
	aliceGood, aliceFailed, bob := seedLogs(t, engine.Store())
	_, page := get(t, client, "/logs")

	resp, err := client.PostForm("http://ui/logs/clear", url.Values{"csrf": {csrfToken(t, page)}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if location := resp.Header.Get("Location"); !strings.Contains(location, "Nothing+was+ticked") {
		t.Errorf("clearing nothing ended at %q", location)
	}
	if jobs, _ := engine.Store().Jobs(0); len(jobs) != 4 {
		t.Fatalf("clearing nothing removed runs: %d left", len(jobs))
	}

	resp, err = client.PostForm("http://ui/logs/clear", url.Values{"csrf": {csrfToken(t, page)},
		"kind": {"backups", "system", "restores", "lifecycle"}, "tab": {"lifecycle"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	location := resp.Header.Get("Location")
	for _, want := range []string{"Cleared+1+backup%2C+1+account+event.", "Kept+3+rows", "tab=lifecycle"} {
		if !strings.Contains(location, want) {
			t.Errorf("the clearing ended at %q, without %q", location, want)
		}
	}
	jobs, _ := engine.Store().Jobs(0)
	left := map[string]bool{}
	for _, stored := range jobs {
		left[stored.ID] = true
	}
	if len(jobs) != 3 || !left[aliceGood.ID] || !left[aliceFailed.ID] || !left[bob.ID] {
		t.Errorf("left %v; want alice's last run, her last good copy, and bob's", left)
	}
	if events, _ := engine.Store().LifecycleEvents(0); len(events) != 0 {
		t.Errorf("events left: %d", len(events))
	}

	// Without the token nothing is cleared.
	resp, err = client.PostForm("http://ui/logs/clear", url.Values{"kind": {"backups"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Errorf("a clearing without the token = %d", resp.StatusCode)
	}
}

// TestTheServiceLogIsSearchedForWords: the service log keeps the lines
// that hold every word typed, in any case.
func TestTheServiceLogIsSearchedForWords(t *testing.T) {
	journal := "2026-09-17T02:00:01+0000 host gniza[1]: time=x level=INFO msg=\"backup finished\" account=alice status=success\n" +
		"2026-09-17T02:00:02+0000 host gniza[1]: time=x level=ERROR msg=\"backup target\" account=bob error=\"connection refused\"\n"
	client, _, _ := newUIWithJournal(t, journal)
	_, page := get(t, client, "/logs?tab=service&q=REFUSED+bob")
	box := page[strings.Index(page, `data-live="servicelog"`):]
	if !strings.Contains(box, "connection refused") || strings.Contains(box, "account=alice") {
		t.Errorf("the service log was not narrowed to the words: %.300s", box)
	}
	if !strings.Contains(page, `id="log_words" name="q" value="refused bob"`) {
		t.Error("the form does not keep the words")
	}
}
