package webui

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/shukiv/gniza/internal/bugreport"
)

// journalReader is how the service log is read. It is a field on the server
// so a test can answer with lines of its own: every other route can be
// exercised without a journal, and this one could not.
type journalReader func(ctx context.Context, args ...string) ([]byte, error)

func readJournal(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// CombinedOutput: journalctl explains a refusal on stderr, and an
	// operator reading an empty box deserves to be told why it is empty.
	return exec.CommandContext(ctx, "journalctl", args...).CombinedOutput()
}

// serviceLogBytes caps what one page carries. The journal on a busy server
// is measured in megabytes, and a page nobody can scroll is not a page.
const serviceLogBytes = 512 << 10

// logSpans are the stretches of history offered, and the argument each one
// becomes. The argument is looked up here rather than built from what
// arrived, so nothing from a query string reaches journalctl's command line.
var logSpans = []struct{ Key, Label, Since string }{
	{"hour", "Last hour", "-1h"},
	{"today", "Today", "today"},
	{"week", "Last 7 days", "-7d"},
	{"all", "Everything kept", ""},
}

// logSizes are how many lines to read, by the same rule.
var logSizes = []struct{ Key, Label, Lines string }{
	{"200", "200 lines", "200"},
	{"1000", "1,000 lines", "1000"},
	{"5000", "5,000 lines", "5000"},
	{"all", "All of it", "all"},
}

// serviceLogView is the service log tab.
type serviceLogView struct {
	// Query is text a line has to hold to be kept, every word of it.
	Query   string
	Text    string
	Level   string
	Account string
	Span    string
	Size    string
	Follow  bool
	Spans   []logSpan
	Sizes   []logSpan
	Levels  []string
	Error   string
	Empty   bool
}

// logSpan is one choice in a picker: what it is called and whether it is
// the one in force.
type logSpan struct {
	Key, Label string
	Chosen     bool
}

// serviceLog reads the journal and narrows it to what was asked for.
//
// Nothing here is redacted. This page is behind the root-only socket, and
// an operator who can open it can run journalctl themselves; taking the
// credentials out of a log they are reading to debug a credential would
// hide the answer. A bug report is the other way round -- it leaves the
// server, so bugreport.Redact runs over it before anybody sees it.
func (s *Server) serviceLog(ctx context.Context, r *http.Request) serviceLogView {
	view := serviceLogView{
		Level:   logLevelAsked(r.URL.Query().Get("level")),
		Account: strings.TrimSpace(r.URL.Query().Get("account")),
		Query:   newLogSearch(r.URL.Query().Get("q")).String(),
		Follow:  r.URL.Query().Get("follow") == "1",
		Levels:  []string{"debug", "info", "warn", "error"},
	}
	span := chooseSpan(r.URL.Query().Get("span"))
	size := chooseSize(r.URL.Query().Get("size"))
	// Following re-reads every few seconds. Reading the whole journal that
	// often is a process, a filter and half a megabyte of markup on every
	// tick, for a box that only ever shows its last lines. Whoever wants
	// all of it wants the file.
	if view.Follow && size == "all" {
		size = "1000"
	}
	view.Span, view.Size = span, size
	for _, choice := range logSpans {
		view.Spans = append(view.Spans, logSpan{choice.Key, choice.Label, choice.Key == span})
	}
	for _, choice := range logSizes {
		view.Sizes = append(view.Sizes, logSpan{choice.Key, choice.Label, choice.Key == size})
	}

	args := []string{"-u", "gniza", "--no-pager", "-o", "short-iso", "-n", sizeLines(size)}
	if since := spanSince(span); since != "" {
		args = append(args, "--since", since)
	}
	raw, err := s.journal(ctx, args...)
	if err != nil && len(raw) == 0 {
		view.Error = "the service log could not be read: " + err.Error()
		return view
	}
	text := narrowLog(string(raw), view.Level, view.Account, view.Query)
	view.Empty = strings.TrimSpace(text) == ""
	view.Text = bugreport.Clip(text, serviceLogBytes)
	return view
}

// narrowLog keeps the lines an operator asked for.
//
// The level is read out of the line rather than from journald's own
// priority: the service writes plain text to stderr, so every line reaches
// the journal at the same priority and -p would filter nothing. A line with
// no level on it -- restic's own output, a panic -- is always kept, because
// a line nobody can classify is exactly the one worth seeing.
func narrowLog(text, level, account, query string) string {
	want, err := parseLevel(level)
	if err != nil {
		want = slog.LevelDebug
	}
	search := newLogSearch(query)
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		if account != "" && !namesAccount(line, account) {
			continue
		}
		if !search.matches(line) {
			continue
		}
		if at, found := lineLevel(line); found && at < want {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// namesAccount is true when the line is about that account and not about
// one whose name merely starts the same way: account=studio must not keep
// account=studio2.
func namesAccount(line, account string) bool {
	field := "account=" + account
	for at := 0; ; {
		found := strings.Index(line[at:], field)
		if found < 0 {
			return false
		}
		end := at + found + len(field)
		if end == len(line) || line[end] == ' ' || line[end] == '\t' {
			return true
		}
		at = end
	}
}

func lineLevel(line string) (slog.Level, bool) {
	at := strings.Index(line, "level=")
	if at < 0 {
		return 0, false
	}
	name := line[at+len("level="):]
	if end := strings.IndexAny(name, " \t"); end >= 0 {
		name = name[:end]
	}
	level, err := parseLevel(name)
	return level, err == nil
}

func parseLevel(name string) (slog.Level, error) {
	var level slog.Level
	err := level.UnmarshalText([]byte(strings.TrimSpace(name)))
	return level, err
}

func logLevelAsked(asked string) string {
	if _, err := parseLevel(asked); err != nil {
		return "debug"
	}
	return strings.ToLower(strings.TrimSpace(asked))
}

func chooseSpan(asked string) string {
	for _, choice := range logSpans {
		if choice.Key == asked {
			return asked
		}
	}
	return "today"
}

func chooseSize(asked string) string {
	for _, choice := range logSizes {
		if choice.Key == asked {
			return asked
		}
	}
	return "1000"
}

func spanSince(key string) string {
	for _, choice := range logSpans {
		if choice.Key == key {
			return choice.Since
		}
	}
	return ""
}

func sizeLines(key string) string {
	for _, choice := range logSizes {
		if choice.Key == key {
			return choice.Lines
		}
	}
	return "1000"
}

// handleLogDownload hands over the log as a file, unclipped and unfiltered
// by the page's line cap. The page shows a tail; this is for the whole of
// what the journal still keeps.
func (s *Server) handleLogDownload(w http.ResponseWriter, r *http.Request) {
	span := chooseSpan(r.URL.Query().Get("span"))
	args := []string{"-u", "gniza", "--no-pager", "-o", "short-iso", "-n", "all"}
	if since := spanSince(span); since != "" {
		args = append(args, "--since", since)
	}
	raw, err := s.journal(r.Context(), args...)
	if err != nil && len(raw) == 0 {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	text := narrowLog(string(raw), logLevelAsked(r.URL.Query().Get("level")),
		strings.TrimSpace(r.URL.Query().Get("account")), r.URL.Query().Get("q"))
	name := fmt.Sprintf("gniza-log-%s.txt", time.Now().UTC().Format("20060102-1504"))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	fmt.Fprintln(w, text)
}
