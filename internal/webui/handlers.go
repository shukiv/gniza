package webui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/shukiv/gniza/internal/agent"
	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/destination"
	"github.com/shukiv/gniza/internal/granular"
	"github.com/shukiv/gniza/internal/inventory"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/notify"
	"github.com/shukiv/gniza/internal/panel"
	"github.com/shukiv/gniza/internal/pkgacct"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/reassemble"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/staging"
	"github.com/shukiv/gniza/internal/update"
)

// --- dashboard ---

type dashboardView struct {
	Hostname     string
	Accounts     []accountView
	Destinations []destinationView
	Policies     []nodestore.Policy

	Protected      int
	Stale          int
	Unprotected    int
	Unscheduled    int
	Failed         int
	Partial        int
	OutOfDate      int
	CopyGaps       int
	Verified       int
	ProtectedPct   int
	StalePct       int
	UnprotectedPct int

	// Attention lists the accounts worth acting on, worst first.
	Attention     []accountView
	AttentionMore int

	NextRun       string
	NextRunPolicy string
	NextRunIn     string

	StagingFree uint64
	SpaceTight  bool

	// Managed is what these accounts amount to, as each account's last
	// backup measured it. Largest is the biggest of them, which is what
	// staging has to have room for.
	Managed        uint64
	Largest        uint64
	LargestAccount string

	// Runs is the recent past, newest first: one entry per firing of a
	// schedule rather than one per account. LastRun is the newest of
	// them, and nil on a server that has never run one.
	Runs    []runSummary
	LastRun *runSummary
	// BackedUp is how many accounts those runs stored between them: a
	// week of work as work rather than as rows.
	BackedUp int

	LastDrill *nodestore.Restore
	Lifecycle []nodestore.LifecycleEvent

	// Feed is what has happened recently, in one order: runs, rehearsals
	// and account lifecycle together.
	Feed []feedEvent
	// Schedules is what runs on its own, and Weakest the accounts with
	// the worst record. Weakest is empty on a server where nothing has
	// ever failed, which is a page that says so by leaving it out.
	Schedules []scheduleRow
	Weakest   []accountView

	// Held is what the destinations hold, from the last census of each
	// repository rather than from restic while this page draws.
	Held holdings
	// Now is when this page was drawn, so the template can say how old
	// each stored reading is without asking the clock itself.
	Now time.Time
}

// Verdict is the first line of the page: whether this server's accounts
// are covered, in one sentence.
//
// The question somebody opens this page with is "am I covered?", and a row
// of counters answers it only after they have been read and added up.
func (d dashboardView) Verdict() string {
	switch {
	case len(d.Destinations) == 0:
		return "Nothing is being backed up: this server has no destination."
	case len(d.Accounts) == 0:
		return "There are no accounts on this server yet."
	case d.Stale+d.Unprotected == 0:
		return fmt.Sprintf("All %d accounts are covered.", len(d.Accounts))
	}
	exposed := d.Stale + d.Unprotected
	is := "are"
	if exposed == 1 {
		is = "is"
	}
	return fmt.Sprintf("%d of %d accounts %s not covered.", exposed, len(d.Accounts), is)
}

// Band is how gravely to draw that sentence: the stripe down the side of
// it, and nothing else.
//
// An account that has never been backed up is a different thing from one
// whose copy is a day older than its schedule promised, and a page that
// draws both in red is a page whose red means nothing.
func (d dashboardView) Band() string {
	switch {
	case len(d.Destinations) == 0, d.Unprotected > 0, d.Failed > 0:
		return "bad"
	case d.Stale > 0, d.Unscheduled > 0, d.Partial > 0:
		return "warn"
	default:
		return "ok"
	}
}

// attentionLimit keeps the overview short: it is a prompt to act, not a
// second copy of the accounts page.
const attentionLimit = 3

// What the overview shows of each list before it stops being an overview.
const (
	feedShown      = 8
	lifecycleShown = 5
	weakestShown   = 3
)

// recentRuns is how far back the overview reads. A week covers "what
// happened last night" and every schedule that fires less often than
// daily, without walking years of jobs on every page draw.
const recentRuns = 7 * 24 * time.Hour

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	destinations, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	accounts, _, err := s.accountViews(r)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	policies, err := s.engine.Store().Policies()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	view := dashboardView{
		Hostname:     s.engine.Settings().Hostname,
		Accounts:     accounts,
		Destinations: destinations,
		Policies:     policies,
	}

	addCoverage(&view, accounts)
	if total := len(accounts); total > 0 {
		view.ProtectedPct = view.Protected * 100 / total
		view.StalePct = view.Stale * 100 / total
		// The remainder, so the bar always fills exactly.
		view.UnprotectedPct = 100 - view.ProtectedPct - view.StalePct
	}

	// Worst first: never backed up, then failing, then stale.
	for _, account := range accounts {
		if account.Current() || account.Running {
			continue
		}
		view.Attention = append(view.Attention, account)
	}
	sort.SliceStable(view.Attention, func(i, j int) bool {
		return attentionRank(view.Attention[i]) < attentionRank(view.Attention[j])
	})
	if len(view.Attention) > attentionLimit {
		view.AttentionMore = len(view.Attention) - attentionLimit
		view.Attention = view.Attention[:attentionLimit]
	}

	view.NextRun, view.NextRunPolicy, view.NextRunIn = nextRun(policies, time.Now())
	view.Now = time.Now()
	view.Held = holdingsOf(destinations, view.Now)

	// What last night actually did. The job bucket holds every backup
	// this server has ever made, so only the recent past is read back
	// into runs; anything still in flight is reported however old it is.
	if jobs, err := s.engine.Store().Jobs(0); err == nil {
		if runs := runsOf(jobs, policies, time.Now().Add(-recentRuns)); len(runs) > 0 {
			view.Runs, view.LastRun = runs, &runs[0]
			view.BackedUp = backedUp(runs)
		}
	} else {
		s.log.Error("read the recent runs", "error", err)
	}

	settings := s.engine.Settings()
	if free, err := stagingFree(settings.StagingRoot); err == nil {
		view.StagingFree = free
		// Roughly: not enough room for a large account plus headroom.
		view.SpaceTight = free < 10<<30
	}

	// One read of each, used twice: the rehearsals are both the card at
	// the foot of the page and part of the feed above it.
	restores, err := s.engine.Store().Restores(0)
	if err != nil {
		s.log.Error("read the rehearsals", "error", err)
	}
	for i := range restores {
		if restores[i].Kind == node.KindVerify && restores[i].Status.Terminal() {
			view.LastDrill = &restores[i]
			break
		}
	}
	if events, err := s.engine.Store().LifecycleEvents(lifecycleShown); err == nil {
		view.Lifecycle = events
	} else {
		s.log.Error("read the account lifecycle", "error", err)
	}

	view.Feed = feedOf(view.Runs, restores, view.Lifecycle, feedShown)
	view.Schedules = scheduleRows(policies, view.Runs, len(accounts), time.Now())
	view.Weakest = weakest(accounts, weakestShown)

	s.render(w, r, "dashboard.html", "Overview", "dashboard", view)
}

func addCoverage(view *dashboardView, accounts []accountView) {
	for _, account := range accounts {
		// What a completed backup read, which is the only measurement
		// this server has: listing accounts does not walk home
		// directories, and doing so on every page draw would.
		view.Managed += account.SizeBytes
		if account.SizeBytes > view.Largest {
			view.Largest, view.LargestAccount = account.SizeBytes, account.User
		}
		if account.Verified != nil && account.VerifiedOK {
			view.Verified++
		}
		if account.ExpectedEvery == 0 {
			view.Unscheduled++
		}
		switch account.settled() {
		case StateFailed:
			view.Failed++
		case StatePartial:
			view.Partial++
		case StateOutOfDate:
			view.OutOfDate++
		case StateCopyGap:
			view.CopyGaps++
		}
		switch {
		case account.Current():
			view.Protected++
		case account.LastBackup == nil:
			view.Unprotected++
		default:
			view.Stale++
		}
	}
}

func attentionRank(a accountView) int {
	switch {
	case a.LastBackup == nil:
		return 0
	case a.LastStatus == job.StatusFailed:
		return 1
	default:
		return 2
	}
}

// nextFireOf reports when one schedule fires next, and whether it fires at
// all: a schedule that is switched off or has nowhere to write does not
// run on its own, whatever its cron expression says.
func nextFireOf(policy nodestore.Policy, now time.Time) (time.Time, bool) {
	if !policy.Enabled || len(policy.RepositoryIDs) == 0 {
		return time.Time{}, false
	}
	schedule, err := cron.ParseStandard(policy.ScheduleCron)
	if err != nil {
		return time.Time{}, false
	}
	return schedule.Next(now), true
}

// nextRun reports when the soonest enabled schedule fires.
func nextRun(policies []nodestore.Policy, now time.Time) (at, name, in string) {
	var (
		soonest time.Time
		which   string
	)
	for _, policy := range policies {
		next, ok := nextFireOf(policy, now)
		if !ok {
			continue
		}
		if soonest.IsZero() || next.Before(soonest) {
			soonest, which = next, policy.Name
		}
	}
	if soonest.IsZero() {
		return "", "", ""
	}
	return soonest.Local().Format("15:04"), which, "in " + humanUntil(soonest.Sub(now))
}

