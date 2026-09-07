package bugreport

import (
	"net/url"
	"strings"
)

// MaxIssueURLBytes is how long a prefilled issue link is allowed to get.
// Measured against the tracker rather than guessed: it answers 414 above
// about 8 KiB and starts failing before that, while a link of this length
// is served. A link that arrives whole matters more than one that carries
// every last line, so this stays well under where the trouble starts.
const MaxIssueURLBytes = 6000

// cutNotice is what replaces the part that did not fit. It is in the issue
// body rather than only on the page, because the person reading the issue
// is not the person who filed it.
const cutNotice = "\n\n_(cut short to fit in a link — the full report is the file attached to this issue.)_"

// IssueURL is the tracker's new-issue form holding the whole report --
// what the operator typed and what this server knows -- so that filing a
// bug is opening a tab and pressing the tracker's own button.
//
// A URL cannot hold every line of a service log, so the report is cut down
// until the link fits: the longest section goes first and loses its oldest
// half, because the lines worth reading are the ones nearest the failure.
// The download is still the whole of it, for attaching.
func (r Report) IssueURL() string {
	trimmed := r
	trimmed.Sections = append([]Section(nil), r.Sections...)
	for !trimmed.fits() && trimmed.shrink() {
	}
	return NewIssueURL(trimmed.Subject, trimmed.Markdown())
}

// fits reports whether the whole report goes into a link untrimmed. It
// measures the uncapped link, because capping is what NewIssueURL does and
// asking it would always answer yes.
func (r Report) fits() bool {
	return len(buildIssue(r.Subject, r.Markdown())) <= MaxIssueURLBytes
}

// shrink cuts the longest section down to the most of its newest lines
// that still leave the link whole, and drops it if not one line does. It
// reports whether there was anything left to give.
//
// The size is searched for rather than halved: halving lands wherever the
// halves happen to fall and throws away lines there was room for, which on
// a service log is the difference between a dozen lines and sixty.
func (r *Report) shrink() bool {
	longest, size := -1, 0
	for i, section := range r.Sections {
		if len(section.Text) > size {
			longest, size = i, len(section.Text)
		}
	}
	if longest < 0 {
		return false
	}

	full := r.Sections[longest].Text
	low, high := 0, size
	for low < high {
		try := (low + high + 1) / 2
		r.Sections[longest].Text = Clip(full, try)
		if r.fits() {
			low = try
		} else {
			high = try - 1
		}
	}
	if low == 0 {
		r.Sections = append(r.Sections[:longest:longest], r.Sections[longest+1:]...)
		return true
	}
	r.Sections[longest].Text = Clip(full, low)
	return true
}

// NewIssueURL is that form with a subject and a body of the caller's
// choosing. IssueURL is what the interface uses; this is the piece under
// it, and it caps the link rather than trimming the report.
func NewIssueURL(subject, body string) string {
	subject, body = strings.TrimSpace(subject), strings.TrimSpace(body)
	if subject == "" && body == "" {
		return PublicReportURL
	}

	issue := buildIssue(subject, body)
	if len(issue) <= MaxIssueURLBytes {
		return issue
	}
	// Cut the description down until the whole link fits, on rune
	// boundaries so the notice does not follow half a character.
	for len(body) > 0 && len(buildIssue(subject, body+cutNotice)) > MaxIssueURLBytes {
		body = body[:len(body)-1]
		for len(body) > 0 && !isRuneStart(body[len(body)-1]) {
			body = body[:len(body)-1]
		}
	}
	return buildIssue(subject, strings.TrimSpace(body)+cutNotice)
}

// buildIssue is the link with no cap applied, which is what tells the
// caller whether there is too much to carry.
func buildIssue(subject, body string) string {
	query := url.Values{}
	if subject != "" {
		query.Set("title", subject)
	}
	if body != "" {
		query.Set("body", body)
	}
	return PublicReportURL + "/new?" + query.Encode()
}

// isRuneStart reports whether a byte can begin a UTF-8 rune, which is any
// byte that is not a continuation byte.
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
