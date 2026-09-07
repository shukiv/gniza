package bugreport_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/bugreport"
)

// TestTheIssueFormArrivesFilledIn is the point of the whole page: the
// operator types what went wrong here, and the tracker's own form opens
// with it already in the fields. Anything less is asking them to type it
// twice, which is how a bug report stops being written at all.
func TestTheIssueFormArrivesFilledIn(t *testing.T) {
	subject := "Restore failed on studio"
	body := "It said exit status 2 and stopped."

	issue, err := url.Parse(bugreport.NewIssueURL(subject, body))
	if err != nil {
		t.Fatalf("the issue URL does not parse: %v", err)
	}
	if !strings.HasPrefix(bugreport.NewIssueURL(subject, body), bugreport.PublicReportURL) {
		t.Errorf("the issue URL is not on the tracker: %s", issue)
	}
	if got := issue.Query().Get("title"); got != subject {
		t.Errorf("title = %q, want %q", got, subject)
	}
	if got := issue.Query().Get("body"); !strings.Contains(got, body) {
		t.Errorf("body = %q, does not carry what was typed", got)
	}
}

// TestAReportTooLongForAURLIsCutRatherThanLost. A URL is not a transport
// for a whole diagnostic report -- servers and browsers both stop reading
// one somewhere. Cut it here, on purpose, and say so in the text, rather
// than hand over a link that silently arrives truncated or not at all.
func TestAReportTooLongForAURLIsCutRatherThanLost(t *testing.T) {
	long := strings.Repeat("the log said something. ", 2000)

	issue := bugreport.NewIssueURL("Backups fail every night", long)
	if len(issue) > bugreport.MaxIssueURLBytes {
		t.Fatalf("the issue URL is %d bytes, over the %d cap", len(issue), bugreport.MaxIssueURLBytes)
	}
	parsed, err := url.Parse(issue)
	if err != nil {
		t.Fatalf("the issue URL does not parse: %v", err)
	}
	if body := parsed.Query().Get("body"); !strings.Contains(body, "cut short") {
		t.Errorf("the body was cut but does not say so: %q", body)
	}
}

// TestAnEmptyFormStillOpensTheTracker. The button is on the page before
// anything is typed, and it has to go somewhere sensible then too.
func TestAnEmptyFormStillOpensTheTracker(t *testing.T) {
	if got := bugreport.NewIssueURL("", ""); !strings.HasPrefix(got, bugreport.PublicReportURL) {
		t.Errorf("an empty report links to %q", got)
	}
}

// TestTheIssueCarriesTheDiagnostics. Filing a bug is one action, so what
// the server knows has to travel with what the operator typed. A form that
// arrives holding only the description asks the maintainer the same three
// questions the report exists to answer.
func TestTheIssueCarriesTheDiagnostics(t *testing.T) {
	report := bugreport.Report{
		Subject: "Backups fail every night",
		Body:    "Every account fails at 02:00.",
		Sections: []bugreport.Section{
			{Title: "Versions and environment", Text: "gniza v0.2.5\nhostname cp01.example.com"},
			{Title: "Service log", Text: strings.Repeat("an old line\n", 4000) + "the newest line"},
		},
	}

	issue := report.IssueURL()
	if len(issue) > bugreport.MaxIssueURLBytes {
		t.Fatalf("the issue URL is %d bytes, over the %d cap", len(issue), bugreport.MaxIssueURLBytes)
	}
	parsed, err := url.Parse(issue)
	if err != nil {
		t.Fatalf("the issue URL does not parse: %v", err)
	}
	body := parsed.Query().Get("body")

	for _, want := range []string{"Every account fails at 02:00.", "cp01.example.com"} {
		if !strings.Contains(body, want) {
			t.Errorf("the issue does not carry %q", want)
		}
	}
	// A log is cut from its front, because the lines that matter are the
	// ones nearest the failure. The newest line survives; most of the
	// 4000 older ones do not.
	if !strings.Contains(body, "the newest line") {
		t.Error("the log was cut from the wrong end: the newest line is gone")
	}
	if kept := strings.Count(body, "an old line"); kept >= 4000 {
		t.Errorf("the log was not cut down at all: %d lines of it", kept)
	}
}