func humanUntil(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

// --- destinations ---

type destinationView struct {
	nodestore.Destination
	Repository nodestore.Repository
	Endpoint   string
	Status     string
	// PublicKey is the line to add to the remote account's
	// authorized_keys, shown so it can be copied at any time rather than
	// only once when the destination was created.
	PublicKey    string
	RemoteTarget string
	// Keeps is the retention this destination would apply: the most
	// generous of every enabled schedule that writes here.
	Keeps nodestore.Retention
	// KeepsChanged says the schedules have been edited since retention
	// was approved here, so nothing will be deleted until the new plan
	// has been read. What was approved is on the repository.
	KeepsChanged bool
	// PlanApprovable says the plan on record was taken under the policy
	// in force now. Approving any other plan is refused, so the button
	// is not offered for one.
	PlanApprovable bool
}

// TypeName is the destination's type in the words the form used to offer
// it, so the table and the form agree with each other.
func (d destinationView) TypeName() string {
	switch destination.Type(d.Type) {
	case destination.TypeSFTP:
		return "Another Linux server (SFTP)"
	case destination.TypeREST:
		return "Backup server (restic REST)"
	case destination.TypeS3:
		return "S3 or S3-compatible"
	case destination.TypeLocal:
		return "Local disk or mounted NAS"
	}
	return d.Type
}

// Stripe is the severity colour on the row's leading edge.
func (d destinationView) Stripe() string {
	switch d.Status {
	case "ok":
		return "cpr-s-ok"
	case "error":
		return "cpr-s-bad"
	}
	return "cpr-s-warn"
}

func (s *Server) destinationViews() ([]destinationView, error) {
	destinations, err := s.engine.Store().Destinations()
	if err != nil {
		return nil, err
	}
	repositories, err := s.engine.Store().Repositories()
	if err != nil {
		return nil, err
	}
	policies, err := s.engine.Store().Policies()
	if err != nil {
		return nil, err
	}

	views := make([]destinationView, 0, len(destinations))
	for _, dest := range destinations {
		view := destinationView{Destination: dest, Endpoint: endpointOf(dest)}
		for _, repo := range repositories {
			if repo.DestinationID == dest.ID {
				view.Repository = repo
				break
			}
		}
		switch {
		case dest.LastCheckError != "":
			view.Status = "error"
		case dest.LastCheckedAt == nil:
			view.Status = "unchecked"
		default:
			view.Status = "ok"
		}
		if dest.Type == string(destination.TypeSFTP) {
			view.PublicKey = s.engine.PublicKeyFor(dest)
			view.RemoteTarget = dest.Config["user"] + "@" + dest.Config["host"]
		}
		if view.Repository.ID != "" {
			view.Keeps = node.MergedRetention(policies, view.Repository.ID)
			view.KeepsChanged = view.Repository.RetentionApprovedAt != nil &&
				!s.engine.RetentionApprovalCovers(view.Repository, view.Keeps)
			view.PlanApprovable = s.engine.PlanApprovable(view.Repository, view.Keeps)
		}
		views = append(views, view)
	}
	return views, nil
}

// endpointOf renders the human-facing address of a destination.
func endpointOf(dest nodestore.Destination) string {
	switch destination.Type(dest.Type) {
	case destination.TypeLocal:
		return dest.Config["root"]
	case destination.TypeSFTP:
		host := dest.Config["host"]
		if port := dest.Config["port"]; port != "" && port != "22" {
			host += ":" + port
		}
		return dest.Config["user"] + "@" + host + ":" + dest.Config["root"]
	case destination.TypeREST:
		return dest.Config["base_url"]
	case destination.TypeS3:
		endpoint := dest.Config["endpoint"]
		if endpoint == "" {
			endpoint = "s3.amazonaws.com"
		}
		return endpoint + "/" + dest.Config["bucket"]
	default:
		return ""
	}
}

// unnoted names the destinations whose recovery key has never been
// written down anywhere but this server.
func unnoted(views []destinationView) []string {
	var names []string
	for _, view := range views {
		if view.Repository.ID != "" && view.Repository.RecoveryNotedAt == nil {
			names = append(names, view.Name)
		}
	}
	return names
}

// handleRecoveryKey shows the password for one repository, once, because
// it was asked for.
//
// It is not on the page by default: a secret that is always rendered is a
// secret in every proxy log and every screenshot. This is a POST with the
// token, so it cannot be reached by a link someone was sent.
func (s *Server) handleRecoveryKey(w http.ResponseWriter, r *http.Request) {
	card, err := s.engine.Recovery(r.PostFormValue("repository"))
	if err != nil {
		s.redirect(w, r, "/destinations", "error", err.Error())
		return
	}
	views, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	s.render(w, r, "destinations.html", "Backup destinations", "destinations", destinationsView{
		Destinations: views,
		Hostname:     s.engine.Settings().Hostname,
		Revealed:     &card,
		RevealedID:   r.PostFormValue("repository"),
		Unnoted:      unnoted(views),
	})
}

// withoutRecoveryKey names the destinations among these repositories whose
// recovery key has never left this server.
func (s *Server) withoutRecoveryKey(repositoryIDs []string) []string {
	var names []string
	for _, id := range repositoryIDs {
		repo, err := s.engine.Store().Repository(id)
		if err != nil || repo.RecoveryNotedAt != nil {
			continue
		}
		name := repo.Path
		if dest, err := s.engine.Store().Destination(repo.DestinationID); err == nil {
			name = dest.Name
		}
		names = append(names, name)
	}
	return names
}

// repositoryOf is the repository kept at a destination.
func (s *Server) repositoryOf(destinationID string) (string, bool) {
	repositories, err := s.engine.Store().Repositories()
	if err != nil {
		s.log.Error("read the repositories", "error", err)
		return "", false
	}
	for _, repo := range repositories {
		if repo.DestinationID == destinationID {
			return repo.ID, true
		}
	}
	return "", false
}

// revealRecovery finishes adding a destination on its recovery key.
//
// The key is generated on this server and exists nowhere else. A
// destination whose key was never taken off the machine is a destination
// that cannot be restored from after the machine is lost, which is the
// case backups are for -- so it is put in front of the operator as the
// last step of adding one, rather than left behind a menu they have no
// reason to open.
func (s *Server) revealRecovery(w http.ResponseWriter, r *http.Request, repositoryID string) bool {
	card, err := s.engine.Recovery(repositoryID)
	if err != nil {
		s.log.Error("show the recovery key for a new destination",
			"repository_id", repositoryID, "error", err)
		return false
	}
	views, err := s.destinationViews()
	if err != nil {
		s.log.Error("read the destinations", "error", err)
		return false
	}
	s.render(w, r, "destinations.html", "Backup destinations", "destinations", destinationsView{
		Destinations: views,
		Hostname:     s.engine.Settings().Hostname,
		Revealed:     &card,
		RevealedID:   repositoryID,
		Unnoted:      unnoted(views),
		JustCreated:  true,
	})
	return true
}

// handleNoteRecovery records that the password has been written down
// somewhere off this server, which is what stops the warning.
func (s *Server) handleNoteRecovery(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.NoteRecoveryKey(r.PostFormValue("repository")); err != nil {
		s.redirect(w, r, "/destinations", "error", err.Error())
		return
	}
	s.redirect(w, r, "/destinations", "ok",
		"Noted. Keep it somewhere that survives this server — a password manager, "+
			"not a file on this machine.")
}

// handleRecoveryCard hands over the same thing as a file, for putting
// somewhere else. It is a download rather than a page so it does not sit
// in browser history.
func (s *Server) handleRecoveryCard(w http.ResponseWriter, r *http.Request) {
	card, err := s.engine.Recovery(r.PostFormValue("repository"))
	if err != nil {
		s.redirect(w, r, "/destinations", "error", err.Error())
		return
	}
	var body strings.Builder
	fmt.Fprintf(&body, `gniza recovery key
===================

Server        %s
Destination   %s
Repository    %s
Address       %s

Repository password
    %s

This password is the only thing that can read those backups. The
destination holds nothing but ciphertext, and the key that unlocks it
lives on %s. If that server is lost and this password is not written down
somewhere else, the backups it made cannot be read by anyone, including
gniza.

`, card.Hostname, card.Destination, card.Repository, card.URI,
		card.Password, card.Hostname)

	switch {
	case card.SSHPrivateKey != "":
		// Reaching an SFTP destination needs the key this server made for
		// it and the host key it pinned. A restore run anywhere else needs
		// both of those as well as the password.
		fmt.Fprintf(&body, `Reaching it from %s
------------------------------------------------------------------
    export RESTIC_REPOSITORY='%s'
    export RESTIC_PASSWORD='%s'

    restic -o sftp.args="%s" snapshots
    restic -o sftp.args="%s" restore <snapshot-id> --target /somewhere

Reaching it from anywhere else
------------------------------------------------------------------
That server's own SSH key is below. Write it to a file, make it readable
only by you, and point restic at it:

    install -m 600 /dev/null /root/gniza-key
    cat > /root/gniza-key <<'KEY'
%s
KEY
    printf '%%s\n' '%s' > /root/gniza-known-hosts

    export RESTIC_REPOSITORY='%s'
    export RESTIC_PASSWORD='%s'
    restic -o sftp.args="-i /root/gniza-key -o UserKnownHostsFile=/root/gniza-known-hosts -o StrictHostKeyChecking=yes -o IdentitiesOnly=yes" snapshots

If that key no longer works — the account was removed, or its
authorized_keys was rebuilt — any account with SSH access to
%s@%s will do. Drop the -o sftp.args and let ssh ask for a
password:

    export RESTIC_REPOSITORY='%s'
    restic snapshots

`, card.Hostname, card.URI, card.Password, card.ResticOptions, card.ResticOptions,
			card.SSHPrivateKey, card.SSHHostKey, card.URI, card.Password,
			card.SSHUser, card.SSHHost, card.URI)

	default:
		fmt.Fprintf(&body, `To restore without Gniza, on any machine with restic installed:

    export RESTIC_REPOSITORY='%s'
    export RESTIC_PASSWORD='%s'

    restic snapshots
    restic restore <snapshot-id> --target /somewhere

`, card.URI, card.Password)
	}

	fmt.Fprintf(&body, `Keep this file somewhere that survives the loss of %s. It carries
everything needed to read those backups, which is the point of it and
also the reason it belongs in a password manager rather than on a disk.
Written %s
`, card.Hostname, time.Now().UTC().Format("2006-01-02 15:04 MST"))

	// Downloading it is having it. The warning exists to make sure the key
	// leaves this server, and a file on the operator's machine is exactly
	// that; making them press "I have stored it somewhere else" as well
	// only teaches them to press it without reading.
	if err := s.engine.NoteRecoveryKey(r.PostFormValue("repository")); err != nil {
		s.log.Error("record that the recovery key was taken away", "error", err)
	}

	filename := fmt.Sprintf("gniza-recovery-%s-%s.txt", card.Hostname, card.Repository)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// This file is the only thing that can read the backups. No copy of
	// it belongs in a cache between here and the operator.
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	_, _ = w.Write([]byte(body.String()))
}

// destinationsView is the destinations page.
type destinationsView struct {
	Destinations []destinationView
	Hostname     string
	Editing      *destinationView
	// Adding renders the form as a page of its own rather than as a
	// drawer over the list. That is what a browser with no JavaScript
	// follows the button to, and what a bookmarked link opens.
	Adding bool
	// FormError is why the last attempt was refused, shown inside the
	// drawer with the form still filled in. A destination that could not
	// be reached used to send the operator back to an empty form with the
	// reason in a banner above it, which is a way of asking them to type
	// it all again.
	FormError string
	// ConfirmHost, HostFingerprint and HostKeyType are set when a remote
	// server has answered and the operator has not yet agreed to who it
	// is. Nothing has been sent to it at that point.
	ConfirmHost     string
	HostFingerprint string
	HostKeyType     string
	// Submitted is what they typed, so a rejected form comes back as they
	// left it.
	Submitted map[string]string
	// Prepared is a key made before the destination exists, so its public
	// half can be put on the far side by whoever administers that server
	// before anything here tries to log in.
	Prepared *node.PreparedKey
	// Revealed is a repository password an operator has just asked to
	// see, shown on this render and on no other.
	Revealed *node.RecoveryCard
	// RevealedID is the repository it belongs to, for the buttons beside
	// it. The password itself is never put in a form field.
	RevealedID string
	// Unnoted are the destinations whose recovery key exists nowhere but
	// this server.
	Unnoted []string
	// JustCreated says the destination on this page was made a moment
	// ago, so the card is the last step of adding it rather than an
	// answer to somebody asking to see the key.
	JustCreated bool
}

// Field is what an input should show: what was submitted, else what is
// stored for the destination being edited.
// ChosenType is the type the form is showing: what was submitted when a
// form is coming back, and the first option otherwise.
//
// A form that is re-rendered -- a refused destination, a host key to agree
// to, a key just made -- used to come back with the select on its first
// option and the section for the chosen type hidden, so the fields
// somebody had filled in were still there and invisible.
func (v destinationsView) ChosenType() string {
	if chosen := v.Field("type"); chosen != "" {
		return chosen
	}
	return string(destination.TypeREST)
}

// HiddenFor says whether one type's fields start hidden. Without
// JavaScript this is the only thing that hides them, and with it the
// select takes over from here.
func (v destinationsView) HiddenFor(kind string) bool {
	return v.ChosenType() != kind
}

func (v destinationsView) Field(name string) string {
	if value, typed := v.Submitted[name]; typed {
		return value
	}
	if v.Editing == nil {
		return ""
	}
	switch name {
	case "name":
		return v.Editing.Name
	default:
		return v.Editing.Config[name]
	}
}

// FieldOr is Field with a default for the fields that have one.
func (v destinationsView) FieldOr(name, fallback string) string {
	if value := v.Field(name); value != "" {
		return value
	}
	return fallback
}

func (s *Server) handleDestinations(w http.ResponseWriter, r *http.Request) {
	views, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	view := destinationsView{
		Destinations: views,
		Hostname:     s.engine.Settings().Hostname,
		Adding:       r.URL.Query().Get("add") != "",
		Unnoted:      unnoted(views),
	}

	if id := r.URL.Query().Get("edit"); id != "" {
		for i := range views {
			if views[i].ID == id {
				view.Editing = &views[i]
				break
			}
		}
		if view.Editing == nil {
			s.redirect(w, r, "/destinations", "error", "That destination no longer exists.")
			return
		}
	}
	s.render(w, r, "destinations.html", "Backup destinations", "destinations", view)
}

// refuseDestination hands the form back with what went wrong and what was
// typed, rather than sending the operator to an empty one with the reason
// in a banner above it.
func (s *Server) refuseDestination(w http.ResponseWriter, r *http.Request, cause error) {
	views, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	view := destinationsView{
		Destinations: views,
		Hostname:     s.engine.Settings().Hostname,
		FormError:    cause.Error(),
		Submitted:    map[string]string{},
	}
	for name, values := range r.PostForm {
		// Secrets are not handed back: they would then live in a page
		// rather than only in the vault, and the operator retyping one is
		// a smaller cost than that.
		switch name {
		case "csrf", "password", "secret_access_key", "admin_password":
			continue
		}
		if len(values) > 0 {
			view.Submitted[name] = values[0]
		}
	}
	if id := r.PostFormValue("id"); id != "" {
		for i := range views {
			if views[i].ID == id {
				view.Editing = &views[i]
				break
			}
		}
	}
	s.keepPreparedKey(&view)
	s.render(w, r, "destinations.html", "Backup destinations", "destinations", view)
}

// keepPreparedKey shows again the key an earlier click made, when the form
// that comes back is still pointing at it.
//
// A form is handed back more than once -- something was refused, or a host
// key is waiting to be agreed to -- and the key whose public half somebody
// may already have installed on the far side must stay on the page. What
// happened before was that the green block vanished and the button to make
// a key took its place, so pressing it made a second key and pointed the
// form at that one instead.
func (s *Server) keepPreparedKey(view *destinationsView) {
	if prepared, ok := s.engine.PreparedKeyAt(view.Submitted["identity_file"]); ok {
		view.Prepared = &prepared
	}
}

// handlePrepareKey makes an SSH key and hands the form back with the public
// half in it, ready to copy.
//
// The key is generated here rather than when the destination is saved
// because the usual order is the other way round: a backup server is often
// administered by somebody else, and what they want from you is a public
// key. Before this, there was nothing to give them until the destination
// that needed it had already been created.
func (s *Server) handlePrepareKey(w http.ResponseWriter, r *http.Request) {
	prepared, err := s.engine.PrepareSFTPKey()
	if err != nil {
		s.refuseDestination(w, r, err)
		return
	}
	views, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	view := destinationsView{
		Destinations: views,
		Hostname:     s.engine.Settings().Hostname,
		Adding:       true,
		Prepared:     &prepared,
		Submitted:    map[string]string{},
	}
	for name, values := range r.PostForm {
		// Secrets are not handed back, here as everywhere else: they
		// would then live in a page rather than only in the vault.
		switch name {
		case "csrf", "password", "secret_access_key", "admin_password":
			continue
		}
		if len(values) > 0 {
			view.Submitted[name] = values[0]
		}
	}
	// The form now points at the key that was just made, so saving the
	// destination uses the one whose public half was copied out of this
	// page rather than making a second one nobody installed.
	view.Submitted["identity_file"] = prepared.Path
	view.Submitted["type"] = string(destination.TypeSFTP)

	s.render(w, r, "destinations.html", "Backup destinations", "destinations", view)
}

func (s *Server) handleAddDestination(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	destType := r.PostFormValue("type")
	if name == "" {
		s.refuseDestination(w, r, fmt.Errorf("Give the destination a name."))
		return
	}

	config := map[string]string{}
	secrets := map[string]string{}
	switch destination.Type(destType) {
	case destination.TypeREST:
		config["base_url"] = strings.TrimSpace(r.PostFormValue("base_url"))
		if maintenance := strings.TrimSpace(r.PostFormValue("maintenance_base_url")); maintenance != "" {
			config["maintenance_base_url"] = maintenance
		}
		if r.PostFormValue("append_only") != "" {
			config["append_only"] = "true"
		}
		if bundle := strings.TrimSpace(r.PostFormValue("ca_bundle")); bundle != "" {
			config["ca_bundle"] = bundle
		}
		secrets["username"] = strings.TrimSpace(r.PostFormValue("username"))
		secrets["password"] = r.PostFormValue("password")
	case destination.TypeSFTP:
		s.addSFTPDestination(w, r, name, repositoryPathFrom(r, s.engine.Settings().Hostname))
		return
	case destination.TypeS3:
		config["bucket"] = strings.TrimSpace(r.PostFormValue("bucket"))
		if endpoint := strings.TrimSpace(r.PostFormValue("endpoint")); endpoint != "" {
			config["endpoint"] = endpoint
		}
		if region := strings.TrimSpace(r.PostFormValue("region")); region != "" {
			config["region"] = region
		}
		secrets["access_key_id"] = strings.TrimSpace(r.PostFormValue("access_key_id"))
		secrets["secret_access_key"] = r.PostFormValue("secret_access_key")
	case destination.TypeLocal:
		config["root"] = strings.TrimSpace(r.PostFormValue("root"))
	default:
		s.refuseDestination(w, r, fmt.Errorf("Choose a destination type."))
		return
	}

	repositoryPath := repositoryPathFrom(r, s.engine.Settings().Hostname)

	s.saveDestination(w, r, nodestore.Destination{
		Name:       name,
		Type:       destType,
		Config:     config,
		AppendOnly: config["append_only"] == "true",
	}, secrets, repositoryPath)
}

// saveDestination stores a destination, then proves it works while the
// operator is still looking at the screen.
func (s *Server) saveDestination(w http.ResponseWriter, r *http.Request,
	dest nodestore.Destination, secrets map[string]string, repositoryPath string) {

	name := dest.Name
	stored, created, err := s.engine.AddDestination(dest, secrets, repositoryPath)
	if err != nil {
		s.refuseDestination(w, r, err)
		return
	}

	// Check it now, while the operator is still looking at the screen.
	if err := s.engine.TestDestination(r.Context(), stored.ID); err != nil {
		s.redirect(w, r, "/destinations", "warn",
			fmt.Sprintf("Saved %q, but it could not be reached: %v", name, err))
		return
	}
	if _, err := s.engine.EnsureProvisioned(r.Context()); err != nil {
		s.redirect(w, r, "/destinations", "warn",
			fmt.Sprintf("Saved %q and reached it, but creating the repository failed: %v", name, err))
		return
	}
	if s.revealRecovery(w, r, created.ID) {
		return
	}
	s.redirect(w, r, "/destinations", "ok",
		fmt.Sprintf("%q is reachable and its repository is ready.", name))
}

// addSFTPDestination sets up another Linux server as a destination.
//
// Gniza generates its own key, reads the server's host key, and — if the
// operator supplied the remote password — installs the key and proves it
// works. The password is used for that and discarded.
func (s *Server) addSFTPDestination(w http.ResponseWriter, r *http.Request, name, repositoryPath string) {
	s.finishSFTP(w, r, node.SFTPRequest{
		Name:            name,
		Host:            r.PostFormValue("host"),
		Port:            atoiOr(r.PostFormValue("port"), 22),
		User:            r.PostFormValue("user"),
		RemoteDir:       r.PostFormValue("root"),
		RepositoryPath:  repositoryPath,
		Password:        r.PostFormValue("password"),
		AdminUser:       strings.TrimSpace(r.PostFormValue("admin_user")),
		AdminPassword:   r.PostFormValue("admin_password"),
		ExistingKeyPath: strings.TrimSpace(r.PostFormValue("identity_file")),
		// Empty until the operator has been shown who answered on that
		// address and has agreed it is the right server.
		ConfirmedFingerprint: strings.TrimSpace(r.PostFormValue("confirm_fingerprint")),
	})
}

// confirmHost re-renders the form with the host key the far side
// presented, and nothing else changed.
//
// The password is not carried back -- no secret is -- so the operator
// retypes it. That is the cost of not holding a remote server's root
// password in this process between two page loads.
func (s *Server) confirmHost(w http.ResponseWriter, r *http.Request, unconfirmed *node.UnconfirmedHostError) {
	views, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	view := destinationsView{
		Destinations:    views,
		Hostname:        s.engine.Settings().Hostname,
		Adding:          true,
		Submitted:       map[string]string{},
		ConfirmHost:     unconfirmed.Host,
		HostFingerprint: unconfirmed.Fingerprint,
		HostKeyType:     unconfirmed.KeyType,
	}
	for name, values := range r.PostForm {
		switch name {
		case "csrf", "password", "secret_access_key", "admin_password":
			continue
		}
		if len(values) > 0 {
			view.Submitted[name] = values[0]
		}
	}
	s.keepPreparedKey(&view)
	s.render(w, r, "destinations.html", "Destinations", "destinations", view)
}

// finishSFTP runs a prepared request and says what happened.
func (s *Server) finishSFTP(w http.ResponseWriter, r *http.Request, request node.SFTPRequest) {
	result, err := s.engine.AddSFTPDestination(request)
	var unconfirmed *node.UnconfirmedHostError
	if errors.As(err, &unconfirmed) {
		// Nothing has been sent to that server. The form comes back with
		// the fingerprint to check and a box to agree to it.
		s.confirmHost(w, r, unconfirmed)
		return
	}
	if err != nil {
		s.refuseDestination(w, r, err)
		return
	}

	switch {
	case result.Verified:
		if _, err := s.engine.EnsureProvisioned(r.Context()); err != nil {
			s.redirect(w, r, "/destinations", "warn", fmt.Sprintf(
				"Logged in to %s, but creating the repository failed: %v", result.Destination.Name, err))
			return
		}
		if s.revealRecovery(w, r, result.Repository.ID) {
			return
		}
		if result.Created {
			s.redirect(w, r, "/destinations", "ok", fmt.Sprintf(
				"%s is ready. Gniza created %s on that server, gave it a locked password so "+
					"the key is the only way in, made the backup directory and created the "+
					"repository.", result.Destination.Name, request.User))
			return
		}
		s.redirect(w, r, "/destinations", "ok", fmt.Sprintf(
			"%s is ready. Gniza installed its own key and the repository is created.",
			result.Destination.Name))
	case result.Warning != "":
		// The login is not proved yet -- typically the key still has to be
		// installed on that server -- so creating the repository will
		// usually fail here. It is still attempted, because the one case
		// where it works is the operator who put the key there already,
		// and the warning is what they are shown either way.
		if created, err := s.engine.EnsureProvisioned(r.Context()); err == nil && created > 0 {
			if s.revealRecovery(w, r, result.Repository.ID) {
				return
			}
			s.redirect(w, r, "/destinations", "warn",
				result.Warning+" The repository is created.")
			return
		}
		s.redirect(w, r, "/destinations", "warn", result.Warning)
	default:
		if _, err := s.engine.EnsureProvisioned(r.Context()); err != nil {
			s.redirect(w, r, "/destinations", "warn",
				"Saved, but creating the repository failed: "+err.Error())
			return
		}
		if s.revealRecovery(w, r, result.Repository.ID) {
			return
		}
		s.redirect(w, r, "/destinations", "ok", "Saved, and the repository is created.")
	}
}

// repositoryPathFrom keeps this server's backups in their own folder.
func repositoryPathFrom(r *http.Request, hostname string) string {
	path := strings.TrimSpace(r.PostFormValue("repo_path"))
	if path == "" {
		path = hostname
	}
	if path == "" {
		path = "gniza"
	}
	return path
}

// handleEditDestination updates a destination that already exists.
//
// Credentials left blank are kept, so an operator correcting a hostname is
// not made to retype an access key. The repository path is not editable
// once the repository exists: it is where the backups already are, and
// changing it would silently start a new empty repository elsewhere.
func (s *Server) handleEditDestination(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	dest, err := s.engine.Store().Destination(id)
	if err != nil {
		s.redirect(w, r, "/destinations", "error", "That destination no longer exists.")
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		s.redirect(w, r, "/destinations?edit="+id, "error", "Give the destination a name.")
		return
	}
	dest.Name = name

	config := map[string]string{}
	secrets := map[string]string{}
	switch destination.Type(dest.Type) {
	case destination.TypeREST:
		config["base_url"] = strings.TrimSpace(r.PostFormValue("base_url"))
		if maintenance := strings.TrimSpace(r.PostFormValue("maintenance_base_url")); maintenance != "" {
			config["maintenance_base_url"] = maintenance
		}
		if r.PostFormValue("append_only") != "" {
			config["append_only"] = "true"
		}
		if bundle := strings.TrimSpace(r.PostFormValue("ca_bundle")); bundle != "" {
			config["ca_bundle"] = bundle
		}
		if user := strings.TrimSpace(r.PostFormValue("username")); user != "" {
			secrets["username"] = user
			secrets["password"] = r.PostFormValue("password")
		}
	case destination.TypeSFTP:
		config["host"] = strings.TrimSpace(r.PostFormValue("host"))
		config["user"] = strings.TrimSpace(r.PostFormValue("user"))
		config["root"] = strings.TrimSpace(r.PostFormValue("root"))
		// The generated key and the learned host key belong to this
		// destination and are not something to retype.
		config["identity_file"] = dest.Config["identity_file"]
		config["known_hosts_file"] = dest.Config["known_hosts_file"]
		if port := strings.TrimSpace(r.PostFormValue("port")); port != "" && port != "22" {
			config["port"] = port
		}
	case destination.TypeS3:
		config["bucket"] = strings.TrimSpace(r.PostFormValue("bucket"))
		if endpoint := strings.TrimSpace(r.PostFormValue("endpoint")); endpoint != "" {
			config["endpoint"] = endpoint
		}
		if region := strings.TrimSpace(r.PostFormValue("region")); region != "" {
			config["region"] = region
		}
		if key := strings.TrimSpace(r.PostFormValue("access_key_id")); key != "" {
			secrets["access_key_id"] = key
			secrets["secret_access_key"] = r.PostFormValue("secret_access_key")
		}
	case destination.TypeLocal:
		config["root"] = strings.TrimSpace(r.PostFormValue("root"))
	}
	dest.Config = config
	dest.AppendOnly = config["append_only"] == "true"

	if len(secrets) > 0 {
		secretID, err := node.SealCredentials(s.engine.Store(), s.engine.Vault(), secrets)
		if err != nil {
			s.redirect(w, r, "/destinations?edit="+id, "error", err.Error())
			return
		}
		dest.CredentialsSecretID = secretID
	}

	if err := s.engine.SaveDestination(dest); err != nil {
		s.redirect(w, r, "/destinations?edit="+id, "error", err.Error())
		return
	}
	if err := s.engine.TestDestination(r.Context(), id); err != nil {
		s.redirect(w, r, "/destinations", "warn",
			fmt.Sprintf("Saved, but %s could not be reached: %v", name, err))
		return
	}
	if _, err := s.engine.EnsureProvisioned(r.Context()); err != nil {
		s.redirect(w, r, "/destinations", "warn", fmt.Sprintf(
			"%s is reachable, but creating the repository failed: %v", name, err))
		return
	}
	s.redirect(w, r, "/destinations", "ok", name+" updated and reachable.")
}

func (s *Server) handleTestDestination(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	if err := s.engine.TestDestination(r.Context(), id); err != nil {
		s.redirect(w, r, "/destinations", "error", err.Error())
		return
	}
	created, err := s.engine.EnsureProvisioned(r.Context())
	switch {
	case err != nil:
		s.redirect(w, r, "/destinations", "warn",
			"That server answered, but creating the repository failed: "+err.Error())
	case created > 0:
		if repo, found := s.repositoryOf(id); found && s.revealRecovery(w, r, repo) {
			return
		}
		s.redirect(w, r, "/destinations", "ok",
			"Destination reachable, and its repository is created.")
	default:
		s.redirect(w, r, "/destinations", "ok", "Destination reachable.")
	}
}

func (s *Server) handleDeleteDestination(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	if err := s.engine.Store().DeleteDestination(id); err != nil {
		s.redirect(w, r, "/destinations", "error", "Could not remove it: "+err.Error())
		return
	}
	s.redirect(w, r, "/destinations", "ok",
		"Destination removed. Anything already stored there is untouched, "+
			"but Gniza no longer knows how to read it.")
}

// --- schedules ---

// excludePresets are directories that are rebuilt from what is already
// backed up: caches, compiled templates, session files. Storing them costs
// space every night and gives a restore nothing.
var excludePresets = map[string][]string{
	"Caches and temporary files": {
		"/home/*/tmp",
		"/home/*/.cpanel/caches",
		"/home/*/.cphorde",
		"/home/*/public_html/*/tmp",
		"/home/*/.cache",
		"**/node_modules",
		"**/.git",
	},
	"WordPress": {
		"/home/*/public_html/wp-content/cache",
		"/home/*/public_html/wp-content/uploads/cache",
		"/home/*/public_html/wp-content/backup*",
		"/home/*/public_html/wp-content/updraft",
		"/home/*/public_html/wp-content/ai1wm-backups",
	},
	"Magento": {
		"/home/*/public_html/var/cache",
		"/home/*/public_html/var/page_cache",
		"/home/*/public_html/var/session",
		"/home/*/public_html/generated",
		"/home/*/public_html/pub/static",
	},
	"PrestaShop": {
		"/home/*/public_html/var/cache",
		"/home/*/public_html/cache/smarty",
		"/home/*/public_html/img/tmp",
	},
	"Moodle": {
		"/home/*/moodledata/cache",
		"/home/*/moodledata/localcache",
		"/home/*/moodledata/sessions",
		"/home/*/moodledata/temp",
		"/home/*/moodledata/trashdir",
	},
}

// policyView is a schedule as the schedules page states it: the stored
// policy, plus the things the page says in words rather than in cron.
type policyView struct {
	nodestore.Policy
	// AccountCount is how many accounts this schedule covers right now.
	AccountCount int
	// Next is when it fires next, zero when it never will.
	Next time.Time
}

// Stripe is the severity colour on the row's leading edge.
func (p policyView) Stripe() string {
	switch {
	case !p.Enabled:
		return "cpr-s-warn"
	case len(p.RepositoryIDs) == 0:
		return "cpr-s-bad"
	default:
		return "cpr-s-ok"
	}
}

// When reads the schedule back in words. A hand-written expression that
// does not fit one of the shapes the form offers returns empty, and the
// page shows the expression alone.
func (p policyView) When() string { return humanCron(p.ScheduleCron) }

// NextIn is how long until this schedule fires, empty when it never will.
func (p policyView) NextIn() string {
	if p.Next.IsZero() {
		return ""
	}
	return "next in " + humanUntil(time.Until(p.Next))
}

// Covers names what the schedule backs up.
func (p policyView) Covers() string {
	if p.AllAccounts() {
		return "Every account"
	}
	return strings.Join(p.Accounts, ", ")
}

// CoversDetail counts what that comes to today, which matters for a
// schedule that says "every account" on a server where accounts come
// and go.
func (p policyView) CoversDetail() string {
	if !p.AllAccounts() {
		return ""
	}
	return fmt.Sprintf("%d account%s", p.AccountCount, plural(p.AccountCount))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

var weekdays = [7]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

// humanCron reads back the shapes the schedule form offers. Anything else
// returns empty rather than a wrong reading of someone's own expression.
func humanCron(expr string) string {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return ""
	}
	minute, hour, dayOfMonth, month, dayOfWeek := fields[0], fields[1], fields[2], fields[3], fields[4]
	if month != "*" {
		return ""
	}
	m, minuteIsNumber := atoi(minute)
	h, hourIsNumber := atoi(hour)
	switch {
	case !minuteIsNumber:
		return ""
	case hour == "*" && dayOfMonth == "*" && dayOfWeek == "*":
		return fmt.Sprintf("Every hour at :%02d", m)
	case !hourIsNumber:
		return ""
	case dayOfMonth == "*" && dayOfWeek == "*":
		return fmt.Sprintf("Every day at %02d:%02d", h, m)
	case dayOfMonth == "*":
		if d, ok := atoi(dayOfWeek); ok && d >= 0 && d <= 7 {
			return fmt.Sprintf("Every %s at %02d:%02d", weekdays[d%7], h, m)
		}
	}
	return ""
}

func atoi(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	policies, err := s.engine.Store().Policies()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	destinations, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	accounts, _, err := s.accountViews(r)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	now := time.Now()
	views := make([]policyView, 0, len(policies))
	for _, policy := range policies {
		view := policyView{Policy: policy, AccountCount: len(accounts)}
		if !policy.AllAccounts() {
			view.AccountCount = len(policy.Accounts)
		}
		if policy.Enabled && len(policy.RepositoryIDs) > 0 {
			if schedule, err := cron.ParseStandard(policy.ScheduleCron); err == nil {
				view.Next = schedule.Next(now)
			}
		}
		views = append(views, view)
	}

	view := struct {
		Policies     []policyView
		Destinations []destinationView
		Accounts     []accountView
		Editing      *nodestore.Policy
		Selected     map[string]bool
		Chosen       map[string]bool
		// ExcludePresets are the paths worth never storing, by the thing
		// that puts them there. They are suggestions the operator can
		// edit, not rules this program applies on its own.
		ExcludePresets map[string][]string
	}{
		Policies: views, Destinations: destinations, Accounts: accounts,
		ExcludePresets: excludePresets,
	}

	if id := r.URL.Query().Get("edit"); id != "" {
		policy, err := s.engine.Store().Policy(id)
		if err != nil {
			s.redirect(w, r, "/schedule", "error", "That schedule no longer exists.")
			return
		}
		view.Editing = &policy
		view.Selected = map[string]bool{}
		for _, repositoryID := range policy.RepositoryIDs {
			view.Selected[repositoryID] = true
		}
		view.Chosen = map[string]bool{}
		for _, account := range policy.Accounts {
			view.Chosen[account] = true
		}
	}
	s.render(w, r, "schedule.html", "Schedules", "schedule", view)
}

// linesOf reads a textarea into a list, dropping blank lines and the
// spaces around each one.
func linesOf(raw string) []string {
	var lines []string
	for _, line := range strings.Split(raw, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

func (s *Server) handleSaveSchedule(w http.ResponseWriter, r *http.Request) {
	policy := nodestore.Policy{
		ID:           r.PostFormValue("id"),
		Name:         strings.TrimSpace(r.PostFormValue("name")),
		ScheduleCron: strings.TrimSpace(r.PostFormValue("cron")),
		PayloadMode:  r.PostFormValue("mode"),
		Compression:  r.PostFormValue("compression"),
		Enabled:      r.PostFormValue("enabled") != "",
		Retention: nodestore.Retention{
			KeepDaily:   atoiOr(r.PostFormValue("keep_daily"), 7),
			KeepWeekly:  atoiOr(r.PostFormValue("keep_weekly"), 4),
			KeepMonthly: atoiOr(r.PostFormValue("keep_monthly"), 6),
		},
		RepositoryIDs: r.PostForm["repository"],
		// A schedule that leaves something out says so here; everything
		// it does not mention travels.
		IncludeSystem:     r.PostFormValue("include_system") != "",
		SkipHomedir:       r.PostFormValue("skip_homedir") != "",
		SkipDatabases:     r.PostFormValue("skip_databases") != "",
		SkipEmail:         r.PostFormValue("skip_email") != "",
		RetryFailed:       r.PostFormValue("retry_failed") != "",
		Excludes:          linesOf(r.PostFormValue("excludes")),
		AlertNoBackupDays: atoiOr(r.PostFormValue("alert_no_backup_days"), 0),
		AlertRunHours:     atoiOr(r.PostFormValue("alert_run_hours"), 0),
	}
	if policy.Name == "" || policy.ScheduleCron == "" {
		s.redirect(w, r, "/schedule", "error", "A schedule needs a name and a time.")
		return
	}
	if len(policy.RepositoryIDs) == 0 {
		s.redirect(w, r, "/schedule", "error",
			"Choose at least one destination, or nothing will be backed up.")
		return
	}
	// A backup nobody can read is not a backup. The recovery key is made
	// on this server and, until it has been taken off it, the only copy
	// is on the machine the backups exist to survive -- so a schedule
	// that would start writing there waits for it.
	if policy.Enabled {
		if names := s.withoutRecoveryKey(policy.RepositoryIDs); len(names) > 0 {
			s.redirect(w, r, "/destinations", "error", fmt.Sprintf(
				"Take the recovery key for %s off this server first: press "+
					"\u201cRecovery key\u201d, then download it or say you have stored "+
					"it. Without it, nothing can read what this schedule would "+
					"write.", strings.Join(names, " and ")))
			return
		}
	}
	// Empty means every account, resolved when the schedule fires so new
	// accounts are included without an edit.
	if r.PostFormValue("scope") == "selected" {
		policy.Accounts = r.PostForm["account"]
		if len(policy.Accounts) == 0 {
			s.redirect(w, r, "/schedule", "error", "Choose at least one account.")
			return
		}
	}
	if existing := policy.ID; existing != "" {
		previous, err := s.engine.Store().Policy(existing)
		if err == nil {
			policy.CreatedAt = previous.CreatedAt
			policy.LastRunAt = previous.LastRunAt
		}
	}

	editing := policy.ID != ""
	if _, err := s.engine.Store().PutPolicy(policy); err != nil {
		s.redirect(w, r, "/schedule", "error", err.Error())
		return
	}
	if editing {
		s.redirect(w, r, "/schedule", "ok", "Schedule updated.")
		return
	}
	s.redirect(w, r, "/schedule", "ok", "Schedule saved.")
}

// handleRunSchedule queues a schedule's accounts straight away, rather
// than waiting for its next cron time.
func (s *Server) handleRunSchedule(w http.ResponseWriter, r *http.Request) {
	queued, skipped, err := s.engine.RunPolicyNow(r.Context(), r.PostFormValue("id"))
	if err != nil {
		s.redirect(w, r, "/schedule", "error", err.Error())
		return
	}

	switch {
	case queued == 0:
		s.redirect(w, r, "/schedule", "warn", fmt.Sprintf(
			"Nothing queued: %s already being backed up.", strings.Join(skipped, ", ")))
	case len(skipped) > 0:
		s.redirect(w, r, "/schedule", "warn", fmt.Sprintf(
			"Queued %d account(s). Skipped %s, already being backed up.",
			queued, strings.Join(skipped, ", ")))
	default:
		s.redirect(w, r, "/schedule", "ok", fmt.Sprintf(
			"Queued %d account(s). Watch progress under Logs.", queued))
	}
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.Store().DeletePolicy(r.PostFormValue("id")); err != nil {
		s.redirect(w, r, "/schedule", "error", err.Error())
		return
	}
	s.redirect(w, r, "/schedule", "ok", "Schedule removed. Existing backups are untouched.")
}

// --- accounts ---

type accountView struct {
	panel.AccountInfo
	LastBackup *time.Time
	LastStatus job.Status
	Running    bool
	// Progress is how far the running backup of this account has got,
	// nil when nothing is running for it.
	Progress *nodestore.JobProgress
	// ExpectedEvery is how often the schedules covering this account fire.
	// Zero means no enabled schedule covers it: whatever backup it has is
	// the last one it will ever get.
	ExpectedEvery time.Duration
	// AlertAfter is how long without a good backup this account's
	// schedule considers worth saying out loud.
	AlertAfter time.Duration
	// Runs and Succeeded are this account's record: how many backups have
	// finished, and how many of those worked. One good backup says little
	// on its own — a run that succeeds nine times in ten is a different
	// account to look after than one that has never failed.
	Runs      int
	Succeeded int
	// Verified is when a rehearsal last proved this account's backup
	// rebuilds, with VerifiedOK saying whether it passed.
	Verified   *time.Time
	VerifiedOK bool
	// VerifiedSkipped names what the rehearsed backup was taken without.
	// A rehearsal of a backup with no databases in it proves that backup
	// and says nothing about the databases, so it does not count as the
	// account having been verified.
	VerifiedSkipped []string
	// FreshCopies and MissingCopies compare successful target writes with
	// what every enabled policy promises for this account. A healthy copy
	// in one destination must not hide a missing second copy.
	FreshCopies      []string
	MissingCopies    []string
	RepairPolicyID   string
	RepairPolicyName string
	// RemovalSafety is present only when the cPanel pre-termination gate is
	// enabled. It is computed by the same node evaluator the hook invokes.
	RemovalSafety *node.RemovalSafety
	// Stored is what the last backup actually cost across its
	// destinations, after restic deduplicated it, and Took is how long the
	// whole run lasted — staging the account included, which is most of it
	// for a small account.
	Stored uint64
	Took   time.Duration
	// LastError is why the last backup failed, in the words it failed
	// with. A row that says only "the last run failed" sends the operator
	// to another page to find out what every row already knows.
	LastError string
	// Copies is how many backups of this account each destination has
	// taken. Counted from this server's own records, so it says what was
	// written rather than what survives: retention prunes old snapshots
	// in the repository, and nothing tells this server when it does.
	Copies []copyCount
}

// copyCount is one destination's share of an account's backups.
type copyCount struct {
	Destination string
	Written     int
}

// Record is the account's history in one line.
func (a accountView) Record() string {
	if a.Runs == 0 {
		return ""
	}
	return fmt.Sprintf("%d of %d succeeded", a.Succeeded, a.Runs)
}

// Failures is how many runs did not work, for the pages that show it as
// its own number rather than as part of a ratio.
func (a accountView) Failures() int { return a.Runs - a.Succeeded }

// NeedsRemovalPreparation reports whether WHM can offer the remediation
// action. A missing full-account policy needs configuration, not a button.
func (a accountView) NeedsRemovalPreparation() bool {
	return a.RemovalSafety != nil && !a.RemovalSafety.Allowed &&
		len(a.RemovalSafety.MissingRepositoryIDs) > 0
}

// Stripe is the severity colour on the row's leading edge, so state reads
// without depending on colour alone.
func (a accountView) Stripe() string {
	switch {
	case a.Running:
		return "cpr-s-warn"
	case a.LastBackup == nil, a.LastStatus == job.StatusFailed:
		return "cpr-s-bad"
	case a.LastStatus == job.StatusPartialSuccess, len(a.MissingCopies) > 0:
		return "cpr-s-warn"
	default:
		return "cpr-s-ok"
	}
}

// Filter is the value the client-side state filter matches on.
func (a accountView) Filter() string {
	switch a.State() {
	case StateProtected:
		return "ok"
	case StateNever:
		return "bad"
	default:
		return "warn"
	}
}

// State is what this account's backups amount to right now.
//
// "Protected" has to mean something an operator can rely on, so it is not
// merely "a backup once worked". A copy that succeeded six months ago and
// is refreshed by nothing protects a site that has changed every day
// since; a copy nothing will refresh again is worth saying out loud.
type State string

const (
	StateWorking     State = "working"
	StateProtected   State = "protected"
	StateOutOfDate   State = "out-of-date"
	StateUnscheduled State = "unscheduled"
	StateCopyGap     State = "copy-gap"
	StatePartial     State = "partial"
	StateFailed      State = "failed"
	StateNever       State = "never"
)

func (a accountView) State() State {
	if a.Running {
		return StateWorking
	}
	return a.settled()
}

// settled is the same judgement with work in flight left out: what this
// account has stored right now, whether or not another backup is on its
// way to it. Coverage is counted from this rather than from State, so
// that asking for a backup of every account does not read, one second
// later, as every account having lost the copy it already had.
func (a accountView) settled() State {
	switch {
	case a.LastBackup == nil:
		return StateNever
	case a.LastStatus == job.StatusFailed:
		return StateFailed
	case a.LastStatus == job.StatusPartialSuccess:
		return StatePartial
	case len(a.MissingCopies) > 0:
		return StateCopyGap
	case a.ExpectedEvery == 0:
		// It has a good copy and nothing will ever take another.
		return StateUnscheduled
	case time.Since(*a.LastBackup) > a.overdueAfter():
		return StateOutOfDate
	default:
		return StateProtected
	}
}

// Current reports whether this account has a backup worth relying on: the
// last run succeeded, and a schedule has produced one since it was due.
func (a accountView) Current() bool { return a.settled() == StateProtected }

// overdueAfter is how long without a good backup counts as out of date:
// what the schedule covering this account asked for, or two of its own
// runs, which is the smallest gap a slow night cannot explain.
func (a accountView) overdueAfter() time.Duration {
	if a.AlertAfter > 0 {
		return a.AlertAfter
	}
	return 2 * a.ExpectedEvery
}

// Why explains the state in the words the operator needs, shown under the
// pill rather than left to be inferred from a colour.
func (a accountView) Why() string {
	switch a.State() {
	case StateProtected:
		return ""
	case StateOutOfDate:
		return "the schedule has not produced one since"
	case StateUnscheduled:
		return "no schedule covers it, so it will not be taken again"
	case StatePartial:
		return "some files could not be read"
	case StateCopyGap:
		return "a scheduled destination has no recent successful copy"
	case StateFailed:
		if a.LastError != "" {
			return a.LastError
		}
		return "the last run failed"
	case StateNever:
		return "nothing has ever been backed up"
	}
	return ""
}

// shortID is a repository id an operator can still recognise, for the
// case where its destination has been removed since.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// failureOf is why a job failed, taken from wherever it went wrong: the
// staging step, which stops the job before restic runs, or the first
// destination that refused it.
func failureOf(stored nodestore.Job) string {
	if stored.Status != job.StatusFailed {
		return ""
	}
	if stored.StagingErr != "" {
		return stored.StagingErr
	}
	for _, target := range stored.Targets {
		if target.Error != "" {
			return target.Error
		}
	}
	return ""
}

// cost is what one backup came to: the bytes it added across the
// destinations that took it, and how long the run took from end to end.
//
// Added bytes rather than processed: what an operator is paying for is
// what restic actually had to store, which on an unchanged account is a
// small fraction of what it read.
func cost(stored nodestore.Job) (uint64, time.Duration) {
	var bytes uint64
	var targets time.Duration
	for _, target := range stored.Targets {
		if target.Status != job.TargetSuccess {
			continue
		}
		bytes += target.BytesAdded
		targets += time.Duration(target.DurationSecs * float64(time.Second))
	}
	if stored.StartedAt != nil && stored.FinishedAt != nil {
		// The wall clock covers staging too, which is most of the time
		// spent on a small account.
		if wall := stored.FinishedAt.Sub(*stored.StartedAt); wall > 0 {
			return bytes, wall
		}
	}
	return bytes, targets
}

// longRunThreshold is how long a single run may take before it is worth
// saying so, taken from the shortest any schedule asked for.
func (s *Server) longRunThreshold() (time.Duration, error) {
	policies, err := s.engine.Store().Policies()
	if err != nil {
		return 0, err
	}
	threshold := 6 * time.Hour
	for _, policy := range policies {
		if policy.AlertRunHours > 0 {
			if asked := time.Duration(policy.AlertRunHours) * time.Hour; asked < threshold {
				threshold = asked
			}
		}
	}
	return threshold, nil
}

// expectedIntervals is how often each account is backed up, by the
// shortest interval among the enabled schedules covering it.
//
// The interval is read off the cron expression by asking it when it fires
// twice, rather than by interpreting the fields: the parser is the
// authority on what an expression means.
func (s *Server) expectedIntervals(accounts []panel.AccountInfo) (map[string]time.Duration, map[string]time.Duration, error) {
	policies, err := s.engine.Store().Policies()
	if err != nil {
		return nil, nil, err
	}
	intervals := map[string]time.Duration{}
	// How long without a good backup is worth saying out loud. A schedule
	// that named a number means it; one that did not means two of its own
	// runs, which is the smallest gap that cannot be explained by a slow
	// night.
	alerts := map[string]time.Duration{}
	now := time.Now()
	for _, policy := range policies {
		if !policy.Enabled || len(policy.RepositoryIDs) == 0 {
			continue
		}
		schedule, err := cron.ParseStandard(policy.ScheduleCron)
		if err != nil {
			continue
		}
		first := schedule.Next(now)
		interval := schedule.Next(first).Sub(first)
		if interval <= 0 {
			continue
		}

		covered := policy.Accounts
		if policy.AllAccounts() {
			covered = nil
			for _, account := range accounts {
				covered = append(covered, account.User)
			}
		}
		alertAfter := 2 * interval
		if policy.AlertNoBackupDays > 0 {
			alertAfter = time.Duration(policy.AlertNoBackupDays) * 24 * time.Hour
		}
		for _, name := range covered {
			if current, seen := intervals[name]; !seen || interval < current {
				intervals[name] = interval
			}
			if current, seen := alerts[name]; !seen || alertAfter < current {
				alerts[name] = alertAfter
			}
		}
	}
	return intervals, alerts, nil
}

// expectedTargetIntervals says how fresh each promised destination copy
// must be for each account. It is per target rather than per account because
// a daily local copy and a weekly off-site copy have different due dates.
func (s *Server) expectedTargetIntervals(accounts []panel.AccountInfo) (map[string]map[string]time.Duration, error) {
	policies, err := s.engine.Store().Policies()
	if err != nil {
		return nil, err
	}
	expected := map[string]map[string]time.Duration{}
	now := time.Now()
	for _, policy := range policies {
		if !policy.Enabled || len(policy.RepositoryIDs) == 0 {
			continue
		}
		schedule, err := cron.ParseStandard(policy.ScheduleCron)
		if err != nil {
			continue
		}
		first := schedule.Next(now)
		interval := schedule.Next(first).Sub(first)
		if interval <= 0 {
			continue
		}
		due := 2 * interval
		if policy.AlertNoBackupDays > 0 {
			due = time.Duration(policy.AlertNoBackupDays) * 24 * time.Hour
		}
		covered := policy.Accounts
		if policy.AllAccounts() {
			covered = make([]string, 0, len(accounts))
			for _, account := range accounts {
				covered = append(covered, account.User)
			}
		}
		for _, account := range covered {
			if expected[account] == nil {
				expected[account] = map[string]time.Duration{}
			}
			for _, repositoryID := range policy.RepositoryIDs {
				if current, exists := expected[account][repositoryID]; !exists || due < current {
					expected[account][repositoryID] = due
				}
			}
		}
	}
	return expected, nil
}

func (s *Server) accountViews(r *http.Request) ([]accountView, []string, error) {
	accounts, err := s.engine.Accounts(r.Context())
	if err != nil {
		// A server whose accounts cannot be listed is still worth showing
		// a page for, with the reason on it.
		return nil, []string{"Could not list cPanel accounts: " + err.Error()}, nil
	}
	jobs, err := s.engine.Store().Jobs(0)
	if err != nil {
		return nil, nil, err
	}
	stuckAfter, err := s.longRunThreshold()
	if err != nil {
		return nil, nil, err
	}
	var warnings []string

	// How often each account is due a backup, taken from the schedules
	// that actually cover it. An account no schedule covers has no
	// expectation to fall behind, which is its own kind of exposure.
	expected, alertAfter, err := s.expectedIntervals(accounts)
	if err != nil {
		return nil, nil, err
	}
	expectedTargets, err := s.expectedTargetIntervals(accounts)
	if err != nil {
		return nil, nil, err
	}
	policies, err := s.engine.Store().Policies()
	if err != nil {
		return nil, nil, err
	}
	accountNames := make([]string, 0, len(accounts))
	for _, account := range accounts {
		accountNames = append(accountNames, account.User)
	}
	removalSafeties, err := s.engine.AccountRemovalSafeties(accountNames, time.Now())
	if err != nil {
		return nil, nil, err
	}

	latest := map[string]nodestore.Job{}
	running := map[string]bool{}
	progress := map[string]*nodestore.JobProgress{}
	runs := map[string]int{}
	succeeded := map[string]int{}
	for _, stored := range jobs {
		if !stored.Status.Terminal() {
			running[stored.Account] = true
			if stored.Progress != nil {
				progress[stored.Account] = stored.Progress
			}
			// A run still going long after it should have finished is
			// stuck more often than it is slow, and nothing else here
			// would ever say so.
			if stored.StartedAt != nil && time.Since(*stored.StartedAt) > stuckAfter {
				warnings = append(warnings, fmt.Sprintf(
					"The backup of %s has been running for %s. A run that long is usually stuck.",
					stored.Account, humanUntil(time.Since(*stored.StartedAt))))
			}
			continue
		}
		runs[stored.Account]++
		if stored.Status == job.StatusSuccess {
			succeeded[stored.Account]++
		}
		if previous, seen := latest[stored.Account]; !seen || stored.QueuedAt.After(previous.QueuedAt) {
			latest[stored.Account] = stored
		}
	}

	// Destination names, so a count can say where the copies went rather
	// than quoting a repository id.
	destinations, err := s.destinationViews()
	if err != nil {
		return nil, nil, err
	}
	names := map[string]string{}
	for _, dest := range destinations {
		names[dest.Repository.ID] = dest.Name
	}
	copies := map[string]map[string]int{}
	lastTargetSuccess := map[string]map[string]time.Time{}
	for _, stored := range jobs {
		if !stored.Status.Terminal() {
			continue
		}
		for _, target := range stored.Targets {
			if target.Status != job.TargetSuccess {
				continue
			}
			name := names[target.RepositoryID]
			if name == "" {
				name = shortID(target.RepositoryID)
			}
			if copies[stored.Account] == nil {
				copies[stored.Account] = map[string]int{}
			}
			copies[stored.Account][name]++
			at := stored.QueuedAt
			if stored.FinishedAt != nil {
				at = *stored.FinishedAt
			}
			if lastTargetSuccess[stored.Account] == nil {
				lastTargetSuccess[stored.Account] = map[string]time.Time{}
			}
			if at.After(lastTargetSuccess[stored.Account][target.RepositoryID]) {
				lastTargetSuccess[stored.Account][target.RepositoryID] = at
			}
		}
	}

	// A rehearsal is the only thing that says a backup rebuilds, so when
	// one last ran belongs beside the account it was run for.
	restores, err := s.engine.Store().Restores(0)
	if err != nil {
		return nil, nil, err
	}
	type drill struct {
		at      time.Time
		ok      bool
		skipped []string
	}
	verified := map[string]drill{}
	for _, restore := range restores {
		if restore.Kind != node.KindVerify || restore.FinishedAt == nil {
			continue
		}
		if previous, seen := verified[restore.Account]; seen && previous.at.After(*restore.FinishedAt) {
			continue
		}
		verified[restore.Account] = drill{
			at: *restore.FinishedAt,
			// A rehearsal that passed on a backup taken without part of
			// the account is not this account verified. Saying it is
			// puts a tick beside a customer whose databases have never
			// been proved to come back.
			ok:      restore.Status == job.StatusSuccess && !restore.PartialSource,
			skipped: restore.SkippedParts,
		}
	}

	views := make([]accountView, 0, len(accounts))
	for _, account := range accounts {
		view := accountView{
			AccountInfo:   account,
			Running:       running[account.User],
			Progress:      progress[account.User],
			ExpectedEvery: expected[account.User],
			AlertAfter:    alertAfter[account.User],
			Runs:          runs[account.User],
			Succeeded:     succeeded[account.User],
		}
		if drilled, seen := verified[account.User]; seen {
			at := drilled.at
			view.Verified, view.VerifiedOK = &at, drilled.ok
			view.VerifiedSkipped = drilled.skipped
		}
		if last, seen := latest[account.User]; seen {
			view.Stored, view.Took = cost(last)
			view.LastError = failureOf(last)
		}
		for name, written := range copies[account.User] {
			view.Copies = append(view.Copies, copyCount{Destination: name, Written: written})
		}
		view.FreshCopies, view.MissingCopies = targetCoverage(
			expectedTargets[account.User], lastTargetSuccess[account.User], names, time.Now())
		missingIDs := map[string]bool{}
		now := time.Now()
		for repositoryID, due := range expectedTargets[account.User] {
			last, exists := lastTargetSuccess[account.User][repositoryID]
			if !exists || now.Sub(last) > due {
				missingIDs[repositoryID] = true
			}
		}
		view.RepairPolicyID, view.RepairPolicyName = repairPolicy(policies, account.User, missingIDs)
		if decision := removalSafeties[account.User]; decision.Enforced {
			copy := decision
			view.RemovalSafety = &copy
		}
		sort.Slice(view.Copies, func(i, j int) bool {
			return view.Copies[i].Destination < view.Copies[j].Destination
		})
		if last, seen := latest[account.User]; seen {
			view.LastBackup = last.FinishedAt
			view.LastStatus = last.Status
			// A listing does not measure sizes, but a completed backup
			// did, so show what it read.
			for _, target := range last.Targets {
				if target.BytesProcessed > view.SizeBytes {
					view.SizeBytes = target.BytesProcessed
				}
			}
		}
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].User < views[j].User })
	// After the sort, so that the accounts a warning names are in the
	// order the page below it lists them.
	warnings = append(warnings, failureWarnings(views)...)
	return views, warnings, nil
}

func targetCoverage(expected map[string]time.Duration, successful map[string]time.Time,
	names map[string]string, now time.Time) (fresh, missing []string) {
	for repositoryID, due := range expected {
		name := names[repositoryID]
		if name == "" {
			name = shortID(repositoryID)
		}
		last, exists := successful[repositoryID]
		if !exists || now.Sub(last) > due {
			missing = append(missing, name)
		} else {
			fresh = append(fresh, name)
		}
	}
	sort.Strings(fresh)
	sort.Strings(missing)
	return fresh, missing
}

func repairPolicy(policies []nodestore.Policy, account string, missing map[string]bool) (id, name string) {
	best := 0
	for _, policy := range policies {
		if !policy.Enabled || len(policy.RepositoryIDs) == 0 {
			continue
		}
		covered := policy.AllAccounts()
		for _, selected := range policy.Accounts {
			if selected == account {
				covered = true
				break
			}
		}
		if !covered {
			continue
		}
		score := 0
		for _, repositoryID := range policy.RepositoryIDs {
			if missing[repositoryID] {
				score++
			}
		}
		if score > best {
			best, id, name = score, policy.ID, policy.Name
		}
	}
	return id, name
}

// preferredBackupPolicy chooses what the generic "Back up" button means.
// The old behavior picked the first policy alphabetically, even when it was
// disabled, had no destination, or covered somebody else. Prefer a complete
// payload, then the policy that writes the most copies.
func preferredBackupPolicy(policies []nodestore.Policy, account string, allOnly bool) (nodestore.Policy, bool) {
	var selected nodestore.Policy
	selectedFull := false
	for _, policy := range policies {
		if !policy.Enabled || len(policy.RepositoryIDs) == 0 {
			continue
		}
		covered := policy.AllAccounts()
		if !allOnly && !covered {
			for _, name := range policy.Accounts {
				if name == account {
					covered = true
					break
				}
			}
		}
		if !covered {
			continue
		}
		full := !policy.SkipHomedir && !policy.SkipDatabases && !policy.SkipEmail
		if selected.ID == "" || full && !selectedFull ||
			full == selectedFull && len(policy.RepositoryIDs) > len(selected.RepositoryIDs) {
			selected, selectedFull = policy, full
		}
	}
	return selected, selected.ID != ""
}

// handleAccounts lists what this server has, using local state only.
//
// Nothing here talks to a backup destination: listing snapshots means a
// round trip per repository, and doing that for every account would make
// the page as slow as the account walk this replaced. Snapshots are read
// when one account is opened.
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, warnings, err := s.accountViews(r)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	policies, err := s.engine.Store().Policies()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	view := struct {
		Accounts    []accountView
		RunAll      *nodestore.Policy
		Warnings    []string
		Protected   int
		Unprotected int
	}{Accounts: accounts, Warnings: warnings}
	if policy, ok := preferredBackupPolicy(policies, "", true); ok {
		view.RunAll = &policy
	}
	for _, account := range accounts {
		switch {
		case account.Current():
			view.Protected++
		case account.LastBackup == nil:
			view.Unprotected++
		}
	}
	s.render(w, r, "accounts.html", "Accounts", "accounts", view)
}

