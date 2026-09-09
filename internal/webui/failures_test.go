package webui

import (
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/panel"
)

func failedView(user, reason string) accountView {
	return accountView{
		AccountInfo: panel.AccountInfo{User: user},
		LastStatus:  job.StatusFailed,
		LastError:   reason,
	}
}

// A failure the operator cannot act on is a failure they will stop
// reading. The reason is already stored on the job; saying only that
// something failed throws it away.
func TestAFailureSaysWhyItFailed(t *testing.T) {
	warnings := failureWarnings([]accountView{
		failedView("toobian", "dabackup: archive filename does not belong to toobian"),
	})
	if len(warnings) != 1 {
		t.Fatalf("%d warnings, wanted one: %q", len(warnings), warnings)
	}
	for _, want := range []string{"toobian", "archive filename does not belong to"} {
		if !strings.Contains(warnings[0], want) {
			t.Errorf("the warning does not mention %q: %q", want, warnings[0])
		}
	}
}

// Twenty-five accounts that failed for one reason are one thing to fix,
// not twenty-five. Listed one by one they fill the page and the reason
// they share is nowhere on it.
func TestAccountsThatFailedForTheSameReasonAreOneWarning(t *testing.T) {
	var views []accountView
	for _, user := range []string{"admin", "coilco", "fix4u", "toobian"} {
		views = append(views, failedView(user,
			"dabackup: archive filename does not belong to "+user))
	}
	warnings := failureWarnings(views)
	if len(warnings) != 1 {
		t.Fatalf("%d warnings, wanted one: %q", len(warnings), warnings)
	}
	for _, want := range []string{"4", "admin", "coilco", "fix4u", "toobian",
		"archive filename does not belong to"} {
		if !strings.Contains(warnings[0], want) {
			t.Errorf("the warning does not mention %q: %q", want, warnings[0])
		}
	}
}

// The numbers in a staging failure are different every run and for every
// account, and grouping on them would put each account back on its own
// line -- which is the thing being fixed.
func TestFailuresThatDifferOnlyInTheirNumbersAreOneWarning(t *testing.T) {
	warnings := failureWarnings([]accountView{
		failedView("wallaishi", "directadmin: native staging needs 35811665584 bytes including reserve; "+
			"/var/lib/gniza-directadmin-native/wallaishi-851980776/output has 28904513536 available"),
		failedView("yeshuot", "directadmin: native staging needs 30960062953 bytes including reserve; "+
			"/var/lib/gniza-directadmin-native/yeshuot-2180157504/output has 28929249280 available"),
	})
	if len(warnings) != 1 {
		t.Fatalf("%d warnings, wanted one: %q", len(warnings), warnings)
	}
}

// Different reasons stay different: an operator fixing disk space should
// not have to read past the accounts that failed for another reason.
func TestDifferentReasonsStayApart(t *testing.T) {
	warnings := failureWarnings([]accountView{
		failedView("toobian", "dabackup: archive filename does not belong to toobian"),
		failedView("wallaishi", "directadmin: native staging needs 35811665584 bytes"),
	})
	if len(warnings) != 2 {
		t.Fatalf("%d warnings, wanted two: %q", len(warnings), warnings)
	}
}

// A very long list is not more useful than a short one and a count.
func TestALongListIsCutShort(t *testing.T) {
	var views []accountView
	for _, user := range []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "b1", "b2"} {
		views = append(views, failedView(user, "the destination refused the connection"))
	}
	warnings := failureWarnings(views)
	if len(warnings) != 1 {
		t.Fatalf("%d warnings, wanted one: %q", len(warnings), warnings)
	}
	if strings.Contains(warnings[0], "b2") {
		t.Errorf("every one of eleven accounts is listed: %q", warnings[0])
	}
	if !strings.Contains(warnings[0], "11") {
		t.Errorf("the warning does not say how many accounts failed: %q", warnings[0])
	}
}

// A job that failed before it had anything to say still has to be
// reported, or an account disappears from the page that exists to show
// what is wrong.
func TestAFailureWithNoStoredReasonIsStillReported(t *testing.T) {
	warnings := failureWarnings([]accountView{failedView("gananico", "")})
	if len(warnings) != 1 {
		t.Fatalf("%d warnings, wanted one: %q", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "gananico") {
		t.Errorf("the warning does not name the account: %q", warnings[0])
	}
}

// Accounts whose last backup worked are not warnings.
func TestAccountsThatSucceededAreNotWarnings(t *testing.T) {
	view := failedView("welove", "")
	view.LastStatus = job.StatusSuccess
	if warnings := failureWarnings([]accountView{view}); len(warnings) != 0 {
		t.Errorf("a successful account was reported as failed: %q", warnings)
	}
}

// An exit status or an HTTP code is the difference between two failures,
// not noise inside one. Collapsing them would put two causes under one
// reason and show whichever arrived first as if it were both.
func TestASmallNumberIsPartOfTheReason(t *testing.T) {
	warnings := failureWarnings([]accountView{
		failedView("one", "restic: exit status 3"),
		failedView("two", "restic: exit status 12"),
	})
	if len(warnings) != 2 {
		t.Fatalf("%d warnings, wanted two: %q", len(warnings), warnings)
	}
}

// The reason that stopped the most accounts is the one to fix first, so
// it goes at the top rather than wherever its accounts sort to.
func TestTheWidestFailureIsListedFirst(t *testing.T) {
	warnings := failureWarnings([]accountView{
		failedView("aardvark", "the destination refused the connection"),
		failedView("badger", "no room to stage"),
		failedView("civet", "no room to stage"),
	})
	if len(warnings) != 2 {
		t.Fatalf("%d warnings, wanted two: %q", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "no room to stage") {
		t.Errorf("the reason that stopped two accounts is not first: %q", warnings)
	}
}
