// Package cpanel implements the panel interface for cPanel/WHM: it stages
// the files an account's backup consists of, and hands a rebuilt one back
// to pkgacct's counterpart, restorepkg.
//
// The interface itself lives in internal/panel, because a second panel is
// meant to be another implementation of it rather than a fork of the
// program. The names below are aliases so that the two hundred call sites
// that say cpanel.Provider today keep compiling while they move; new code
// should say panel.Provider.
package cpanel

import "github.com/shukiv/gniza/internal/panel"

type (
	AccountInfo   = panel.AccountInfo
	ApplyOptions  = panel.ApplyOptions
	Certifier     = panel.Certifier
	DatabaseGrant = panel.DatabaseGrant
	DatabaseUser  = panel.DatabaseUser
	Provider      = panel.Provider
	StageRequest  = panel.StageRequest
)