// activityRow is one thing that happened to an account.
type activityRow struct {
	When       time.Time
	What       string
	Status     job.Status
	Note       string
	Log        string
	Download   string
	Incomplete bool
}

func (a activityRow) Stripe() string {
	switch a.Status {
	case job.StatusSuccess:
		return "cpr-s-ok"
	case job.StatusFailed:
		return "cpr-s-bad"
	default:
		return "cpr-s-warn"
	}
}

// handleAccount shows one account: its snapshots, its rehearsals and what
// has happened to it.
//
// This is the page that reads from the backup destination, and it does so
// for one account only — which is why it is a page you open rather than
// something folded into the list.
func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	user := r.URL.Query().Get("user")
	accounts, _, err := s.accountViews(r)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	var account accountView
	found := false
	for _, candidate := range accounts {
		if candidate.User == user {
			account, found = candidate, true
			break
		}
	}
	if !found {
		s.redirect(w, r, "/accounts", "error", "There is no account called "+user+" on this server.")
		return
	}

	view := struct {
		Account        accountView
		Snapshots      []resticrun.Snapshot
		RepositoryID   string
		RepositoryName string
		LookupError    string
		Activity       []activityRow
		LastDrill      *nodestore.Restore
	}{Account: account}

	// Read this account's snapshots from the first provisioned
	// destination. One account, one round trip.
	destinations, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	for _, destination := range destinations {
		if destination.Repository.InitialisedAt == nil {
			continue
		}
		view.RepositoryID = destination.Repository.ID
		view.RepositoryName = destination.Name
		snapshots, err := s.engine.Snapshots(r.Context(), destination.Repository.ID, user)
		if err != nil {
			view.LookupError = err.Error()
			break
		}
		sort.Slice(snapshots, func(i, j int) bool {
			return snapshots[i].Time.After(snapshots[j].Time)
		})
		view.Snapshots = snapshots
		break
	}

	names := map[string]string{}
	for _, destination := range destinations {
		names[destination.Repository.ID] = destination.Name
	}

	jobs, err := s.engine.Store().Jobs(0)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	for _, stored := range jobs {
		if stored.Account != user {
			continue
		}
		when := stored.QueuedAt
		if stored.FinishedAt != nil {
			when = *stored.FinishedAt
		}
		for _, target := range stored.Targets {
			row := activityRow{
				When:       when,
				What:       "Backed up to " + fallback(names[target.RepositoryID], "a destination"),
				Status:     stored.Status,
				Log:        target.Detail,
				Incomplete: target.Incomplete,
			}
			switch {
			case target.Error != "":
				row.Note = target.Error
			case target.Status == job.TargetSuccess:
				row.Note = fmt.Sprintf("%s stored of %s read",
					humanBytes(target.BytesAdded), humanBytes(target.BytesProcessed))
			}
			view.Activity = append(view.Activity, row)
		}
		if len(stored.Targets) == 0 {
			view.Activity = append(view.Activity, activityRow{
				When: when, What: "Backup", Status: stored.Status, Note: stored.StagingErr,
			})
		}
	}

	restores, err := s.engine.Store().Restores(0)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	for i := range restores {
		restore := restores[i]
		if restore.Account != user {
			continue
		}
		when := restore.QueuedAt
		if restore.FinishedAt != nil {
			when = *restore.FinishedAt
		}
		row := activityRow{When: when, Status: restore.Status, Note: restore.Error}
		switch restore.Kind {
		case node.KindVerify:
			row.What = "Restore rehearsed"
			if restore.Detail != "" {
				row.Note = restore.Detail
			}
			if view.LastDrill == nil && restore.Status.Terminal() {
				view.LastDrill = &restores[i]
			}
		case protocol.RestoreFiles:
			row.What = "Files recovered"
			if restore.RestoredTo != "" {
				row.Note = "into " + restore.RestoredTo
			}
		default:
			row.What = "Rebuilt for download"
			if restore.Applied {
				row.What = "Restored over the live account"
			}
			if restore.ArchivePath != "" && !restore.Applied {
				if onDisk(restore.ArchivePath) {
					row.Download = restore.ID
					row.Note = ""
				} else {
					row.Note = "no longer on this server"
				}
			}
		}
		view.Activity = append(view.Activity, row)
	}

	sort.Slice(view.Activity, func(i, j int) bool {
		return view.Activity[i].When.After(view.Activity[j].When)
	})

	s.render(w, r, "account.html", account.User, "accounts", view)
}

