package directadmin

import (
	"os"
	"path/filepath"

	"github.com/shukiv/gniza/internal/layout/dabackup"
	"github.com/shukiv/gniza/internal/panel"
)

// This file is DirectAdmin's word on who its accounts are, for the
// account-facing socket. DirectAdmin runs an account's plugin page as
// the account (ADR 0020), so the kernel names the customer; these say
// whether that name is an account here and in what standing. There is no
// Feature Manager on DirectAdmin, so whether the pages are turned on for
// an account is Gniza's own setting, which asks for the package.

var _ panel.AccountHolder = (*Real)(nil)

// HoldsAccount reports whether DirectAdmin has a user.conf for the name.
func (r *Real) HoldsAccount(name string) bool {
	if err := usableAccountName(name); err != nil {
		return false
	}
	info, err := os.Stat(filepath.Join(r.dataDir(), name, dabackup.UserConf))
	return err == nil && info.Mode().IsRegular()
}

// Suspended reports whether user.conf says suspended=yes. A record that
// cannot be read is reported suspended: it was an account a moment ago,
// and its standing is not known.
func (r *Real) Suspended(name string) bool {
	conf, err := r.userConf(name)
	if err != nil {
		return true
	}
	return conf["suspended"] == "yes"
}

// Package is the package= line of user.conf, or "".
func (r *Real) Package(name string) string {
	conf, err := r.userConf(name)
	if err != nil {
		return ""
	}
	return conf["package"]
}
