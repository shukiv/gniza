package webui

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/shukiv/gniza/internal/job"
)

// namesListed is how many accounts a warning names before it stops and
// counts the rest. Enough to recognise the shape of what failed --
// whether it is one reseller or every account on the server -- without
// the line becoming the page.
const namesListed = 8

// varying is what changes between two accounts that failed for the same
// reason and nothing else: the byte counts in the message and the digits
// in a run's own temporary path. Grouping on the message as stored would
// put every account back on a line of its own, which is the thing being
// fixed.
//
// Long runs only. An exit status or an HTTP code is the difference
// between two failures rather than noise inside one, and collapsing
// those would file two causes under one reason and print whichever
// arrived first as if it were both.
var varying = regexp.MustCompile(`[0-9]{6,}`)

// failureWarnings says what failed and why, one line per reason rather
// than one line per account.
//
// The reason is on the job already. A page that said only "the last
// backup of X failed", twenty-five times over, made an operator open
// twenty-five accounts to find they shared one cause -- and the cause,
// on this server, was one line of code away in the log the whole time.
func failureWarnings(views []accountView) []string {
	type group struct {
		reason   string
		accounts []string
	}
	var order []string
	groups := map[string]*group{}
	for _, view := range views {
		if view.LastStatus != job.StatusFailed {
			continue
		}
		// The account's own name is in most of these messages, so it has
		// to come out before two accounts can be seen to share one.
		reason := view.LastError
		key := strings.ReplaceAll(reason, view.User, "")
		key = varying.ReplaceAllString(key, "")
		found, seen := groups[key]
		if !seen {
			found = &group{reason: reason}
			groups[key] = found
			order = append(order, key)
		}
		found.accounts = append(found.accounts, view.User)
	}

	// Widest first: the reason that stopped the most accounts is the one
	// to fix first, and the order the accounts happened to sort in says
	// nothing about that. Ties keep the order they were found in, which
	// is the order the page below lists them.
	sort.SliceStable(order, func(i, j int) bool {
		return len(groups[order[i]].accounts) > len(groups[order[j]].accounts)
	})
	warnings := make([]string, 0, len(order))
	for _, key := range order {
		found := groups[key]
		warnings = append(warnings, describeFailure(found.reason, found.accounts))
	}
	return warnings
}

// describeFailure writes the one line for a reason and the accounts that
// hit it. The reason is quoted as one account actually received it, so a
// message that names an account still reads truthfully even though the
// others got their own.
func describeFailure(reason string, accounts []string) string {
	listed := accounts
	rest := 0
	if len(listed) > namesListed {
		rest = len(listed) - namesListed
		listed = listed[:namesListed]
	}
	named := strings.Join(listed, ", ")
	if rest > 0 {
		named = fmt.Sprintf("%s and %d more", named, rest)
	}

	if len(accounts) == 1 {
		if reason == "" {
			return fmt.Sprintf("The last backup of %s failed, and did not say why.", named)
		}
		return fmt.Sprintf("The last backup of %s failed: %s", named, reason)
	}
	if reason == "" {
		return fmt.Sprintf("The last backup failed for %d accounts, none of which said why. Accounts: %s.",
			len(accounts), named)
	}
	return fmt.Sprintf(
		"The last backup failed for %d accounts with the same error, here as %s received it: %s. Accounts: %s.",
		len(accounts), accounts[0], strings.TrimRight(reason, "."), named)
}
