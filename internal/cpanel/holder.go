package cpanel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/shukiv/gniza/internal/panel"
)

// This file is cPanel's word on who its accounts are, for the
// account-facing socket: the kernel names the uid that opened it, and
// these say whether that is a customer in good standing and whether the
// host has turned the pages on for them.

const (
	// gnizaFeature is the Feature Manager feature the plugin registers.
	gnizaFeature        = "gniza"
	featureCheckTimeout = 5 * time.Second
	maxFeatureResponse  = 64 << 10
)

var (
	_ panel.AccountHolder   = (*Real)(nil)
	_ panel.SelfServiceGate = (*Real)(nil)
)

func (r *Real) whmapi() string {
	if r.WHMAPIPath != "" {
		return r.WHMAPIPath
	}
	return "/usr/local/cpanel/bin/whmapi1"
}

// plainName keeps a name that is about to be joined onto a path to
// something that cannot climb out of it.
func plainName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "/.")
}

// HoldsAccount reports whether cPanel has an account file for the name.
func (r *Real) HoldsAccount(name string) bool {
	return plainName(name) && r.accountRegistered(name)
}

// Suspended reports whether cPanel has this account suspended. cPanel
// records it two ways depending on version and on what suspended it, so
// both are checked: an account that is suspended by only one of them is
// still suspended.
func (r *Real) Suspended(name string) bool {
	if !plainName(name) {
		// Not a name this program will act on either way.
		return true
	}
	if _, err := os.Stat(filepath.Join(r.usersDir(), name)); err != nil {
		// The account file is what made this a cPanel account a moment
		// ago. If it cannot be read now, do not assume the account is in
		// good standing.
		return true
	}
	return r.suspended(name)
}

// Package is the PLAN line of the account file, or "".
func (r *Real) Package(name string) string {
	if !plainName(name) {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(r.usersDir(), name))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if value, found := strings.CutPrefix(strings.TrimSpace(line), "PLAN="); found {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// SelfServiceAllowed asks cPanel itself instead of reimplementing feature
// list inheritance and the server-wide disabled list. Arguments are
// passed without a shell, and the name has already come from cPanel's
// account registry by way of the socket's peer credentials.
func (r *Real) SelfServiceAllowed(ctx context.Context, account string) (bool, error) {
	if !plainName(account) {
		return false, fmt.Errorf("cpanel: %q is not an account name", account)
	}
	checkCtx, cancel := context.WithTimeout(ctx, featureCheckTimeout)
	defer cancel()
	output, err := exec.CommandContext(checkCtx, r.whmapi(), "--output=json",
		"verify_user_has_feature", "user="+account, "feature="+gnizaFeature).Output()
	if err != nil {
		return false, fmt.Errorf("cpanel: check feature access for %s: %w", account, err)
	}
	if len(output) > maxFeatureResponse {
		return false, fmt.Errorf("cpanel: check feature access for %s: response is too large", account)
	}
	return parseFeatureResponse(output)
}

func parseFeatureResponse(output []byte) (bool, error) {
	var response struct {
		Data struct {
			HasFeature int `json:"has_feature"`
		} `json:"data"`
		Metadata struct {
			Result int    `json:"result"`
			Reason string `json:"reason"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return false, fmt.Errorf("cpanel: decode feature response: %w", err)
	}
	if response.Metadata.Result != 1 {
		if response.Metadata.Reason == "" {
			response.Metadata.Reason = "cPanel did not accept the feature query"
		}
		return false, fmt.Errorf("cpanel: feature query failed: %s", response.Metadata.Reason)
	}
	return response.Data.HasFeature == 1, nil
}
