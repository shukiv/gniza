package webui

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
)

// How many rows a tab of the Logs page draws. The counts on the tabs say
// how many there are; a search narrows them, which is how an older row is
// reached.
const (
	logRunsShown   = 200
	logOthersShown = 100
)

func firstRuns(runs []nodestore.Job, limit int) []nodestore.Job {
	if len(runs) > limit {
		return runs[:limit]
	}
	return runs
}

// logSearch is what the history is searched for: words, every one of
// which a row has to hold somewhere, in any case. No words matches
// everything.
type logSearch []string

// newLogSearch reads what was typed. It is cut short: it is matched
// against every row of the history on every read of the page.
func newLogSearch(typed string) logSearch {
	if len(typed) > 200 {
		typed = typed[:200]
	}
	return logSearch(strings.Fields(strings.ToLower(typed)))
}

func (q logSearch) String() string { return strings.Join(q, " ") }

// matches says every word is somewhere in what a row says.
func (q logSearch) matches(said ...string) bool {
	if len(q) == 0 {
		return true
	}
	text := strings.ToLower(strings.Join(said, "\n"))
	for _, word := range q {
		if !strings.Contains(text, word) {
			return false
		}
	}
	return true
}

// pageStamp is a moment as the pages write it, so that what is read on
// a row can be searched for.
func pageStamp(run nodestore.Job) string { return run.QueuedAt.Local().Format("2006-01-02 15:04") }

func (q logSearch) matchesJob(run nodestore.Job, destinations, schedules map[string]string) bool {
	if len(q) == 0 {
		return true
	}
	said := []string{run.ID, run.Account, string(run.Status), run.PolicyName, schedules[run.PolicyID],
		pageStamp(run), run.StagingErr}
	said = append(said, run.Missing...)
	said = append(said, run.Warnings...)
	if run.Staged != nil {
		said = append(said, stagedWords(run)...)
		said = append(said, run.Staged.Paths...)
		said = append(said, run.Staged.Databases...)
	}
	for _, target := range run.Targets {
		said = append(said, destinations[target.RepositoryID], target.SnapshotID, string(target.Status), target.Error)
		if target.Incomplete {
			said = append(said, "some files unreadable")
		}
	}
	return q.matches(said...)
}

func (q logSearch) matchesRestore(restore nodestore.Restore, destinations map[string]string) bool {
	if len(q) == 0 {
		return true
	}
	return q.matches(restore.ID, restore.Account, string(restore.Status), restore.Kind, restoreRow{Restore: restore}.Parts(),
		restore.SnapshotID, destinations[restore.RepositoryID], restore.Error, restore.Detail,
		restore.RestoredTo, restore.ArchivePath, restore.QueuedAt.Local().Format("2006-01-02 15:04"))
}

func (q logSearch) matchesEvent(event nodestore.LifecycleEvent) bool {
	if len(q) == 0 {
		return true
	}
	return q.matches(event.Title(), event.Account, event.Outcome(), event.Detail,
		event.At.Local().Format("2006-01-02 15:04"))
}

// stagedPart is one kind of part in the words a person would use.
func stagedPart(kind string) string {
	switch kind {
	case "homedir":
		return "files"
	case "metadata":
		return "settings"
	case "database":
		return "databases"
	case "archive":
		return "the whole account as one archive"
	case "system":
		return "the server's settings"
	}
	return kind
}

// skippedPart is what a schedule left out, in the same words.
func skippedPart(kind string) string {
	switch kind {
	case "homedir":
		return "files"
	case "email":
		return "mail"
	}
	return kind
}

// stagedWords is what a run backed up, short enough for a table cell:
// the parts, and how many databases.
func stagedWords(run nodestore.Job) []string {
	if run.Staged == nil {
		return nil
	}
	var words []string
	counted := false
	for _, kind := range run.Staged.Parts {
		word := stagedPart(kind)
		if kind == "database" && len(run.Staged.Databases) > 0 {
			word, counted = databasesCounted(len(run.Staged.Databases)), true
		}
		words = append(words, word)
	}
	if !counted && len(run.Staged.Databases) > 0 {
		words = append(words, databasesCounted(len(run.Staged.Databases)))
	}
	return words
}

func databasesCounted(n int) string {
	if n == 1 {
		return "1 database"
	}
	return fmt.Sprintf("%d databases", n)
}

// handleClearLogs empties the parts of the history that were ticked.
// What the overview and termination protection still read is kept, and
// the page says so before and after (node.ClearLogs).
func (s *Server) handleClearLogs(w http.ResponseWriter, r *http.Request) {
	back := "/logs"
	if tab := logTab(r.PostFormValue("tab")); tab != "backups" {
		back += "?tab=" + tab
	}
	var kinds node.LogKinds
	for _, kind := range r.PostForm["kind"] {
		switch kind {
		case "backups":
			kinds.Backups = true
		case "system":
			kinds.System = true
		case "restores":
			kinds.Restores = true
		case "lifecycle":
			kinds.Lifecycle = true
		}
	}
	if kinds == (node.LogKinds{}) {
		s.redirect(w, r, back, "error", "Nothing was ticked, so nothing was cleared.")
		return
	}
	cleared, err := s.engine.ClearLogs(kinds)
	if err != nil {
		s.redirect(w, r, back, "error", "The logs could not be cleared: "+err.Error())
		return
	}
	var went []string
	for _, part := range []struct {
		n    int
		what string
	}{
		{cleared.Backups, "backup"}, {cleared.System, "system backup"},
		{cleared.Restores, "restore"}, {cleared.Lifecycle, "account event"},
	} {
		if part.n > 0 {
			went = append(went, counted(part.n, part.what))
		}
	}
	message := "There was nothing to clear."
	if len(went) > 0 {
		message = "Cleared " + strings.Join(went, ", ") + "."
	}
	if kept := cleared.KeptRuns + cleared.KeptRestores; kept > 0 {
		message += fmt.Sprintf(" Kept %s: the last run and the last good copy of each account, the last "+
			"rehearsal and any download still waiting, which the overview and termination protection read.",
			counted(kept, "row"))
	}
	s.redirect(w, r, back, "ok", message)
}

// countedNumber writes a count with its thousands apart, which is how a
// number of files is read.
func countedNumber(n uint64) string {
	digits := fmt.Sprintf("%d", n)
	var out []byte
	for i, digit := range []byte(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, digit)
	}
	return string(out)
}
