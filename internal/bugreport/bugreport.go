// Package bugreport turns what an operator saw into something a maintainer
// can act on.
//
// A bug report that says "the restore failed" costs a round trip to find
// out which restore, on what, running what version. So the report carries
// the answers: versions, the failures this server has recorded, the
// settings that shape behaviour, and the last lines the service logged.
//
// Nothing here leaves the server on its own. The report is built, shown
// and handed over as a file; the operator files it as an issue on the
// tracker at PublicReportURL, a page a person fills in. So the contents are
// chosen rather than swept up -- no credentials, repository passwords or
// tokens -- and log lines pass through a redactor first. That the operator
// carries the report themselves is what makes the redaction matter: they
// are the last check, and they see the whole thing before it goes.
package bugreport

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// PublicReportURL is where a bug report is filed: this project's issues,
// which anybody can open in a browser and which need nothing installed.
//
// It is a page a person fills in, not an API this program posts to. An
// endpoint that accepts reports with nobody behind them needs a
// credential, and there is no credential a plugin published to every
// cPanel server could hold without publishing it too. NewIssueURL fills
// that page in from what the operator typed.
const PublicReportURL = "https://github.com/shukiv/gniza/issues"

// Safe returns a separate copy suitable for both preview and download.
// User text is redacted too: the operator wrote it, but they may have
// pasted a log line into it.
func (r Report) Safe() Report {
	out := Report{Subject: Redact(r.Subject), Body: Redact(r.Body)}
	for _, section := range r.Sections {
		out.Sections = append(out.Sections, Section{
			Title: Redact(section.Title), Text: Clip(Redact(section.Text), MaxSectionBytes),
		})
	}
	return out
}

// Validate holds the report to what the public form accepts, so a report
// is refused here rather than after somebody has carried it there.
func (r Report) Validate() error {
	if strings.TrimSpace(r.Subject) == "" || strings.TrimSpace(r.Body) == "" {
		return fmt.Errorf("a report needs a subject and a description of what happened")
	}
	if !utf8.ValidString(r.Subject) || len(r.Subject) > 255 || strings.ContainsAny(r.Subject, "\r\n") {
		return fmt.Errorf("the subject must be one line of valid text, at most 255 bytes")
	}
	if !utf8.ValidString(r.Body) || len(r.Body) > 20000 {
		return fmt.Errorf("the description must be valid text, at most 20,000 bytes; shorten it before previewing")
	}
	if len(r.Sections) > 20 {
		return fmt.Errorf("the report has too many diagnostic sections (maximum 20)")
	}
	return nil
}

// Report is one bug report, before it becomes an issue.
type Report struct {
	Subject string
	Body    string
	// Sections are the gathered facts, in the order they are shown.
	Sections []Section
}

// Section is one heading and what is under it.
type Section struct {
	Title string
	Text  string
}

// MaxSectionBytes caps each section. A journal that has been shouting for
// a week must not turn one report into a megabyte nobody reads.
const MaxSectionBytes = 16 << 10

// Markdown renders the whole report: what the operator wrote, then each
// section under its heading.
func (r Report) Markdown() string {
	var out strings.Builder
	out.WriteString(strings.TrimSpace(r.Body))
	out.WriteString("\n")
	for _, section := range r.Sections {
		text := strings.TrimSpace(section.Text)
		if text == "" {
			continue
		}
		fmt.Fprintf(&out, "\n### %s\n\n```\n%s\n```\n", section.Title, text)
	}
	out.WriteString("\n---\n")
	fmt.Fprintf(&out, "Reported from Gniza on %s.\n",
		time.Now().UTC().Format("2006-01-02 15:04 UTC"))
	return out.String()
}

// secrets are the shapes a credential takes in a log line. Each keeps its
// name and loses its value: "password=hunter2" says more than "[redacted]"
// alone, and the name is what makes the line still readable.
var secrets = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(pass(word|_hash)?|secret|token|key|credential[s]?|auth)\s*[:=]\s*\S+`),
	regexp.MustCompile(`(?i)\b(AWS_SECRET_ACCESS_KEY|RESTIC_PASSWORD|B2_ACCOUNT_KEY)\S*\s*[:=]?\s*\S+`),
	// A bearer token or an https URL carrying a user and password.
	regexp.MustCompile(`(?i)\bBearer\s+\S+`),
	regexp.MustCompile(`(?i)\b[a-z0-9+.-]+://[^/\s:@]+:[^/\s@]+@`),
	// GitHub's own token shapes, in case one is pasted into a log line.
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{16,}`),
	// Anything that looks like a private key block.
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
}

// Redact removes what must not travel, keeping enough shape that the line
// still reads.
//
// It is a net rather than a proof: the sections this program builds carry
// no credentials by construction, and this is the second line of defence
// for the one section that is not built but copied -- the service log.
func Redact(text string) string {
	for _, pattern := range secrets {
		text = pattern.ReplaceAllStringFunc(text, func(match string) string {
			if name, _, found := strings.Cut(match, "="); found {
				return name + "=[removed]"
			}
			if name, _, found := strings.Cut(match, ":"); found &&
				!strings.Contains(name, "://") {
				return name + ": [removed]"
			}
			return "[removed]"
		})
	}
	return text
}

// Clip cuts a section to its cap, keeping the end: the last lines of a log
// are the ones nearest what went wrong.
func Clip(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	const marker = "[earlier lines left out]\n"
	keep := limit - len(marker)
	if keep <= 0 {
		return marker[:limit]
	}
	cut := text[len(text)-keep:]
	// Do not cut in the middle of a UTF-8 character. Include the marker in
	// the cap so preparing an already-reviewed section cannot clip it again.
	for len(cut) > 0 && !utf8.RuneStart(cut[0]) {
		cut = cut[1:]
	}
	if at := strings.IndexByte(cut, '\n'); at >= 0 && at < len(cut)-1 {
		cut = cut[at+1:]
	}
	return marker + cut
}
