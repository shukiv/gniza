// Package pkgacct plans the payload an agent stages for one cPanel account.
//
// The payload strategy decides how well restic can deduplicate, and is the
// most consequential choice in the system: a compressed pkgacct archive has
// no stable chunk boundaries between runs, so restic stores close to the
// full archive every night, in every destination. See docs/DESIGN.md §4.
package pkgacct

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mode selects how an account is decomposed into a payload.
type Mode string

const (
	// ModeSplit backs up account metadata, the home directory and each
	// database separately, so restic sees real files and deduplicates
	// normally. This is the default.
	ModeSplit Mode = "split"
	// ModeMonolithic backs up one uncompressed pkgacct archive. It keeps
	// cPanel's own archive layout intact at a large storage cost, and is
	// only sound when pkgacct compression can actually be disabled.
	ModeMonolithic Mode = "monolithic"
	// ModeSystem backs up the server's own configuration rather than an
	// account: what a replacement machine has to be told before the
	// accounts on it mean anything.
	ModeSystem Mode = "system"
)

// PartKind labels a component of a staged payload.
type PartKind string

const (
	PartMetadata PartKind = "metadata"
	PartHomedir  PartKind = "homedir"
	PartDatabase PartKind = "database"
	PartArchive  PartKind = "archive"
	PartSystem   PartKind = "system"
)

// Part is one staged path handed to restic.
type Part struct {
	Kind PartKind
	Path string
}

// Payload is everything restic backs up for one account in one job.
type Payload struct {
	Mode    Mode
	Account string
	Parts   []Part
	// DumpPaths is where each database dump is written. They live inside
	// the database part's directory, which is what restic is pointed at:
	// naming the directory rather than each file keeps a snapshot's paths
	// identical when an account gains or loses a database, and paths that
	// change would put every run in its own retention group.
	DumpPaths map[string]string
	// Degraded is set when the payload had to be built in a way that
	// deduplicates poorly, with Reason explaining why. The controller
	// surfaces this rather than letting storage cost silently balloon.
	Degraded bool
	Reason   string
	// Missing is what could not be put in this payload after all. One
	// corrupt table used to cost an account its whole backup -- home
	// directory and forty-six healthy databases with it -- because
	// staging returned on the first failed dump. It carries on now, and
	// what it could not take is written down here: a backup with a hole
	// in it is worth having, and worth being told about, but it is not a
	// complete account and must never be counted as one.
	Missing []Omission
	// Warnings is what this payload holds but a restore of it may not be
	// able to put back. Nothing is left out for one, and the account is
	// still complete: it is what somebody has to know before the night
	// they need the backup, not on it.
	Warnings []string
}

// Omission is one thing a payload does not contain, and why.
type Omission struct {
	What string
	Why  string
}

// String renders an omission for an operator reading a job.
func (o Omission) String() string {
	if o.Why == "" {
		return o.What
	}
	return o.What + ": " + o.Why
}

