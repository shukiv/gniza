package cpanel

import (
	"os"
	"path/filepath"
	"testing"
)

// The account-facing socket serves whoever the kernel says opened it, so
// the panel's word on that name is the whole of the admission check: not
// an account, or an account that is suspended, is not served.
func TestCPanelSaysWhoItsAccountsAreAndWhoIsSuspended(t *testing.T) {
	root := t.TempDir()
	usersDir := filepath.Join(root, "users")
	suspendedDir := filepath.Join(root, "suspended")
	for _, dir := range []string{usersDir, suspendedDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(usersDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("active", "USER=active\nPLAN=gold\n")
	write("byflag", "USER=byflag\nSUSPENDED=1\n")
	write("byfile", "USER=byfile\n")
	if err := os.WriteFile(filepath.Join(suspendedDir, "byfile"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Real{UsersDir: usersDir, SuspendedDir: suspendedDir}

	for name, want := range map[string]bool{"active": true, "byflag": true, "byfile": true, "vanished": false, "../root": false, "": false} {
		if got := r.HoldsAccount(name); got != want {
			t.Errorf("HoldsAccount(%q) = %v, want %v", name, got, want)
		}
	}
	for name, want := range map[string]bool{
		"active": false, "byflag": true, "byfile": true,
		// No account file at all, and a name that tries to climb out of
		// the directory: neither is an account in good standing.
		"vanished": true, "../root": true,
	} {
		if got := r.Suspended(name); got != want {
			t.Errorf("Suspended(%q) = %v, want %v", name, got, want)
		}
	}
	if got := r.Package("active"); got != "gold" {
		t.Errorf("Package(active) = %q, want gold", got)
	}
	if got := r.Package("byfile"); got != "" {
		t.Errorf("Package(byfile) = %q, want none", got)
	}
}

func TestFeatureResponseMustExplicitlyAllowTheAccount(t *testing.T) {
	for name, response := range map[string]string{
		"enabled":  `{"data":{"has_feature":1},"metadata":{"result":1,"reason":"OK"}}`,
		"disabled": `{"data":{"has_feature":0},"metadata":{"result":1,"reason":"OK"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			allowed, err := parseFeatureResponse([]byte(response))
			if err != nil {
				t.Fatalf("parseFeatureResponse: %v", err)
			}
			if allowed != (name == "enabled") {
				t.Fatalf("allowed = %v", allowed)
			}
		})
	}
	for _, response := range []string{
		`not json`,
		`{"data":{"has_feature":1},"metadata":{"result":0,"reason":"bad query"}}`,
		`{"data":{"has_feature":1}}`,
	} {
		if allowed, err := parseFeatureResponse([]byte(response)); err == nil || allowed {
			t.Fatalf("unsafe feature response accepted: %s", response)
		}
	}
}

// The question goes to whmapi1 with the account as an argument, never
// through a shell, and the answer is read as a whole.
func TestSelfServiceAsksWHMAPIForTheAccount(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "args")
	script := filepath.Join(dir, "whmapi1")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuoteForTest(record) + "\n" +
		`printf '{"data":{"has_feature":1},"metadata":{"result":1,"reason":"OK"}}'` + "\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	r := &Real{WHMAPIPath: script}
	allowed, err := r.SelfServiceAllowed(t.Context(), "studio")
	if err != nil || !allowed {
		t.Fatalf("allowed = %v, err = %v", allowed, err)
	}
	args, _ := os.ReadFile(record)
	if string(args) != "--output=json\nverify_user_has_feature\nuser=studio\nfeature=gniza\n" {
		t.Errorf("whmapi1 was asked:\n%s", args)
	}
	if _, err := r.SelfServiceAllowed(t.Context(), "../root"); err == nil {
		t.Error("a name that is not an account name was asked about")
	}
}