func fallback(value, whenEmpty string) string {
	if value == "" {
		return whenEmpty
	}
	return value
}

func (s *Server) handleBackupNow(w http.ResponseWriter, r *http.Request) {
	account := r.PostFormValue("account")
	policyID := r.PostFormValue("policy")
	if policyID == "" {
		policies, err := s.engine.Store().Policies()
		if err != nil {
			s.redirect(w, r, "/accounts", "error", err.Error())
			return
		}
		policy, ok := preferredBackupPolicy(policies, account, false)
		if !ok {
			s.redirect(w, r, "/accounts", "error",
				"Create or enable a schedule that covers this account and has a destination.")
			return
		}
		policyID = policy.ID
	}
	if _, err := s.engine.QueueCoverageRepair(policyID, account); err != nil {
		s.redirect(w, r, "/accounts", "error", err.Error())
		return
	}
	s.redirect(w, r, "/accounts", "ok", "Backup of "+account+" queued.")
}

func (s *Server) handleRepairCoverage(w http.ResponseWriter, r *http.Request) {
	account := r.PostFormValue("account")
	if _, err := s.engine.QueueCoverageRepair(r.PostFormValue("policy"), account); err != nil {
		s.redirect(w, r, "/accounts", "error", err.Error())
		return
	}
	s.redirect(w, r, "/accounts", "ok", "Coverage repair for "+account+" queued.")
}