// Verify checks that every part a payload promised is actually on disk.
//
// This exists because of a real failure: pkgacct wrote nothing where it was
// told to, restic warned that it could not read the path and carried on,
// and the result was a snapshot holding the home directory and none of the
// account's configuration — reported as a success. A backup missing a part
// is not a backup, so it has to fail here rather than later.
func (p Payload) Verify() error {
	var missing []string
	for _, part := range p.Parts {
		info, err := os.Stat(part.Path)
		switch {
		case err != nil:
			missing = append(missing, fmt.Sprintf("%s (%s) is missing", part.Kind, part.Path))
		case info.IsDir():
			entries, err := os.ReadDir(part.Path)
			if err != nil || len(entries) == 0 {
				missing = append(missing, fmt.Sprintf("%s (%s) is empty", part.Kind, part.Path))
			}
		case info.Size() == 0:
			missing = append(missing, fmt.Sprintf("%s (%s) is empty", part.Kind, part.Path))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("pkgacct: the staged payload is incomplete: %s",
			strings.Join(missing, "; "))
	}
	return nil
}

// Paths returns the staged paths in payload order.
func (p Payload) Paths() []string {
	paths := make([]string, 0, len(p.Parts))
	for _, part := range p.Parts {
		paths = append(paths, part.Path)
	}
	return paths
}

// Capabilities records which pkgacct flags a given cPanel version accepts.
//
// Flag names have moved between cPanel releases, so the agent probes the
// installed binary at enrolment instead of assuming. An empty field means
// the flag was not found in the help output.
type Capabilities struct {
	NoCompressFlag  string
	SkipHomedirFlag string
	SkipDBFlag      string
	// SkipMailFlag leaves the account's mail messages out of pkgacct's
	// archive. It matters because a restic exclude cannot reach inside
	// that archive.
	SkipMailFlag string
	// SkipMailConfigFlag leaves the mail configuration out, which is a
	// different thing and the more sensitive one: cPanel keeps the mail
	// account names and their password hashes in the configuration, not
	// in the mail directory. Leaving email out with --skipmail alone
	// still shipped every mailbox password on the account.
	SkipMailConfigFlag string
}

// knownFlags maps the capability we need to the flag spellings observed
// across cPanel versions, most current first.
var knownFlags = map[string][]string{
	"nocompress":  {"--nocompress", "--uncompressed"},
	"skiphomedir": {"--skiphomedir", "--skiphome"},
	"skipdb":      {"--skipdb", "--skipmysql"},
	"skipmail":    {"--skipmail"},
	// cPanel's own help: "exclude mail configuration". Separate from
	// --skipmail, which is only the mail directory.
	"skipmailconfig": {"--skipmailconfig"},
}

// ProbeCapabilities parses the output of "pkgacct --help".
//
// Parsing help text is unlovely but beats hardcoding flags for a binary we
// do not ship and cannot version-pin.
func ProbeCapabilities(helpOutput string) Capabilities {
	find := func(key string) string {
		for _, flag := range knownFlags[key] {
			// Match the flag as a whole token so "--skiphome" does not
			// match inside "--skiphomedir".
			for _, field := range strings.FieldsFunc(helpOutput, isFlagSeparator) {
				if field == strings.TrimPrefix(flag, "--") {
					return flag
				}
			}
		}
		return ""
	}
	return Capabilities{
		NoCompressFlag:     find("nocompress"),
		SkipHomedirFlag:    find("skiphomedir"),
		SkipDBFlag:         find("skipdb"),
		SkipMailFlag:       find("skipmail"),
		SkipMailConfigFlag: find("skipmailconfig"),
	}
}

// PlanRequest describes the account to stage.
type PlanRequest struct {
	Account    string
	HomeDir    string
	Databases  []string
	StagingDir string
	Mode       Mode
	Caps       Capabilities
	// SkipHomedir and SkipDatabases leave those parts out, for a schedule
	// that asked for a backup of less than the whole account.
	SkipHomedir   bool
	SkipDatabases bool
	// SkipEmail leaves the account's mail out: the messages under the
	// home directory, and the mail configuration -- the addresses,
	// forwarders and filters -- that pkgacct would otherwise copy into
	// its archive.
	//
	// It does not reach the mailbox password hashes in every mode. They
	// live in ~/etc, which pkgacct has no flag for: in split mode the
	// home directory is backed up as files and an exclude keeps them
	// out, and in monolithic mode they stay in cPanel's own archive.
	SkipEmail bool
}

// Plan works out the payload for a request, without running anything.
//
// It reports Degraded rather than failing when the requested mode is only
// partly supported: a poorly deduplicating backup still beats no backup,
// provided the operator is told.
func Plan(req PlanRequest) (Payload, error) {
	if req.Account == "" {
		return Payload{}, fmt.Errorf("pkgacct: account is required")
	}
	if req.StagingDir == "" {
		return Payload{}, fmt.Errorf("pkgacct: staging directory is required")
	}

	switch req.Mode {
	case ModeSplit:
		return planSplit(req)
	case ModeMonolithic, "":
		return planMonolithic(req)
	default:
		return Payload{}, fmt.Errorf("pkgacct: unknown mode %q", req.Mode)
	}
}

func planSplit(req PlanRequest) (Payload, error) {
	if req.HomeDir == "" {
		return Payload{}, fmt.Errorf("pkgacct: split mode needs the account home directory")
	}
	payload := Payload{Mode: ModeSplit, Account: req.Account}
	payload.Parts = append(payload.Parts, Part{
		Kind: PartMetadata,
		Path: filepath.Join(req.StagingDir, "metadata"),
	})
	if !req.SkipHomedir {
		payload.Parts = append(payload.Parts, Part{Kind: PartHomedir, Path: req.HomeDir})
	}
	if len(req.Databases) > 0 && !req.SkipDatabases {
		databaseDir := filepath.Join(req.StagingDir, "databases")
		payload.Parts = append(payload.Parts, Part{Kind: PartDatabase, Path: databaseDir})
		payload.DumpPaths = make(map[string]string, len(req.Databases))
		for _, db := range req.Databases {
			payload.DumpPaths[db] = filepath.Join(databaseDir, db+".sql")
		}
	}
	if req.Caps.SkipHomedirFlag == "" {
		// Without a skip flag the metadata archive re-includes the home
		// directory, so it is stored twice: once as files, once inside
		// the archive.
		payload.Degraded = true
		payload.Reason = "pkgacct on this server cannot exclude the home directory; " +
			"metadata archive duplicates it"
	}
	return payload, nil
}

func planMonolithic(req PlanRequest) (Payload, error) {
	payload := Payload{
		Mode:    ModeMonolithic,
		Account: req.Account,
		Parts: []Part{{
			Kind: PartArchive,
			Path: filepath.Join(req.StagingDir, "cpmove-"+req.Account+".tar"),
		}},
	}
	var reasons []string
	if req.Caps.NoCompressFlag == "" {
		payload.Parts[0].Path += ".gz"
		reasons = append(reasons, "pkgacct on this server cannot disable compression; "+
			"restic deduplication will be close to zero and every run stores a full copy")
	}
	if req.SkipEmail {
		// pkgacct's own copy of the home directory leaves out ~/mail and
		// nothing else, and it has no flag for ~/etc, where cPanel keeps
		// the mailbox names and their password hashes. In split mode the
		// home directory is backed up as files and an exclude keeps them
		// out; here there is no way in, and saying the mail was left out
		// would not be true of the credentials.
		reasons = append(reasons, "pkgacct packs the whole home directory into its "+
			"archive apart from ~/mail, so leaving email out does not remove ~/etc -- "+
			"the mailbox names and password hashes are in this backup")
	}
	if len(reasons) > 0 {
		payload.Degraded = true
		payload.Reason = strings.Join(reasons, "; ")
	}
	return payload, nil
}

func isFlagSeparator(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' ||
		r == ',' || r == '[' || r == ']' || r == '|' || r == '-' || r == '='
}

// CommandArgs builds the pkgacct invocation for a mode, using only flags
// the host was probed to support.
//
// The archive is written into the staging directory rather than pkgacct's
// default location so the agent controls where the disk fills up.
func CommandArgs(account, stagingDir string, mode Mode, caps Capabilities, skipEmail bool) []string {
	var args []string
	if skipEmail {
		// An exclude given to restic cannot reach inside pkgacct's own
		// archive, so leaving mail out has to be said here as well --
		// twice, because cPanel counts the messages and the mail
		// configuration as separate things. The second one is the
		// addresses, forwarders and filters; the mailbox passwords are
		// in ~/etc, which neither flag touches.
		if caps.SkipMailFlag != "" {
			args = append(args, caps.SkipMailFlag)
		}
		if caps.SkipMailConfigFlag != "" {
			args = append(args, caps.SkipMailConfigFlag)
		}
	}
	if caps.NoCompressFlag != "" {
		// Compression here would defeat restic's deduplication entirely.
		args = append(args, caps.NoCompressFlag)
	}
	if mode == ModeSplit {
		if caps.SkipHomedirFlag != "" {
			args = append(args, caps.SkipHomedirFlag)
		}
		if caps.SkipDBFlag != "" {
			// Databases are dumped separately, one file each, so a single
			// changed table does not rewrite the whole payload.
			args = append(args, caps.SkipDBFlag)
		}
	}
	target := stagingDir
	if mode == ModeSplit {
		target = filepath.Join(stagingDir, "metadata")
	}
	return append(args, account, target)
}
