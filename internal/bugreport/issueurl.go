package bugreport

import (
	"net/url"
	"strings"
)

// MaxIssueURLBytes is how long a prefilled issue link is allowed to get.
// Browsers and servers each stop reading a URL somewhere, and the limits
// are neither published nor the same, so this is well under the lowest of
// them: a link that arrives whole matters more than one that carries every
// last line.
const MaxIssueURLBytes = 6000

// cutNotice is what replaces the part that did not fit. It is in the issue
// body rather than only on the page, because the person reading the issue
// is not the person who filed it.
const cutNotice = "\n\n_(cut short to fit in a link — the full report is the file attached to this issue.)_"

// NewIssueURL is the tracker's own new-issue form with the subject and the
// description already in it, so that filing a report is opening a tab and
// pressing the tracker's own button.
//
// It carries what the operator typed and nothing else. The diagnostics --
// versions, failures, settings, the log tail -- are the file they download
// and attach: they are too long for a URL, and they are also the part that
// deserves a look before it becomes public.
func NewIssueURL(subject, body string) string {
	subject, body = strings.TrimSpace(subject), strings.TrimSpace(body)
	if subject == "" && body == "" {
		return PublicReportURL
	}

	build := func(body string) string {
		query := url.Values{}
		if subject != "" {
			query.Set("title", subject)
		}
		if body != "" {
			query.Set("body", body)
		}
		return PublicReportURL + "/new?" + query.Encode()
	}

	issue := build(body)
	if len(issue) <= MaxIssueURLBytes {
		return issue
	}
	// Cut the description down until the whole link fits, on rune
	// boundaries so the notice does not follow half a character.
	for len(body) > 0 && len(build(body+cutNotice)) > MaxIssueURLBytes {
		body = body[:len(body)-1]
		for len(body) > 0 && !isRuneStart(body[len(body)-1]) {
			body = body[:len(body)-1]
		}
	}
	return build(strings.TrimSpace(body) + cutNotice)
}

// isRuneStart reports whether a byte can begin a UTF-8 rune, which is any
// byte that is not a continuation byte.
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
