package webui_test

import (
	"os/user"
	"strconv"
	"testing"

	"github.com/shukiv/gniza/internal/granular"
	"github.com/shukiv/gniza/internal/webui"
)

// TestAnAccountCannotAskForTheThingsThePageDoesNotOffer covers a gap
// between what the customer page shows and what its form handler accepted.
// A full account is an explicit safe-download flow; the raw panel-settings
// subset is not, because it exposes shadow, digestshadow and other metadata
// without the context of a cPanel account archive.
func TestAnAccountCannotAskForTheThingsThePageDoesNotOffer(t *testing.T) {
	for _, kind := range []string{
		string(granular.KindFiles), string(granular.KindDatabase),
		string(granular.KindDNS), "account",
	} {
		if !webui.IsUserKindForTest(kind) {
			t.Errorf("%s is on the customer page but the handler refuses it", kind)
		}
	}
	for _, kind := range []string{string(granular.KindSettings), "", "../settings"} {
		if webui.IsUserKindForTest(kind) {
			t.Errorf("an account was allowed to ask for %q", kind)
		}
	}
}

// TestASuspendedAccountIsNotServed covers both marks cPanel uses for a
// suspension. A suspended account's processes can still be running, and
// this interface talks to a service running as root.
// fakeHolder is a panel's word on its accounts, for a test.
type fakeHolder struct {
	accounts  map[string]bool
	suspended map[string]bool
}

func (f fakeHolder) HoldsAccount(name string) bool { return f.accounts[name] }
func (f fakeHolder) Suspended(name string) bool    { return f.suspended[name] }
func (f fakeHolder) Package(string) string         { return "" }

// The kernel names the uid; the panel says whether it is a customer in
// good standing. Root, a uid the panel does not hold, and a suspended
// account are all refused, and a panel with no word at all serves nobody.
func TestTheAccountSocketServesOnlyThePanelsAccountsInGoodStanding(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	uid64, err := strconv.ParseUint(me.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	uid := uint32(uid64)
	if uid == 0 {
		t.Skip("running as root; the check under test refuses root before it asks the panel")
	}

	if _, err := webui.AccountFromCredentialsForTest(fakeHolder{accounts: map[string]bool{me.Username: true}}, 0); err == nil {
		t.Error("root was served as an account")
	}
	if _, err := webui.AccountFromCredentialsForTest(nil, uid); err == nil {
		t.Error("a panel with no account-facing interface served somebody")
	}
	if _, err := webui.AccountFromCredentialsForTest(fakeHolder{}, uid); err == nil {
		t.Error("a uid the panel does not hold was served")
	}
	if _, err := webui.AccountFromCredentialsForTest(fakeHolder{
		accounts: map[string]bool{me.Username: true}, suspended: map[string]bool{me.Username: true},
	}, uid); err == nil {
		t.Error("a suspended account was served")
	}
	got, err := webui.AccountFromCredentialsForTest(fakeHolder{accounts: map[string]bool{me.Username: true}}, uid)
	if err != nil || got != me.Username {
		t.Errorf("account = %q, %v; want %q", got, err, me.Username)
	}
}

func TestSocketConnectionsAreBoundedBeforeHTTPParsing(t *testing.T) {
	budget := webui.NewConnectionBudgetForTest(3, 2)
	if !budget.Acquire(1001) || !budget.Acquire(1001) {
		t.Fatal("the first two connections from an account were refused")
	}
	if budget.Acquire(1001) {
		t.Fatal("one account exceeded its pre-HTTP connection budget")
	}
	if !budget.Acquire(1002) {
		t.Fatal("a second account could not use the remaining global slot")
	}
	if budget.Acquire(1003) {
		t.Fatal("the global pre-HTTP connection budget was exceeded")
	}
	budget.Release(1001)
	if !budget.Acquire(1003) {
		t.Fatal("a closed connection did not release its slot")
	}
}
