package directadmin

import (
	"os"
	"path/filepath"
	"testing"
)

// DirectAdmin's record of an account is its user.conf, and suspension is
// a line in it. A name with no record, or one that is not an account
// name at all, is not served.
func TestDirectAdminSaysWhoItsAccountsAreAndWhoIsSuspended(t *testing.T) {
	data := t.TempDir()
	write := func(name, body string) {
		if err := os.MkdirAll(filepath.Join(data, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(data, name, "user.conf"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("active", "username=active\npackage=silver\nsuspended=no\n")
	write("asleep", "username=asleep\npackage=silver\nsuspended=yes\n")
	if err := os.MkdirAll(filepath.Join(data, "hollow"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Real{DataDir: data}

	for name, want := range map[string]bool{"active": true, "asleep": true, "hollow": false, "vanished": false, "../root": false, "": false} {
		if got := r.HoldsAccount(name); got != want {
			t.Errorf("HoldsAccount(%q) = %v, want %v", name, got, want)
		}
	}
	for name, want := range map[string]bool{"active": false, "asleep": true, "hollow": true, "vanished": true, "../root": true} {
		if got := r.Suspended(name); got != want {
			t.Errorf("Suspended(%q) = %v, want %v", name, got, want)
		}
	}
	if got := r.Package("active"); got != "silver" {
		t.Errorf("Package(active) = %q, want silver", got)
	}
	if got := r.Package("vanished"); got != "" {
		t.Errorf("Package(vanished) = %q, want none", got)
	}
}