func (s *Server) handlePrepareRemoval(w http.ResponseWriter, r *http.Request) {
	account := r.PostFormValue("account")
	policies, err := s.engine.QueueRemovalPreparation(account, time.Now())
	target := "/accounts"
	if r.PostFormValue("detail") == "1" {
		target = "/account?user=" + url.QueryEscape(account)
	}
	if err != nil {
		s.redirect(w, r, target, "error", err.Error())
		return
	}
	names := make([]string, 0, len(policies))
	for _, policy := range policies {
		names = append(names, policy.Name)
	}
	message := "Queued " + strings.Join(names, ", ") + " to prepare " + account + " for safe termination."
	if len(policies) > 1 {
		message += " The backups will run sequentially."
	}
	s.redirect(w, r, target, "ok", message)
}

// --- restore ---

// restoreRow is a restore as the history shows it: the record, plus
// whether what it produced is still on this server.
//
// Output does not live for ever — it is swept once nobody has collected
// it, and a newer restore of the same account supersedes an older one — so
// whether a download still works is a question about the disk, asked when
// the page is rendered rather than assumed from the record.
type restoreRow struct {
	nodestore.Restore
	Collectable bool
}

// Parts names what a restore asked for, in the operator's own vocabulary.
// A basket names each of its parts; a whole-account restore names itself.
func (r restoreRow) Parts() string {
	selections := r.Selections()
	if len(selections) == 0 {
		return r.Kind
	}
	kinds := make([]string, 0, len(selections))
	for _, selection := range selections {
		kinds = append(kinds, selection.Kind)
	}
	return strings.Join(kinds, ", ")
}

// handleForgetAccount removes a deleted account's backups and everything
// this server remembers about it.
//
// Irreversible, and the only thing in the program that deletes backups
// outright, so it takes a confirmation the page carries rather than one the
// browser asks for: the interface works with scripting switched off, and a
// window.confirm nobody saw is no confirmation at all.
func (s *Server) handleForgetAccount(w http.ResponseWriter, r *http.Request) {
	back := "?p=restore&tab=deleted"
	account := r.PostFormValue("account")
	if account == "" {
		s.redirect(w, r, back, "error", "No account was named.")
		return
	}
	if r.PostFormValue("confirm") != "1" {
		s.redirect(w, r, back, "error",
			"Tick the box to confirm: this deletes the backups themselves.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	removed, err := s.engine.ForgetAccountBackups(ctx, account)
	if err != nil {
		s.log.Error("forget a deleted account", "account", account, "error", err)
		s.redirect(w, r, back, "error", err.Error())
		return
	}
	s.redirect(w, r, back, "ok", fmt.Sprintf(
		"%s is forgotten: %d backup(s) removed.", account, removed))
}

// usableItemNames checks the names chosen for a granular restore.
//
// They reach a command line, a file name and the list of members taken out
// of the account's archive. Each is checked again further down; they are
// checked here because this is where they arrive from a browser, and a
// domain carrying a slash would select members belonging to a different
// part of the account.
func usableItemNames(kind granular.Kind, names []string) error {
	for _, name := range names {
		var err error
		switch kind {
		case granular.KindDatabase:
			err = granular.UsableDatabaseName(name)
		case granular.KindDBUsers:
			err = granular.UsableDatabaseUserName(name)
		case granular.KindDNS, granular.KindSSL, granular.KindDomains:
			err = granular.UsableDomainName(name)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// collectableRows pairs each restore with the state of what it left behind.
func collectableRows(restores []nodestore.Restore) []restoreRow {
	rows := make([]restoreRow, 0, len(restores))
	for _, restore := range restores {
		rows = append(rows, restoreRow{
			Restore:     restore,
			Collectable: restore.ArchivePath != "" && onDisk(restore.ArchivePath),
		})
	}
	return rows
}

// onDisk reports whether a path is still a readable file.
func onDisk(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

type restoreView struct {
	// Tab is which of the three restore views is showing: "account",
	// "deleted" or "server". The server view renders from recover.html,
	// so this field only ever holds the first two.
	Tab      string
	Accounts []accountView
	// Deleted are accounts cPanel has removed whose backups are still
	// here, offered in their own group so a deleted account is something
	// an operator can find rather than something the picker forgets.
	Deleted []deletedAccount
	// AccountDeleted says the chosen account is one of those, which
	// changes what a restore of it means: cPanel creates the account.
	AccountDeleted bool
	// Forgetting is a deleted account the operator has asked to forget,
	// and the page is showing what that would do before doing it. The
	// confirmation is here rather than in the browser because this
	// deletes backups and the page has to work with scripting off.
	Forgetting string
	// Contents is every account the chosen destination holds, which is
	// not the same list as the accounts on this server: it includes ones
	// deleted here and ones that were never here at all.
	Contents     []storedAccount
	ContentsErr  string
	Repositories []destinationView
	Account      string
	RepositoryID string
	Snapshots    []resticrun.Snapshot
	Restores     []restoreRow
	LookupError  string

	// The file picker for a whole-account restore: what the chosen
	// snapshot holds at the path being looked at, so files are picked
	// rather than typed from memory.
	FileSnapshot string
	FilePath     string
	FileUp       string
	FileEntries  []browseEntry
	FileErr      string

	// Granular restore: which part of the account is being picked, from
	// which snapshot, and what that snapshot holds at the path being
	// looked at. Populated only when a kind is chosen, so listing a
	// snapshot costs nothing until someone asks.
	Kinds      []granular.Kind
	Kind       granular.Kind
	SnapshotID string
	Path       string
	Up         string
	Entries    []browseEntry
	BrowseErr  string
	// Items is what this part of the account holds, for the parts that
	// are not files and so cannot be listed by browsing the snapshot.
	Items []inventory.Item

	// Basket is what has been chosen out of this backup so far, across
	// all of its parts.
	Basket nodestore.Basket
}

// BasketRows is the basket in the order the parts are offered.
func (v restoreView) BasketRows() []basketRow {
	return basketRows(v.Basket, granular.Kinds, v.KindTitle)
}

// BasketCount is how many things are in the basket.
func (v restoreView) BasketCount() int { return v.Basket.Count() }

// BasketCanApply reports whether all of it can go back into the account.
func (v restoreView) BasketCanApply() bool { return basketCanApply(v.Basket) }

// BasketBlocker names the parts that keep it from being put back.
func (v restoreView) BasketBlocker() string {
	return basketBlocker(v.Basket, granular.Kinds, v.KindTitle)
}

// InBasket reports whether a part has already been chosen.
func (v restoreView) InBasket(kind granular.Kind) bool { return inBasket(v.Basket, kind) }

// PicksItems reports whether the listed items can be chosen between,
// rather than only read.
func (v restoreView) PicksItems() bool { return v.Kind.PicksItems() }

// ItemsNote says what the list under a kind means, which differs by
// whether choosing narrows the restore or only says what is in it.
func (v restoreView) ItemsNote() string {
	if v.Kind.PicksItems() {
		return "Tick what you want. Leave every box empty to take all of them."
	}
	if v.Kind == granular.KindCron {
		return "What this backup holds. Putting them back replaces the account's " +
			"crontab with this one: an account's jobs are lines in a single file, " +
			"so a job added since this backup was taken goes. A copy of the " +
			"crontab being replaced is kept on the server first."
	}
	return "What this backup holds. They come out together: a crontab and the FTP " +
		"logins are each one file, and handing back part of one would hand back a " +
		"file that is not the one in the backup."
}

// browseEntry is one line of a snapshot, as the picker shows it.
type browseEntry struct {
	Name string
	Path string
	Size uint64
	Dir  bool
	// Name of the item this entry would restore, in the form the kind
	// expects: a path for files, "domain/box" for a mailbox, a bare name
	// for a database.
	Item string
}

// KindTitle lets the template name a kind without knowing the package.
func (v restoreView) KindTitle(k granular.Kind) string { return k.Title() }

// Selected reports whether a kind is the one being picked.
func (v restoreView) Selected(k granular.Kind) bool { return v.Kind == k }

// storedAccount is one account a destination holds, and what this server
// knows about it now.
//
// The three states used to be three places to look: a tab for the accounts
// cPanel no longer has, and a page for the ones it never had. They are one
// list with the state on the row, because "which of these is gone?" is a
// question about a list and not a reason to make somebody navigate.
type storedAccount struct {
	node.AccountBackups
	// State is "live", "deleted" or "elsewhere": on this server, removed
	// from it, or belonging to a server that is not this one.
	State string
	// RetiredAt is when cPanel lost it, for the ones it lost.
	RetiredAt time.Time
}

// Gone reports whether this account is not on the server, which is what
// tints the row.
func (a storedAccount) Gone() bool { return a.State != "live" }

// StateTitle names the state in the words the page uses.
func (a storedAccount) StateTitle() string {
	switch a.State {
	case "deleted":
		return "deleted"
	case "elsewhere":
		return "another server"
	}
	return ""
}

// storedAccounts says, for each account a destination holds, whether this
// server still has it.
func storedAccounts(held []node.AccountBackups, live []accountView,
	deleted []deletedAccount) []storedAccount {

	here := make(map[string]bool, len(live))
	for _, account := range live {
		here[account.User] = true
	}
	retired := make(map[string]time.Time, len(deleted))
	for _, account := range deleted {
		retired[account.Account] = account.RetiredAt
	}

	rows := make([]storedAccount, 0, len(held)+len(deleted))
	listed := make(map[string]bool, len(held))
	for _, account := range held {
		row := storedAccount{AccountBackups: account, State: "elsewhere"}
		switch {
		case here[account.Account]:
			row.State = "live"
		case !retired[account.Account].IsZero():
			row.State, row.RetiredAt = "deleted", retired[account.Account]
		}
		listed[account.Account] = true
		rows = append(rows, row)
	}
	// A deleted account this server knows about but the destination did
	// not name. Reading a destination needs restic and a network; the
	// records here need neither, and an account nobody can see is one
	// nobody can restore or forget.
	for _, account := range deleted {
		if listed[account.Account] {
			continue
		}
		rows = append(rows, storedAccount{
			AccountBackups: node.AccountBackups{
				Account: account.Account, Latest: account.LastBackup,
			},
			State: "deleted", RetiredAt: account.RetiredAt,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Account < rows[j].Account })
	return rows
}

// deletedAccount is an account cPanel no longer has, whose backups this
// server still holds. JetBackup calls these orphans; this page calls them
// deleted accounts, because that is what happened — the account went, the
// backups stayed, and restoring one asks cPanel to create it again.
type deletedAccount struct {
	Account   string
	RetiredAt time.Time
	// LastBackup is the newest backup this server recorded for the name,
	// and the reason the account is offered at all: a name with nothing
	// behind it would be a dead end in the picker.
	LastBackup time.Time
}

// deletedAccounts lists the names this server retired and still has backups
// for. It reads what the lifecycle hooks recorded rather than the repository
// itself: reading the destination is authoritative, and slow enough that
// every visit to this page would wait on restic. The whole-server view is
// the one that reads a destination directly, which is also where an account
// this server never knew — one from a different host — shows up.
func (s *Server) deletedAccounts(live []accountView) ([]deletedAccount, error) {
	identities, err := s.engine.Store().Identities()
	if err != nil {
		return nil, err
	}
	jobs, err := s.engine.Store().Jobs(0)
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(live))
	for _, account := range live {
		present[account.User] = true
	}
	// The newest backup that finished with something in it, per name.
	backed := map[string]time.Time{}
	for _, run := range jobs {
		if run.FinishedAt == nil {
			continue
		}
		if run.Status != job.StatusSuccess && run.Status != job.StatusPartialSuccess {
			continue
		}
		if at, ok := backed[run.Account]; !ok || run.FinishedAt.After(at) {
			backed[run.Account] = *run.FinishedAt
		}
	}

	var deleted []deletedAccount
	for _, identity := range identities {
		if identity.RetiredAt == nil {
			continue
		}
		// The name is on the server again. Whoever holds it now owns the
		// live row in the other group, and the previous holder's backups
		// are not theirs to be offered under the same name here.
		if present[identity.Account] {
			continue
		}
		last, ok := backed[identity.Account]
		if !ok {
			continue
		}
		deleted = append(deleted, deletedAccount{
			Account:    identity.Account,
			RetiredAt:  *identity.RetiredAt,
			LastBackup: last,
		})
	}
	// Most recently deleted first: the account someone is looking for is
	// usually the one that just went.
	sort.Slice(deleted, func(i, j int) bool {
		return deleted[i].RetiredAt.After(deleted[j].RetiredAt)
	})
	return deleted, nil
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	// Recovering a whole server is a restore, and used to be its own page
	// in the rail. It is the third tab here; the old address still works.
	if r.URL.Query().Get("tab") == "server" {
		s.handleRecover(w, r)
		return
	}
	accounts, _, err := s.accountViews(r)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	destinations, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	restores, err := s.engine.Store().Restores(10)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	deleted, err := s.deletedAccounts(accounts)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	// One list, with the state on the row. "Deleted accounts" was a tab of
	// its own until the same question -- which of these is gone? -- turned
	// out to be about a list rather than a reason to navigate. The old
	// address still works and lands on the list.
	tab := "account"
	view := restoreView{
		Tab:      tab,
		Accounts: accounts, Deleted: deleted, Repositories: destinations,
		Restores:     collectableRows(restores),
		Account:      r.URL.Query().Get("account"),
		RepositoryID: r.URL.Query().Get("repository"),
	}
	for _, gone := range deleted {
		if gone.Account == view.Account {
			view.AccountDeleted = true
		}
		// Only a name in this list. Asking to forget something else --
		// a live account, a name off the end of a query string -- is
		// not something to render a confirmation for.
		if gone.Account == r.URL.Query().Get("forget") {
			view.Forgetting = gone.Account
		}
	}
	// Default to the first provisioned repository so the common case is
	// one click rather than two.
	if view.RepositoryID == "" {
		for _, dest := range destinations {
			if dest.Repository.InitialisedAt != nil {
				view.RepositoryID = dest.Repository.ID
				break
			}
		}
	}
	// What the chosen destination holds, so several accounts can be picked
	// off one list. It is read from the repository rather than from this
	// server's records, because the whole point of restoring is that this
	// server's records may be what was lost.
	// What the chosen destination holds, and what this server remembers
	// about accounts it has lost. Reading a destination needs restic and
	// a network; the records here need neither, and an account nobody can
	// see is one nobody can restore or forget -- so the list is built
	// either way, and says when it is the smaller of the two.
	if view.RepositoryID != "" || len(deleted) > 0 {
		var held []node.AccountBackups
		if view.RepositoryID != "" {
			contents, err := s.engine.Contents(r.Context(), view.RepositoryID)
			if err != nil {
				view.ContentsErr = err.Error()
			}
			held = contents.Accounts
		}
		view.Contents = storedAccounts(held, accounts, deleted)
	}
	if view.Account != "" && view.RepositoryID != "" {
		snapshots, err := s.engine.Snapshots(r.Context(), view.RepositoryID, view.Account)
		if err != nil {
			view.LookupError = err.Error()
		} else {
			// Newest first: the snapshot someone wants is almost always
			// the most recent one.
			sort.Slice(snapshots, func(i, j int) bool {
				return snapshots[i].Time.After(snapshots[j].Time)
			})
			view.Snapshots = snapshots
		}
	}

	// The picker over the chosen snapshot. It is only listed when a
	// snapshot is chosen, and only one directory at a time.
	if len(view.Snapshots) > 0 {
		view.FileSnapshot = r.URL.Query().Get("files")
		if view.FileSnapshot != "" {
			s.fillFilePicker(r, &view)
		}
	}

	view.Kinds = granular.Kinds
	view.Kind = granular.Kind(r.URL.Query().Get("item"))
	view.SnapshotID = r.URL.Query().Get("snapshot")
	if view.SnapshotID == "" && len(view.Snapshots) > 0 {
		view.SnapshotID = view.Snapshots[0].ID
	}
	if view.Kind != "" && view.SnapshotID != "" {
		s.fillPicker(r, &view)
	}
	if view.Account != "" && view.RepositoryID != "" && view.SnapshotID != "" {
		basket, err := s.engine.Store().Basket(nodestore.BasketOfOperator, view.Account, view.RepositoryID, view.SnapshotID)
		if err != nil {
			s.log.Error("read the recovery basket", "account", view.Account, "error", err)
		} else {
			view.Basket = basket
		}
	}
	s.render(w, r, "restore.html", "Restore", "restore", view)
}

// fillFilePicker lists one directory of the snapshot being restored from,
// so specific files are picked out of what is actually in there rather
// than typed from memory.
func (s *Server) fillFilePicker(r *http.Request, view *restoreView) {
	snapshot, ok := findSnapshot(view.Snapshots, view.FileSnapshot)
	if !ok {
		view.FileErr = "That backup is not in this destination."
		return
	}
	parts, err := reassemble.Classify(snapshot.Paths)
	if err != nil {
		view.FileErr = err.Error()
		return
	}
	root := parts.Homedir
	if root == "" {
		view.FileErr = "This backup has no home directory in it to pick files from."
		return
	}

	view.FilePath = root
	if asked := path.Clean(r.URL.Query().Get("path")); asked != "." && asked != "" {
		if asked == root || strings.HasPrefix(asked, root+"/") {
			view.FilePath = asked
		}
	}
	view.FileUp = parentWithin(view.FilePath, root)

	entries, err := s.engine.Browse(r.Context(), view.RepositoryID, snapshot.ID, view.FilePath)
	if err != nil {
		view.FileErr = err.Error()
		return
	}
	for _, entry := range entries {
		if entry.Path == view.FilePath {
			continue
		}
		view.FileEntries = append(view.FileEntries, browseEntry{
			Name: entry.Name, Path: entry.Path, Size: entry.Size,
			Dir: entry.IsDir(), Item: entry.Path,
		})
	}
	sort.Slice(view.FileEntries, func(i, j int) bool {
		if view.FileEntries[i].Dir != view.FileEntries[j].Dir {
			return view.FileEntries[i].Dir
		}
		return view.FileEntries[i].Name < view.FileEntries[j].Name
	})
}

// fillPicker lists the part of the snapshot the chosen kind is picked
// from. Only kinds that need a name are listed: the rest restore one known
// thing and have nothing to choose.
func (s *Server) fillPicker(r *http.Request, view *restoreView) {
	snapshot, ok := findSnapshot(view.Snapshots, view.SnapshotID)
	if !ok {
		view.BrowseErr = "That backup is not in this destination."
		return
	}
	parts, err := reassemble.Classify(snapshot.Paths)
	if err != nil {
		view.BrowseErr = err.Error()
		return
	}

	var root string
	switch view.Kind {
	case granular.KindFiles:
		root = parts.Homedir
	case granular.KindMailbox:
		root = path.Join(parts.Homedir, "mail")
	case granular.KindDatabase:
		root = parts.Databases
	default:
		// The rest of an account is inside its archive or one file beside
		// the dumps, so what it holds is read rather than browsed.
		if view.Kind.ListsItems() {
			items, err := s.engine.Items(r.Context(), view.RepositoryID, snapshot.ID, parts, view.Kind)
			if err != nil {
				view.BrowseErr = err.Error()
				return
			}
			view.Items = items
		}
		return
	}
	if root == "" {
		view.BrowseErr = "This backup does not contain that part of the account."
		return
	}

	view.Path = view.Path0(r, root)
	entries, err := s.engine.Browse(r.Context(), view.RepositoryID, snapshot.ID, view.Path)
	if err != nil {
		view.BrowseErr = err.Error()
		return
	}
	view.Up = parentWithin(view.Path, root)
	if view.Kind == granular.KindMailbox && view.Path == root {
		// cPanel keeps the account's own mailbox directly in ~/mail and
		// every additional address under its domain, so the two are
		// offered separately rather than mixed into one listing.
		view.Entries = append(view.Entries, browseEntry{
			Name: "the account's own mailbox and every address on it",
			Path: root, Dir: false, Item: ".",
		})
	}
	for _, entry := range entries {
		// restic lists the directory itself alongside its contents.
		if entry.Path == view.Path {
			continue
		}
		if view.Kind == granular.KindMailbox && view.Path == root &&
			(!entry.IsDir() || maildirInternal(entry.Name)) {
			// The rest of ~/mail is the account's own maildir — folders,
			// index files, quota state — which is offered above as one
			// thing rather than a file at a time. Only domains are listed.
			continue
		}
		if view.Kind == granular.KindDatabase && databaseUsersPart(entry.Name) {
			// The account's database users are staged beside its
			// databases. They are their own recovery choice, and listing
			// them here — even unselectable — reads as three databases
			// nobody recognises.
			continue
		}
		view.Entries = append(view.Entries, browseEntry{
			Name: entry.Name,
			Path: entry.Path,
			Size: entry.Size,
			Dir:  entry.IsDir(),
			Item: itemName(view.Kind, entry, parts),
		})
	}
	sort.Slice(view.Entries, func(i, j int) bool {
		if view.Entries[i].Dir != view.Entries[j].Dir {
			return view.Entries[i].Dir
		}
		return view.Entries[i].Name < view.Entries[j].Name
	})
}

// Path0 keeps the browse path inside the root it was rooted at, so a
// crafted query cannot list another account's home directory.
func (v restoreView) Path0(r *http.Request, root string) string {
	asked := r.URL.Query().Get("path")
	if asked == "" {
		return root
	}
	clean := path.Clean(asked)
	if clean != root && !strings.HasPrefix(clean, root+"/") {
		return root
	}
	return clean
}

// itemName is what the restore form sends for an entry, which differs by
// kind: a database is a name, a mailbox is domain/box, a file is its path.
func itemName(kind granular.Kind, entry resticrun.Entry, parts reassemble.Parts) string {
	switch kind {
	case granular.KindDatabase:
		if entry.IsDir() || !strings.HasSuffix(entry.Name, ".sql") {
			return ""
		}
		// The account's database users are staged beside its databases
		// and are .sql too. They are their own recovery choice; offering
		// them here would name a database that does not exist.
		if databaseUsersPart(entry.Name) {
			return ""
		}
		return strings.TrimSuffix(entry.Name, ".sql")
	case granular.KindMailbox:
		mail := path.Join(parts.Homedir, "mail")
		rel := strings.TrimPrefix(entry.Path, mail+"/")
		if rel == entry.Path || rel == "" {
			return ""
		}
		return rel
	default:
		return entry.Path
	}
}

// databaseUsersPart reports whether a file staged beside an account's
// database dumps belongs to its database users rather than to a database.
func databaseUsersPart(name string) bool {
	switch name {
	case granular.DatabaseUsersFile,
		granular.RunnableDatabaseUsersFile,
		granular.DatabaseUsersAuthFile:
		return true
	}
	return false
}

// maildirInternal reports whether a name in ~/mail is part of the
// account's own maildir rather than a domain.
func maildirInternal(name string) bool {
	switch name {
	case "cur", "new", "tmp":
		return true
	}
	return strings.HasPrefix(name, ".")
}

func findSnapshot(snapshots []resticrun.Snapshot, id string) (resticrun.Snapshot, bool) {
	for _, snapshot := range snapshots {
		if snapshot.ID == id || snapshot.ShortID == id {
			return snapshot, true
		}
	}
	return resticrun.Snapshot{}, false
}

// parentWithin is the directory above path, or empty at the root.
func parentWithin(current, root string) string {
	if current == root || current == "" {
		return ""
	}
	parent := path.Dir(current)
	if parent != root && !strings.HasPrefix(parent, root+"/") {
		return root
	}
	return parent
}

// handleRestoreItems queues a granular restore: one mailbox, one database,
// the DNS records. What comes back is left on this server to collect.
func (s *Server) handleRestoreItems(w http.ResponseWriter, r *http.Request) {
	account := r.PostFormValue("account")
	back := "/restore?account=" + url.QueryEscape(account) +
		"&repository=" + url.QueryEscape(r.PostFormValue("repository")) +
		"&snapshot=" + url.QueryEscape(r.PostFormValue("snapshot")) +
		"&item=" + url.QueryEscape(r.PostFormValue("item"))

	// Choosing across parts of an account is a basket, not a restore: it
	// is put together over several views of this page and started once.
	switch action := r.PostFormValue("action"); action {
	case "add", "append", "remove", "empty":
		s.changeBasket(w, r, action, back)
		return
	}
	if r.PostFormValue("basket") != "" {
		s.startBasket(w, r, back)
		return
	}

	restore := nodestore.Restore{
		Account:      account,
		RepositoryID: r.PostFormValue("repository"),
		SnapshotID:   r.PostFormValue("snapshot"),
		Kind:         protocol.RestoreItems,
		ItemKind:     r.PostFormValue("item"),
		// Whether this goes back into the account or is left to collect.
		// The node refuses it for the kinds that cannot be written back,
		// so an operator who ticks the box on a DNS restore is told so
		// rather than quietly handed a download.
		Apply: r.PostFormValue("apply") != "",
	}
	for _, name := range r.PostForm["name"] {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			restore.ItemNames = append(restore.ItemNames, trimmed)
		}
	}
	// A kind that lists what it holds without being able to hand back part
	// of it -- a crontab is one file, and so is the FTP password file --
	// takes no names. Carrying them would say the choice was honoured.
	if !granular.Kind(restore.ItemKind).PicksItems() {
		restore.ItemNames = nil
	}
	if err := usableItemNames(granular.Kind(restore.ItemKind), restore.ItemNames); err != nil {
		s.redirect(w, r, back, "error", err.Error())
		return
	}
	// Names are checked first, so the page an operator is asked to read
	// back is the one the restore would actually run.
	if restore.Apply && !confirmed(r) {
		s.askFirst(w, r, confirmOnePart(restore.Account, granular.Kind(restore.ItemKind),
			restore.ItemNames, restore.SnapshotID, linkTo(back)))
		return
	}

	if _, err := s.engine.QueueRestore(restore); err != nil {
		s.redirect(w, r, back, "error", err.Error())
		return
	}
	if restore.Apply {
		s.redirect(w, r, back, "ok",
			"Queued. It WILL replace this part of the live account when it runs.")
		return
	}
	s.redirect(w, r, back, "ok",
		"Queued. What it recovers is left on this server to collect — "+
			"nothing on the live account is touched.")
}

func (s *Server) handleStartRestore(w http.ResponseWriter, r *http.Request) {
	if r.PostFormValue("account") == cpanel.SystemAccount &&
		strings.TrimSpace(r.PostFormValue("paths")) == "" {
		s.redirect(w, r, "/restore?account="+url.QueryEscape(cpanel.SystemAccount), "error",
			"The server's settings are not an account, so there is no account archive to "+
				"rebuild. Use Restore one thing → Server settings, which puts the files "+
				"somewhere you can read them.")
		return
	}
	restore := nodestore.Restore{
		Account:      r.PostFormValue("account"),
		RepositoryID: r.PostFormValue("repository"),
		SnapshotID:   r.PostFormValue("snapshot"),
		Kind:         protocol.RestoreAccount,
		Apply:        r.PostFormValue("apply") != "",
		Unrestricted: r.PostFormValue("unrestricted") != "",
	}

	if paths := strings.TrimSpace(r.PostFormValue("paths")); paths != "" {
		restore.Kind = protocol.RestoreFiles
		restore.Apply = false
		for _, line := range strings.Split(paths, "\n") {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				restore.IncludePaths = append(restore.IncludePaths, trimmed)
			}
		}
		restore.TargetDir = strings.TrimSpace(r.PostFormValue("target"))
	}

	// Asked after the file list, because picking files turns this into a
	// restore that overwrites nothing and needs no confirmation.
	if restore.Apply && !confirmed(r) {
		s.askFirst(w, r, confirmWholeAccount(restore.Account, restore.SnapshotID,
			linkTo("/restore?account="+restore.Account),
			s.accountIsGone(restore.Account), restore.Unrestricted))
		return
	}

	queued, err := s.engine.QueueRestore(restore)
	if err != nil {
		s.redirect(w, r, "/restore?account="+restore.Account, "error", err.Error())
		return
	}
	message := "Restore queued. The archive will be left on this server; nothing is overwritten."
	if queued.Apply {
		message = "Restore queued. It WILL overwrite the live account when it runs."
	}
	s.redirect(w, r, "/restore?account="+restore.Account, "ok", message)
}

// handleVerifyRequest rehearses a restore of an account's newest backup.
func (s *Server) handleVerifyRequest(w http.ResponseWriter, r *http.Request) {
	account := r.PostFormValue("account")
	if _, err := s.engine.QueueDrill(r.Context(), account); err != nil {
		s.redirect(w, r, "/accounts", "error", err.Error())
		return
	}
	s.redirect(w, r, "/restore", "ok", fmt.Sprintf(
		"Rehearsing a restore of %s. It rebuilds the account in scratch space, checks the "+
			"result and throws it away — the live account is not touched.", account))
}

// handleDownloadRequest rebuilds an account's newest backup into an archive
// that can then be fetched.
func (s *Server) handleDownloadRequest(w http.ResponseWriter, r *http.Request) {
	account := r.PostFormValue("account")

	// Download should download. Rebuilding takes minutes, so if an archive
	// for this account is already sitting there, hand that over instead of
	// making the operator wait for a copy of what they already have.
	if ready, found := s.engine.ReadyDownload(account); found {
		// Written directly, not through http.Redirect, which resolves a
		// query-only reference against the request path and would turn
		// this into "?p=accounts/".
		w.Header().Set("Location", "?p=download&id="+url.QueryEscape(ready.ID))
		w.WriteHeader(http.StatusSeeOther)
		return
	}

	if _, err := s.engine.QueueDownload(r.Context(), account); err != nil {
		s.redirect(w, r, "/accounts", "error", err.Error())
		return
	}
	s.redirect(w, r, "/restore", "ok", fmt.Sprintf(
		"Rebuilding the newest backup of %s. It takes a minute or two; a Download button "+
			"appears beside it below when it is ready. Nothing on the live account is "+
			"touched.", account))
}

// handleDownload streams a rebuilt archive to the browser.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	file, filename, size, err := s.engine.OpenArchiveForDownload(r.URL.Query().Get("id"))
	if err != nil {
		s.redirect(w, r, "/restore", "error", err.Error())
		return
	}
	defer file.Close()

	// Content-Disposition is what makes the browser save rather than
	// render, and it is also the only place the filename survives: cpsrvd
	// strips Content-Type from what the plugin returns.
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Somebody's whole account. Not something to leave in a cache.
	w.Header().Set("Cache-Control", "no-store, max-age=0")

	if _, err := io.Copy(w, file); err != nil {
		// The response is already going out, so there is nothing to say
		// to the browser beyond stopping.
		s.log.Error("stream archive", "file", filename, "error", err)
	}
}

// --- jobs ---

// logsView is what the Logs page shows, split by the question being asked.
// A backup of an account and a backup of the server's own settings are
// different work with different failure modes, and lumping them together
// made the second kind invisible among nineteen of the first.
type logsView struct {
	Tab         string
	Jobs        []nodestore.Job
	System      []nodestore.Job
	Restores    []restoreRow
	Lifecycle   []nodestore.LifecycleEvent
	Destination map[string]string
	// Counts label the tabs, so an operator can see there is something
	// under one without opening it.
	Counts map[string]int
	// Service is the service's own log, which is the one tab that is not
	// built from what this server recorded about a job but read back from
	// the journal it wrote as the job ran.
	Service serviceLogView
}

// logTable is one tab's worth of backup rows. Backups of accounts and
// backups of the server's own settings render the same way and differ only
// in what is in them, so they share a template and this says which is which.
type logTable struct {
	Rows        []nodestore.Job
	Destination map[string]string
	Live        string
	Empty       string
}

func (v logsView) BackupTable() logTable {
	return logTable{
		Rows: v.Jobs, Destination: v.Destination, Live: "backups",
		Empty: "No backups have run yet. They will appear here as schedules fire.",
	}
}

func (v logsView) SystemTable() logTable {
	return logTable{
		Rows: v.System, Destination: v.Destination, Live: "systembackups",
		Empty: "Nothing has backed up the server's own settings yet. " +
			"A schedule that covers the server as well as its accounts puts them here.",
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.engine.Store().Jobs(200)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	restores, err := s.engine.Store().Restores(100)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	lifecycle, err := s.engine.Store().LifecycleEvents(100)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	destinations, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	names := map[string]string{}
	for _, dest := range destinations {
		names[dest.Repository.ID] = dest.Name
	}

	view := logsView{
		Tab:         logTab(r.URL.Query().Get("tab")),
		Restores:    collectableRows(restores),
		Lifecycle:   lifecycle,
		Destination: names,
	}
	for _, run := range jobs {
		if run.Account == cpanel.SystemAccount {
			view.System = append(view.System, run)
			continue
		}
		view.Jobs = append(view.Jobs, run)
	}
	view.Counts = map[string]int{
		"backups":   len(view.Jobs),
		"system":    len(view.System),
		"restores":  len(view.Restores),
		"lifecycle": len(view.Lifecycle),
	}
	// Reading the journal costs a process, so it happens for the tab that
	// shows it and not for the four that do not.
	if view.Tab == "service" {
		view.Service = s.serviceLog(r.Context(), r)
	}
	s.render(w, r, "logs.html", "Logs", "logs", view)
}

func logTab(asked string) string {
	switch asked {
	case "system", "restores", "lifecycle", "service":
		return asked
	default:
		return "backups"
	}
}

// --- settings ---

// settingsTabs are the parts the page is divided into, and the first is
// what an address with no tab on it gets.
var settingsTabs = []string{"backups", "alerts", "storage", "version"}

// settingsTab is the address of one tab, for a handler sending an operator
// back to the card they were working in rather than to the top of the page.
func settingsTab(tab string) string {
	if tab == settingsTabs[0] {
		return "/settings"
	}
	return "/settings?tab=" + tab
}

// tabFor is which tab a request is asking for. Anything unrecognised is
// the first: a tab is a view of one page, not a thing that can be missing.
func tabFor(r *http.Request) string {
	asked := r.URL.Query().Get("tab")
	for _, tab := range settingsTabs {
		if asked == tab {
			return tab
		}
	}
	// Adding or editing a channel is alerts work whatever the address
	// says. Those links were written before the page had tabs, and a
	// drawer that opens on the wrong tab is a form with nothing behind it.
	if r.URL.Query().Get("addchannel") != "" || r.URL.Query().Get("channel") != "" {
		return "alerts"
	}
	return settingsTabs[0]
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	view, err := s.settingsPage()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	// The form for one channel, when it was asked for: the drawer fetches
	// this page and lifts it out, and a browser without JavaScript gets
	// the same form on a page of its own.
	if id := r.URL.Query().Get("channel"); id != "" {
		for i := range view.Channels {
			if view.Channels[i].ID == id {
				view.Editing = &view.Channels[i]
				break
			}
		}
	}
	view.Adding = r.URL.Query().Get("addchannel") != ""
	view.Tab = tabFor(r)

	s.render(w, r, "settings.html", "Settings", "settings", view)
}

// settingsPage gathers everything the page shows. It is built in one place
// because the page is rendered twice — once as itself, and once when a
// channel form comes back refused — and a second, thinner version of it
// would show an operator a page saying this server had no staging space
// and no restored files, on the strength of a typo in a mail server name.
func (s *Server) settingsPage() (settingsView, error) {
	// Read this from storage rather than the engine's startup snapshot.
	// Termination protection is consumed by a synchronous cPanel hook and
	// takes effect immediately without a service restart.
	settings, err := s.engine.Store().Settings()
	if err != nil {
		return settingsView{}, err
	}
	free := uint64(0)
	if info, err := os.Stat(settings.StagingRoot); err == nil && info.IsDir() {
		free, _ = stagingFree(settings.StagingRoot)
	}
	outputs, err := s.engine.RetainedOutput()
	if err != nil {
		return settingsView{}, err
	}
	var held uint64
	for _, output := range outputs {
		held += output.Bytes
	}

	// What a backup contains depends on the payload mode the schedules
	// use, so the page states it rather than describing the default.
	policies, err := s.engine.Store().Policies()
	if err != nil {
		return settingsView{}, err
	}
	split, monolithic := 0, 0
	for _, policy := range policies {
		if policy.PayloadMode == string(pkgacct.ModeMonolithic) {
			monolithic++
			continue
		}
		split++
	}

	channels, err := s.engine.Channels()
	if err != nil {
		return settingsView{}, err
	}

	// A failed check is not a reason to fail the page.
	lastChecked, checkError := "", ""
	panel := updatePanel{
		Running: agent.Version,
		// A build that is not exactly a tag is never replaced by an
		// update: it came from somebody who knows where it came from,
		// and installing over it would discard their work.
		Unreleased: !update.IsRelease(agent.Version),
	}
	if state, err := s.engine.Store().UpdateState(); err == nil {
		if !state.CheckedAt.IsZero() {
			lastChecked = humanAgo(&state.CheckedAt)
		}
		checkError = state.Error
		panel.Latest, panel.ReleaseURL = state.Version, state.URL
		panel.Checked = lastChecked
		panel.Channel = string(node.Channel(settings))
		panel.Dist = node.Channel(settings) == update.ChannelDist
		if !state.BuiltAt.IsZero() {
			panel.BuiltAt = humanAgo(&state.BuiltAt)
		}
		// Whether to ask every day is a setting; whether there is
		// something to install is a fact. Turning the daily ask off takes
		// the banner off every page, and leaves this card saying what was
		// last found, next to a button that asks again.
		panel.Newer = s.engine.UpdateOffered(state)
	}
	// Reading this is also what notices an upgrade that has finished,
	// since the process that started one is not the process that reports
	// on it.
	if upgrade, err := s.engine.UpgradeStatus(); err == nil {
		panel.Upgrade = upgrade
		panel.Installing = !upgrade.StartedAt.IsZero() && upgrade.FinishedAt.IsZero()
	} else {
		s.log.Error("read the upgrade in flight", "error", err)
	}

	// A server saved before this setting existed has no level stored. It
	// is logging at info, so that is what the page must show selected --
	// not an empty box that saves a level nobody chose.
	if settings.LogLevel == "" {
		settings.LogLevel = nodestore.DefaultLogLevel
	}
	return settingsView{
		Settings:    settings,
		StagingFree: free,
		Outputs:     outputs,
		OutputBytes: held,
		KeepDays:    keepDays(settings),
		DeletedDays: deletedDays(settings), DeletedPreset: deletedPreset(settings),
		LogLevels:   nodestore.LogLevels,
		Version:     agent.Version,
		LastChecked: lastChecked, CheckError: checkError,
		Update:     panel,
		Split:      split,
		Monolithic: monolithic,
		Channels:   channels,
		Kinds:      notify.Kinds,
		Events:     notify.Events,
		Submitted:  map[string]string{},
	}, nil
}

// settingsView is the settings page. It is a named type because the
// notification form needs to come back with what was typed into it.
type settingsView struct {
	Settings    nodestore.Settings
	StagingFree uint64
	Outputs     []staging.Output
	OutputBytes uint64
	KeepDays    int
	// DeletedDays is how long a deleted account's backups are kept, and
	// DeletedPreset is which of the offered periods that is -- "custom"
	// when it is a number somebody typed rather than one on the list.
	DeletedDays   int
	DeletedPreset string
	// LogLevels are what the log level can be set to, quietest first.
	LogLevels []string
	// Version is what this build calls itself, and LastChecked and
	// CheckError are the last ask about a newer release. A check that has
	// been failing for a month is worth seeing beside the tick that turns
	// it off.
	Version     string
	LastChecked string
	CheckError  string
	// Update is the release this server has been told about and, if one
	// is being installed, how that is going.
	Update updatePanel
	// Tab is which part of the settings is being shown. The page carries
	// seven cards, and somebody who came to change one thing should not
	// have to walk past the other six.
	Tab        string
	Split      int
	Monolithic int

	Channels []nodestore.Channel
	Kinds    []notify.Kind
	Events   []notify.Event
	Editing  *nodestore.Channel
	Adding   bool
	// FormError is what was wrong with the channel that was just
	// submitted, shown in the form rather than on the page behind it.
	FormError string
	// Submitted is what they typed, so a refused form comes back filled
	// in. Secrets are not in here.
	Submitted map[string]string
}

// Field is what the form should show: what was typed if the form is
// coming back refused, otherwise what is stored.
func (v settingsView) Field(name string) string {
	if value, typed := v.Submitted[name]; typed {
		return value
	}
	if v.Editing == nil {
		return ""
	}
	if name == "name" {
		return v.Editing.Name
	}
	return v.Editing.Config[name]
}

// Wants reports whether the channel being edited asked about an event, so
// its checkbox comes back ticked.
func (v settingsView) Wants(event notify.Event) bool {
	if len(v.Submitted) > 0 {
		return v.Submitted["event_"+string(event)] != ""
	}
	if v.Editing == nil {
		return false
	}
	for _, asked := range v.Editing.Events {
		if asked == string(event) {
			return true
		}
	}
	return false
}

// Kind is the kind the form should open on.
func (v settingsView) Kind() string {
	if value := v.Submitted["kind"]; value != "" {
		return value
	}
	if v.Editing != nil {
		return v.Editing.Kind
	}
	return string(notify.KindSMTP)
}

// Enabled reports whether the channel being edited is on. A new one is.
func (v settingsView) Enabled() bool {
	if len(v.Submitted) > 0 {
		return v.Submitted["enabled"] != ""
	}
	if v.Editing == nil {
		return true
	}
	return v.Editing.Enabled
}

// --- notification channels ---

// handleSaveChannel adds or edits somewhere to send notifications.
func (s *Server) handleSaveChannel(w http.ResponseWriter, r *http.Request) {
	channel := nodestore.Channel{
		ID:      strings.TrimSpace(r.PostFormValue("id")),
		Name:    strings.TrimSpace(r.PostFormValue("name")),
		Kind:    strings.TrimSpace(r.PostFormValue("kind")),
		Config:  map[string]string{},
		Enabled: r.PostFormValue("enabled") != "",
	}
	if channel.ID != "" {
		// The kind is fixed once the channel exists: its settings mean
		// different things, and silently reinterpreting them is worse
		// than making the operator add a new one.
		existing, err := s.engine.Store().Channel(channel.ID)
		if err != nil {
			s.redirect(w, r, settingsTab("alerts"), "error", "That channel no longer exists.")
			return
		}
		channel.Kind = existing.Kind
	}

	secrets := map[string]string{}
	for _, field := range channelConfigFields(channel.Kind) {
		if value := strings.TrimSpace(r.PostFormValue(field)); value != "" {
			channel.Config[field] = value
		}
	}
	for _, field := range channelSecretFields(channel.Kind) {
		secrets[field] = r.PostFormValue(field)
	}
	for _, event := range notify.Events {
		if r.PostFormValue("event_"+string(event)) != "" {
			channel.Events = append(channel.Events, string(event))
		}
	}

	saved, err := s.engine.SaveChannel(channel, secrets)
	if err != nil {
		s.refuseChannel(w, r, err)
		return
	}
	s.redirect(w, r, settingsTab("alerts"), "ok",
		fmt.Sprintf("Saved %q. Send a test to make sure it arrives.", saved.Name))
}

// handleTestChannel sends one message now.
func (s *Server) handleTestChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	if err := s.engine.TestChannel(r.Context(), id); err != nil {
		s.redirect(w, r, settingsTab("alerts"), "error", "It did not go: "+err.Error())
		return
	}
	s.redirect(w, r, settingsTab("alerts"), "ok", "Sent. If it has not arrived, it was not delivered.")
}

// handleDeleteChannel removes one.
func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.DeleteChannel(r.PostFormValue("id")); err != nil {
		s.redirect(w, r, settingsTab("alerts"), "error", err.Error())
		return
	}
	s.redirect(w, r, settingsTab("alerts"), "ok", "Removed. Nothing will be sent there.")
}

// refuseChannel re-renders the settings page with the channel form open,
// filled in as it was, and the reason on it — rather than sending the
// operator back to an empty form to type it all again.
func (s *Server) refuseChannel(w http.ResponseWriter, r *http.Request, cause error) {
	view, err := s.settingsPage()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	view.FormError = cause.Error()
	view.Adding = true
	// A refused channel form belongs where the channels are, whatever
	// tab the page was last seen on.
	view.Tab = "alerts"

	secret := map[string]bool{"csrf": true}
	for _, kind := range notify.Kinds {
		for _, field := range channelSecretFields(string(kind)) {
			secret[field] = true
		}
	}
	for name, values := range r.PostForm {
		// Secrets are not handed back: they would then live in a page
		// rather than only in the vault.
		if secret[name] || len(values) == 0 {
			continue
		}
		view.Submitted[name] = values[0]
	}
	if id := r.PostFormValue("id"); id != "" {
		for i := range view.Channels {
			if view.Channels[i].ID == id {
				view.Editing = &view.Channels[i]
				view.Adding = false
				break
			}
		}
	}
	s.render(w, r, "settings.html", "Settings", "settings", view)
}

// channelConfigFields is what a kind is configured with, in the order the
// form asks for it.
func channelConfigFields(kind string) []string {
	switch notify.Kind(kind) {
	case notify.KindSMTP:
		return []string{"host", "port", "from", "to", "username"}
	case notify.KindNtfy:
		return []string{"server", "topic"}
	case notify.KindTelegram:
		return []string{"chat_id"}
	case notify.KindWebhook:
		return []string{"url"}
	}
	return nil
}

// channelSecretFields is what a kind is configured with that must never be
// rendered back into a page.
func channelSecretFields(kind string) []string {
	switch notify.Kind(kind) {
	case notify.KindSMTP:
		return []string{"password"}
	case notify.KindNtfy:
		return []string{"token"}
	case notify.KindTelegram:
		return []string{"token"}
	case notify.KindWebhook:
		return []string{"token"}
	}
	return nil
}

// keepDays is the retention shown in the form, with the default spelt out
// rather than left as a zero the operator has to interpret.
func keepDays(settings nodestore.Settings) int {
	if settings.KeepOutputDays == 0 {
		return nodestore.DefaultKeepOutputDays
	}
	return settings.KeepOutputDays
}

// deletedDays is how long a deleted account's backups are kept, with the
// default spelt out rather than left as a zero to interpret.
func deletedDays(settings nodestore.Settings) int {
	if settings.DeletedAccountDays == 0 {
		return nodestore.DefaultDeletedAccountDays
	}
	return settings.DeletedAccountDays
}

// deletedPresets are the periods the form offers, in the order it offers
// them. Anything else an operator has set is offered back as "custom".
var deletedPresets = []int{-1, 30, 90, 180, 365}

// deletedPreset names which of the offered periods is in force.
func deletedPreset(settings nodestore.Settings) string {
	days := deletedDays(settings)
	for _, preset := range deletedPresets {
		if days == preset {
			return strconv.Itoa(preset)
		}
	}
	return "custom"
}

// handleDeleteOutput removes one finished restore's files from the work
// directory, when an operator has finished with them.
func (s *Server) handleDeleteOutput(w http.ResponseWriter, r *http.Request) {
	key := r.PostFormValue("key")
	if err := s.engine.DeleteOutput(key); err != nil {
		s.redirect(w, r, settingsTab("storage"), "error", err.Error())
		return
	}
	s.redirect(w, r, settingsTab("storage"), "ok", "Removed. The backups themselves are untouched.")
}

// handleClearOutput empties the work directory of everything collected.
func (s *Server) handleClearOutput(w http.ResponseWriter, r *http.Request) {
	outputs, err := s.engine.RetainedOutput()
	if err != nil {
		s.redirect(w, r, settingsTab("storage"), "error", err.Error())
		return
	}
	var freed uint64
	for _, output := range outputs {
		if err := s.engine.DeleteOutput(output.Key); err != nil {
			s.redirect(w, r, settingsTab("storage"), "error", err.Error())
			return
		}
		freed += output.Bytes
	}
	s.redirect(w, r, settingsTab("storage"), "ok",
		fmt.Sprintf("Removed %s of restored files. The backups themselves are untouched.",
			humanBytes(freed)))
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.engine.Store().Settings()
	if err != nil {
		s.redirect(w, r, "/settings", "error", err.Error())
		return
	}
	settings.MaxConcurrent = atoiOr(r.PostFormValue("max_concurrent"), 1)
	settings.ResticBinary = strings.TrimSpace(r.PostFormValue("restic"))
	settings.ResticCACert = strings.TrimSpace(r.PostFormValue("restic_cacert"))
	// A restore's files are kept so they can be collected; this is how
	// long before they are swept. Negative keeps them for ever, which is
	// what this server did before it swept anything at all.
	settings.KeepOutputDays = atoiOr(r.PostFormValue("keep_output_days"),
		nodestore.DefaultKeepOutputDays)
	// How long a deleted account's backups are kept. The form offers a
	// few periods and "custom"; anything unreadable leaves the setting
	// alone rather than silently choosing a shorter life for somebody
	// else's backups.
	if chosen := r.PostFormValue("deleted_account_days"); chosen != "" {
		if chosen == "custom" {
			chosen = r.PostFormValue("deleted_account_days_custom")
		}
		if days, err := strconv.Atoi(strings.TrimSpace(chosen)); err == nil && days != 0 {
			settings.DeletedAccountDays = days
		}
	}
	// Bug-report delivery is fixed to the bug tracker intake. Retain legacy
	// email settings on disk for compatibility, but never send through them.
	settings.ProtectAccountRemoval = r.PostFormValue("protect_account_removal") == "1"
	// Stored the other way round: checking for a newer version is what a
	// server does unless somebody says not to.
	settings.NoUpdateCheck = r.PostFormValue("update_check") != "1"
	settings.BackupOnSuspension = r.PostFormValue("backup_on_suspension") == "1"
	// The level is set through the engine rather than written here: it
	// moves the running service as well as the stored setting, and a name
	// it cannot read leaves both where they were.
	if level := strings.TrimSpace(r.PostFormValue("log_level")); level != "" {
		if err := s.engine.SetLogLevel(level); err != nil {
			s.redirect(w, r, "/settings", "error", err.Error())
			return
		}
		// SetLogLevel has already written the settings this handler read
		// before it ran, so the copy in hand is one field out of date.
		settings.LogLevel = level
	}
	if hostname := strings.TrimSpace(r.PostFormValue("hostname")); hostname != "" {
		settings.Hostname = hostname
	}

	// The staging root is deliberately not editable here. Snapshot paths
	// embed it and restic groups retention by path, so changing it after
	// the first backup orphans every existing retention group.
	if err := s.engine.Store().SaveSettings(settings); err != nil {
		s.redirect(w, r, "/settings", "error", err.Error())
		return
	}
	s.redirect(w, r, "/settings", "ok", "Saved. Account-removal protection takes effect immediately; restart the service for the other runtime changes.")
}

func atoiOr(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func stagingFree(path string) (uint64, error) {
	return freeBytes(filepath.Clean(path))
}

// --- fonts ---

// fontFiles is the allowlist. A request names a face, never a path, so
// nothing outside this map can be read however the name is spelt.
var fontFiles = map[string]string{
	"fira-sans-400.woff2": "fonts/fira-sans-400.woff2",
	"fira-sans-500.woff2": "fonts/fira-sans-500.woff2",
	"fira-sans-600.woff2": "fonts/fira-sans-600.woff2",
	"fira-sans-700.woff2": "fonts/fira-sans-700.woff2",
	"fira-code.woff2":     "fonts/fira-code.woff2",
}

// handleFont serves one vendored typeface. Content-Disposition is what
// gets a non-HTML response past cpsrvd intact; a browser ignores it for a
// subresource, and if any of this fails the stylesheet's fallback stack
// renders exactly as it did before the fonts were vendored.
func (s *Server) handleFont(w http.ResponseWriter, r *http.Request) {
	path, ok := fontFiles[r.URL.Query().Get("name")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, err := fontFS.ReadFile(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "font/woff2")
	w.Header().Set("Content-Disposition", `inline; filename="`+filepath.Base(path)+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(body)
}

// --- disaster recovery ---

// recoverView is the page a replacement server starts from.
type recoverView struct {
	// Destinations already attached, so an operator who has done this can
	// see what is in them without doing it again.
	Repositories []recoverRepository
	Hostname     string
	Error        string
}

// recoverRepository is one attached repository and what it holds.
type recoverRepository struct {
	ID       string
	Name     string
	Path     string
	Contents node.Contents
	Err      string
}

func (s *Server) handleRecover(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "recover.html", "Restore", "restore",
		s.recoverContents(r.Context(), ""))
}

// recoverContents reads every attached repository so the page can say what
// is actually in them, rather than what this server remembers.
func (s *Server) recoverContents(ctx context.Context, failure string) recoverView {
	view := recoverView{Hostname: s.engine.Settings().Hostname, Error: failure}
	destinations, err := s.destinationViews()
	if err != nil {
		view.Error = err.Error()
		return view
	}
	for _, dest := range destinations {
		if dest.Repository.ID == "" {
			continue
		}
		row := recoverRepository{
			ID:   dest.Repository.ID,
			Name: dest.Name,
			Path: dest.Repository.Path,
		}
		contents, err := s.engine.Contents(ctx, dest.Repository.ID)
		if err != nil {
			row.Err = err.Error()
		} else {
			row.Contents = contents
		}
		view.Repositories = append(view.Repositories, row)
	}
	return view
}

// handleAttach points this server at backups that already exist.
func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	config := map[string]string{}
	secrets := map[string]string{}
	kind := destination.Type(r.PostFormValue("type"))

	switch kind {
	case destination.TypeSFTP:
		config["host"] = strings.TrimSpace(r.PostFormValue("host"))
		config["user"] = strings.TrimSpace(r.PostFormValue("user"))
		config["root"] = strings.TrimSpace(r.PostFormValue("root"))
		config["identity_file"] = strings.TrimSpace(r.PostFormValue("identity_file"))
		config["known_hosts_file"] = strings.TrimSpace(r.PostFormValue("known_hosts_file"))
		if port := strings.TrimSpace(r.PostFormValue("port")); port != "" && port != "22" {
			config["port"] = port
		}
	case destination.TypeREST:
		config["base_url"] = strings.TrimSpace(r.PostFormValue("base_url"))
		secrets["username"] = strings.TrimSpace(r.PostFormValue("username"))
		secrets["password"] = r.PostFormValue("password")
	case destination.TypeS3:
		config["bucket"] = strings.TrimSpace(r.PostFormValue("bucket"))
		if endpoint := strings.TrimSpace(r.PostFormValue("endpoint")); endpoint != "" {
			config["endpoint"] = endpoint
		}
		if region := strings.TrimSpace(r.PostFormValue("region")); region != "" {
			config["region"] = region
		}
		secrets["access_key_id"] = strings.TrimSpace(r.PostFormValue("access_key_id"))
		secrets["secret_access_key"] = r.PostFormValue("secret_access_key")
	case destination.TypeLocal:
		config["root"] = strings.TrimSpace(r.PostFormValue("root"))
	default:
		s.render(w, r, "recover.html", "Restore", "restore",
			s.recoverContents(r.Context(), "Choose where the old backups are."))
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		name = "Recovered backups"
	}
	contents, err := s.engine.Attach(r.Context(), node.AttachRequest{
		Destination: nodestore.Destination{
			Name: name, Type: string(kind), Config: config,
		},
		Secrets:        secrets,
		RepositoryPath: strings.TrimSpace(r.PostFormValue("repo_path")),
		Password:       r.PostFormValue("recovery_key"),
	})
	if err != nil {
		s.render(w, r, "recover.html", "Restore", "restore",
			s.recoverContents(r.Context(), err.Error()))
		return
	}

	s.redirect(w, r, "/restore?tab=server", "ok", fmt.Sprintf(
		"Attached. %d backups are readable from here, covering %d account%s.",
		contents.Snapshots, len(contents.Accounts), plural(len(contents.Accounts))))
}

// handleRecoverAccount queues the restore of one account from an attached
// repository, onto this server.
func (s *Server) handleRecoverAccount(w http.ResponseWriter, r *http.Request) {
	account := r.PostFormValue("account")
	repository := r.PostFormValue("repository")
	apply := r.PostFormValue("apply") != ""

	// Which backup: the newest one of this account in this destination,
	// resolved now rather than carried in the form. Recovery is chosen by
	// account — the operator picks the customer, not a hash — and this is
	// what "recover them" means.
	snapshot := r.PostFormValue("snapshot")
	if snapshot == "" {
		found, err := s.engine.LatestSnapshot(r.Context(), repository, account)
		if err != nil {
			s.redirect(w, r, "/restore?tab=server", "error", err.Error())
			return
		}
		snapshot = found
	}

	if apply && !confirmed(r) {
		// The backup just resolved travels on to the confirmed request,
		// so the operator runs the restore point they were shown rather
		// than whatever is newest by the time they tick the box.
		r.PostForm.Set("snapshot", snapshot)
		s.askFirst(w, r, confirmWholeAccount(account, snapshot,
			linkTo("/restore?tab=server"), s.accountIsGone(account),
			r.PostFormValue("unrestricted") != ""))
		return
	}

	queued, err := s.engine.QueueRestore(nodestore.Restore{
		Account:      account,
		RepositoryID: repository,
		SnapshotID:   snapshot,
		Kind:         protocol.RestoreAccount,
		Apply:        apply,
		Unrestricted: r.PostFormValue("unrestricted") != "",
	})
	if err != nil {
		s.redirect(w, r, "/restore?tab=server", "error", err.Error())
		return
	}
	if queued.Apply {
		s.redirect(w, r, "/restore?tab=server", "ok", fmt.Sprintf(
			"Restoring %s onto this server. It will be handed to cPanel's own restore when the "+
				"archive is rebuilt.", account))
		return
	}
	s.redirect(w, r, "/restore?tab=server", "ok", fmt.Sprintf(
		"Rebuilding %s. The archive will be left on this server to inspect; nothing is "+
			"created until you ask for that.", account))
}

// handleRecoverAccounts queues the same restore for several accounts at
// once. A server that lost a disk lost every account on it, and asking an
// operator to click through nineteen of them one at a time — in a hurry,
// under pressure, keeping their own tally of which ones they have done — is
// how one gets missed.
//
// The engine still runs them one at a time: staging rebuilds an account in
// full, and a machine with room for one is not a machine with room for
// nineteen at once. What this changes is the asking, not the doing.
func (s *Server) handleRecoverAccounts(w http.ResponseWriter, r *http.Request) {
	repository := r.PostFormValue("repository")

	// Back to the tab the choosing was done on, still pointed at the same
	// destination: an operator who has just queued five accounts out of
	// nineteen wants to see the same list again, not a reset page.
	back := "/restore"
	if r.PostFormValue("from") == "deleted" {
		back = "/restore?tab=deleted"
	}
	if repository != "" {
		if strings.Contains(back, "?") {
			back += "&"
		} else {
			back += "?"
		}
		back += "repository=" + url.QueryEscape(repository)
	}
	accounts := r.PostForm["account"]
	apply := r.PostFormValue("apply") != ""
	unrestricted := r.PostFormValue("unrestricted") != ""

	// A date rather than a snapshot: several accounts are chosen at once
	// and their backups are not taken at the same minute, so each
	// resolves the same day to its own newest backup up to the end of it.
	// Empty means the newest there is, which is what rebuilding a lost
	// server means.
	var asOf time.Time
	if chosen := strings.TrimSpace(r.PostFormValue("asof")); chosen != "" {
		day, err := time.ParseInLocation("2006-01-02", chosen, time.Local)
		if err != nil {
			s.redirect(w, r, back, "error",
				"That restore point is not a date. Use the date field, or leave it "+
					"empty for the most recent backup.")
			return
		}
		asOf = day.AddDate(0, 0, 1).Add(-time.Second)
		if asOf.After(time.Now()) {
			asOf = time.Time{}
		}
	}

	if len(accounts) == 0 {
		s.redirect(w, r, back, "error", "Choose at least one account first.")
		return
	}
	if repository == "" {
		s.redirect(w, r, back, "error", "Choose which destination to restore from.")
		return
	}
	// Asked after the list is known to be a list, so the page names the
	// accounts rather than warning about a restore of nothing.
	if apply && !confirmed(r) {
		s.askFirst(w, r, confirmManyAccounts(accounts,
			strings.TrimSpace(r.PostFormValue("asof")), linkTo(back), unrestricted))
		return
	}

	// One account failing is not a reason to abandon the rest: a name that
	// already has work in flight, or has no backup in this destination, is
	// reported by name and the others still go.
	var queued []string
	var refused []string
	for _, account := range accounts {
		snapshot, err := s.engine.SnapshotAsOf(r.Context(), repository, account, asOf)
		if err != nil {
			refused = append(refused, account+" ("+err.Error()+")")
			continue
		}
		if _, err := s.engine.QueueRestore(nodestore.Restore{
			Account:      account,
			RepositoryID: repository,
			SnapshotID:   snapshot,
			Kind:         protocol.RestoreAccount,
			Apply:        apply,
			Unrestricted: unrestricted,
		}); err != nil {
			refused = append(refused, account+" ("+err.Error()+")")
			continue
		}
		queued = append(queued, account)
	}

	verb := "Rebuilding"
	if apply {
		verb = "Restoring onto this server"
	}
	if !asOf.IsZero() {
		verb += " from " + asOf.Format("2 January 2006")
	}
	switch {
	case len(queued) == 0:
		s.redirect(w, r, back, "error", "Nothing was queued: "+strings.Join(refused, "; "))
	case len(refused) == 0:
		s.redirect(w, r, back, "ok", fmt.Sprintf("%s %s, one after another: %s.",
			verb, counted(len(queued), "account"), strings.Join(queued, ", ")))
	default:
		s.redirect(w, r, back, "warn", fmt.Sprintf(
			"%s %s: %s. Not queued: %s.",
			verb, counted(len(queued), "account"), strings.Join(queued, ", "),
			strings.Join(refused, "; ")))
	}
}

// --- browsing what is stored ---

// browseView is one level of what a destination holds: the destinations
// themselves, then the accounts in one, then that account's backups, then
// what is inside one of them.
//
// Each level is fetched when it is asked for. Listing every file of every
// snapshot of nineteen accounts would be minutes of restic and megabytes
// of page for a question nobody asked.
type browseView struct {
	Destinations []destinationView
	Repository   string
	Destination  string
	Account      string
	Accounts     []node.AccountBackups
	SystemBackup bool
	Snapshot     string
	SnapshotAt   time.Time
	Snapshots    []resticrun.Snapshot
	Path         string
	Up           string
	Crumbs       []crumb
	Entries      []browseEntry
	Err          string
}

// crumb is one step of the path back out.
type crumb struct {
	Label string
	URL   string
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	destinations, err := s.destinationViews()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	query := r.URL.Query()
	view := browseView{
		Destinations: destinations,
		Repository:   query.Get("repository"),
		Account:      query.Get("account"),
		Snapshot:     query.Get("snapshot"),
		Path:         query.Get("path"),
	}
	for _, dest := range destinations {
		if dest.Repository.ID == view.Repository {
			view.Destination = dest.Name
		}
	}

	switch {
	case view.Repository == "":
		// Nothing chosen: the destinations themselves.
	case view.Snapshot != "":
		s.browseSnapshot(r, &view)
	case view.Account != "":
		s.browseSnapshots(r, &view)
	default:
		s.browseAccounts(r, &view)
	}
	view.Crumbs = browseCrumbs(view)
	s.render(w, r, "browse.html", "Browse backups", "browse", view)
}

// browseAccounts lists what a repository holds, from its snapshots.
func (s *Server) browseAccounts(r *http.Request, view *browseView) {
	contents, err := s.engine.Contents(r.Context(), view.Repository)
	if err != nil {
		view.Err = err.Error()
		return
	}
	view.Accounts = contents.Accounts
	view.SystemBackup = contents.System
}

// browseSnapshots lists one account's backups, newest first.
func (s *Server) browseSnapshots(r *http.Request, view *browseView) {
	snapshots, err := s.engine.Snapshots(r.Context(), view.Repository, view.Account)
	if err != nil {
		view.Err = err.Error()
		return
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Time.After(snapshots[j].Time) })
	view.Snapshots = snapshots
}

// browseSnapshot lists one directory inside one backup.
func (s *Server) browseSnapshot(r *http.Request, view *browseView) {
	snapshots, err := s.engine.Snapshots(r.Context(), view.Repository, view.Account)
	if err != nil {
		view.Err = err.Error()
		return
	}
	snapshot, found := findSnapshot(snapshots, view.Snapshot)
	if !found {
		view.Err = "That backup is not in this destination."
		return
	}
	view.SnapshotAt = snapshot.Time

	// With no path, the snapshot's own roots: the home directory, the
	// databases, the metadata archive — whatever it was told to store.
	if view.Path == "" {
		for _, root := range snapshot.Paths {
			view.Entries = append(view.Entries, browseEntry{
				Name: root, Path: root, Dir: true, Item: root,
			})
		}
		sort.Slice(view.Entries, func(i, j int) bool {
			return view.Entries[i].Name < view.Entries[j].Name
		})
		return
	}

	entries, err := s.engine.Browse(r.Context(), view.Repository, snapshot.ID, view.Path)
	if err != nil {
		view.Err = err.Error()
		return
	}
	view.Up = path.Dir(view.Path)
	if view.Up == view.Path {
		view.Up = ""
	}
	for _, entry := range entries {
		if entry.Path == view.Path {
			continue
		}
		view.Entries = append(view.Entries, browseEntry{
			Name: entry.Name,
			Path: entry.Path,
			Size: entry.Size,
			Dir:  entry.IsDir(),
			Item: entry.Path,
		})
	}
	sort.Slice(view.Entries, func(i, j int) bool {
		if view.Entries[i].Dir != view.Entries[j].Dir {
			return view.Entries[i].Dir
		}
		return view.Entries[i].Name < view.Entries[j].Name
	})
}

// browseCrumbs is the way back out of wherever the operator has got to.
func browseCrumbs(view browseView) []crumb {
	crumbs := []crumb{{Label: "Destinations", URL: "?p=browse"}}
	if view.Repository == "" {
		return crumbs
	}
	name := view.Destination
	if name == "" {
		name = "this destination"
	}
	crumbs = append(crumbs, crumb{
		Label: name,
		URL:   "?p=browse&repository=" + url.QueryEscape(view.Repository),
	})
	if view.Account == "" {
		return crumbs
	}
	crumbs = append(crumbs, crumb{
		Label: view.Account,
		URL: "?p=browse&repository=" + url.QueryEscape(view.Repository) +
			"&account=" + url.QueryEscape(view.Account),
	})
	if view.Snapshot == "" {
		return crumbs
	}
	base := "?p=browse&repository=" + url.QueryEscape(view.Repository) +
		"&account=" + url.QueryEscape(view.Account) +
		"&snapshot=" + url.QueryEscape(view.Snapshot)
	crumbs = append(crumbs, crumb{Label: shortID(view.Snapshot), URL: base})

	if view.Path != "" {
		crumbs = append(crumbs, crumb{Label: view.Path, URL: base + "&path=" + url.QueryEscape(view.Path)})
	}
	return crumbs
}

// --- retention ---

// handlePlanRetention asks what retention would delete, without deleting
// anything.
func (s *Server) handlePlanRetention(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("repository")
	state, err := s.engine.PlanRetention(r.Context(), id)
	if err != nil {
		s.redirect(w, r, "/destinations", "error", err.Error())
		return
	}
	if state.WouldDrop == 0 {
		s.redirect(w, r, "/destinations", "ok",
			"Nothing would be removed: every backup there is inside the keep policy.")
		return
	}
	s.redirect(w, r, "/destinations", "warn", fmt.Sprintf(
		"%d of %d backups would be removed. Read what is below before approving it.",
		state.WouldDrop, state.WouldDrop+state.WouldKeep))
}

// handleApproveRetention records that an operator has read a plan and
// agreed this destination may have backups deleted from it.
func (s *Server) handleApproveRetention(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.ApproveRetention(r.PostFormValue("repository")); err != nil {
		s.redirect(w, r, "/destinations", "error", err.Error())
		return
	}
	s.redirect(w, r, "/destinations", "ok",
		"Approved. Retention will run here from now on, and what it removes is recorded.")
}

// handleWithdrawRetention stops it again.
func (s *Server) handleWithdrawRetention(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.WithdrawRetention(r.PostFormValue("repository")); err != nil {
		s.redirect(w, r, "/destinations", "error", err.Error())
		return
	}
	s.redirect(w, r, "/destinations", "ok",
		"Stopped. Nothing more will be removed from this destination until you approve it again.")
}

// handleRunRetention applies it now rather than waiting for the next pass.
//
// This runs under the request: a prune that outlives the server's write
// timeout has its context cancelled when the response dies. restic is
// crash-safe, so nothing is damaged and the next pass finishes the job --
// but the button can abort work the scheduled run would have completed.
func (s *Server) handleRunRetention(w http.ResponseWriter, r *http.Request) {
	removed, err := s.engine.ApplyRetention(r.Context(), r.PostFormValue("repository"))
	if err != nil {
		s.redirect(w, r, "/destinations", "error", err.Error())
		return
	}
	if removed == 0 {
		s.redirect(w, r, "/destinations", "ok", "Nothing needed removing.")
		return
	}
	s.redirect(w, r, "/destinations", "ok", fmt.Sprintf(
		"Removed %d backups and reclaimed the space they were holding.", removed))
}

// basketRow is one category in a recovery basket, as a page shows it.
type basketRow struct {
	Kind  granular.Kind
	Title string
	// Names is what was ticked. Empty means the whole category.
	Names []string
	// Applies reports whether this part can be written back into the
	// account, which decides whether the basket as a whole can be.
	Applies bool
}

// basketRows is a basket in the order its page offers the categories, so
// it reads the same way as the page around it.
func basketRows(basket nodestore.Basket, order []granular.Kind,
	title func(granular.Kind) string) []basketRow {

	rows := make([]basketRow, 0, len(basket.Items))
	for _, kind := range order {
		for _, item := range basket.Items {
			if granular.Kind(item.Kind) != kind {
				continue
			}
			rows = append(rows, basketRow{
				Kind: kind, Title: title(kind),
				Names: item.Names, Applies: kind.CanApply(),
			})
		}
	}
	return rows
}

// basketCanApply reports whether everything in a basket can go back into
// the account.
//
// One part that cannot makes the whole basket a download: putting back the
// rest and leaving that one out is not what was asked for, and would only
// be discovered afterwards.
func basketCanApply(basket nodestore.Basket) bool {
	if basket.Empty() {
		return false
	}
	for _, item := range basket.Items {
		if !granular.Kind(item.Kind).CanApply() {
			return false
		}
	}
	return true
}

// basketBlocker names the parts that keep a basket from being put back.
func basketBlocker(basket nodestore.Basket, order []granular.Kind,
	title func(granular.Kind) string) string {

	var blocked []string
	for _, row := range basketRows(basket, order, title) {
		if !row.Applies {
			blocked = append(blocked, row.Title)
		}
	}
	return granular.JoinAnd(blocked)
}

// orderedSelections is what a basket restores, in the order the parts are
// offered rather than the order they were chosen. What a restore is called
// on the history page is built from this, and a name that reads differently
// for two restores of the same parts reads as two different things.
func orderedSelections(basket nodestore.Basket, order []granular.Kind) []nodestore.RestoreSelection {
	items := make([]nodestore.RestoreSelection, 0, len(basket.Items))
	placed := make(map[int]bool, len(basket.Items))
	for _, kind := range order {
		for i, item := range basket.Items {
			if granular.Kind(item.Kind) == kind {
				items = append(items, item)
				placed[i] = true
			}
		}
	}
	// Anything the order does not name is kept, at the end. Ordering is
	// about how a restore reads; dropping a part here would hand the
	// checks below a basket that is not the one somebody filled.
	for i, item := range basket.Items {
		if !placed[i] {
			items = append(items, item)
		}
	}
	return items
}

func inBasket(basket nodestore.Basket, kind granular.Kind) bool {
	for _, item := range basket.Items {
		if granular.Kind(item.Kind) == kind {
			return true
		}
	}
	return false
}

// changeBasket adds one part of an account to the basket, takes one out,
// or empties it, and returns to the page it was chosen on.
func (s *Server) changeBasket(w http.ResponseWriter, r *http.Request, action, back string) {
	var (
		account    = r.PostFormValue("account")
		repository = r.PostFormValue("repository")
		snapshot   = r.PostFormValue("snapshot")
		kind       = granular.Kind(r.PostFormValue("item"))
	)
	if action == "empty" {
		if err := s.engine.Store().EmptyBasket(nodestore.BasketOfOperator, account, repository, snapshot); err != nil {
			s.redirect(w, r, back, "error", err.Error())
			return
		}
		s.redirect(w, r, back, "ok", "The basket is empty again.")
		return
	}
	if action == "remove" {
		if _, err := s.engine.Store().TakeFromBasket(nodestore.BasketOfOperator, account, repository, snapshot,
			string(kind)); err != nil {
			s.redirect(w, r, back, "error", err.Error())
			return
		}
		s.redirect(w, r, back, "ok", "Taken out of the basket.")
		return
	}

	var names []string
	for _, name := range r.PostForm["name"] {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	if !kind.PicksItems() {
		names = nil
	}
	if kind.NeedsNames() && len(names) == 0 {
		s.redirect(w, r, back, "error", "Choose at least one item first.")
		return
	}
	if err := usableItemNames(kind, names); err != nil {
		s.redirect(w, r, back, "error", err.Error())
		return
	}
	// A button on one row chooses one thing and says nothing about the
	// rows above it, so it adds; a form of tick boxes shows what is
	// chosen now, so it replaces.
	put := s.engine.Store().PutInBasket
	if action == "append" {
		put = s.engine.Store().AddToBasket
	}
	basket, err := put(nodestore.BasketOfOperator, account, repository, snapshot,
		nodestore.RestoreSelection{Kind: string(kind), Names: names})
	if err != nil {
		s.redirect(w, r, back, "error", err.Error())
		return
	}
	s.redirect(w, r, back, "ok", fmt.Sprintf("Added. The basket holds %d.", basket.Count()))
}

// startBasket queues everything in the basket as one restore.
//
// One restore rather than several because the parts of an account depend on
// each other -- a database its user cannot open is what two restores
// produce when the second fails -- and because one account may only have
// one job in flight, so several would run one after another with gaps.
func (s *Server) startBasket(w http.ResponseWriter, r *http.Request, back string) {
	var (
		account    = r.PostFormValue("account")
		repository = r.PostFormValue("repository")
		snapshot   = r.PostFormValue("snapshot")
		apply      = r.PostFormValue("apply") != ""
	)
	basket, err := s.engine.Store().Basket(nodestore.BasketOfOperator, account, repository, snapshot)
	if err != nil {
		s.redirect(w, r, back, "error", err.Error())
		return
	}
	if basket.Empty() {
		s.redirect(w, r, back, "error", "There is nothing in the basket.")
		return
	}
	chosen := orderedSelections(basket, granular.Kinds)
	if apply && !confirmed(r) {
		s.askFirst(w, r, confirmBasket(account,
			basketRows(basket, granular.Kinds, granular.Kind.Title),
			snapshot, linkTo(back)))
		return
	}
	restore := nodestore.Restore{
		Account: account, RepositoryID: repository, SnapshotID: snapshot,
		Kind: protocol.RestoreItems, Apply: apply,
		Items: chosen,
	}
	// A basket of one reads on the history page the way a single restore
	// always has, rather than as a new shape nothing else knows.
	if len(chosen) == 1 {
		restore.ItemKind = chosen[0].Kind
		restore.ItemNames = chosen[0].Names
	}
	for _, item := range chosen {
		if err := usableItemNames(granular.Kind(item.Kind), item.Names); err != nil {
			s.redirect(w, r, back, "error", err.Error())
			return
		}
	}
	if _, err := s.engine.QueueRestore(restore); err != nil {
		s.redirect(w, r, back, "error", err.Error())
		return
	}
	if err := s.engine.Store().EmptyBasket(nodestore.BasketOfOperator, account, repository, snapshot); err != nil {
		s.log.Error("empty the recovery basket", "account", account, "error", err)
	}
	if apply {
		s.redirect(w, r, back, "ok",
			"Queued. It WILL replace these parts of the live account when it runs.")
		return
	}
	s.redirect(w, r, back, "ok",
		"Queued. What it recovers is left on this server to collect — "+
			"nothing on the live account is touched.")
}
