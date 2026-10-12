package webui

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shukiv/gniza/internal/granular"
	"github.com/shukiv/gniza/internal/inventory"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/protocol"
	"github.com/shukiv/gniza/internal/reassemble"
	"github.com/shukiv/gniza/internal/resticrun"
)

// accountKey carries the account a request belongs to, taken from the
// socket rather than from anything the browser said.
type accountKey struct{}

// accountOf is whose backups this request may see. Empty means the
// connection could not be attributed, which is refused.
func accountOf(r *http.Request) string {
	account, _ := r.Context().Value(accountKey{}).(string)
	return account
}

// ListenUser serves the account-facing interface.
//
// It is a second socket, not a second port: cpsrvd runs a cPanel user's
// plugin as that user, so the kernel can say who is connecting and this
// program never has to take the browser's word for it. The socket is
// writable by anyone, and every request is then confined to the account
// that opened it.
func (s *Server) ListenUser(ctx context.Context, socketPath string) error {
	// This one has to be reachable by accounts, so it lives in its own
	// directory. The operator's socket is in a different one, at 0700,
	// and neither listener can now change the other's boundary.
	dir := filepath.Dir(socketPath)
	if err := prepareSocketDir(dir, 0o755); err != nil {
		return err
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("webui: remove stale socket: %w", err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("webui: listen on %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o666); err != nil {
		_ = listener.Close()
		return fmt.Errorf("webui: open socket: %w", err)
	}
	// The per-account request limit below starts only after HTTP headers
	// arrive. Bound connections before parsing too, or a local account can
	// open sockets, send nothing, and consume the root service's file
	// descriptors until ReadHeaderTimeout expires them.
	listener = &peerLimitedListener{
		Listener: listener,
		budget: connectionBudget{
			maxTotal:  128,
			maxPerUID: 8,
			byUID:     map[uint32]int{},
		},
	}

	server := &http.Server{
		Handler:           s.UserHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    16 << 10,
		// A body has to arrive as well as start. Without this a local
		// process can open connections and dribble a form into them for
		// as long as it likes, and this service runs as root.
		ReadTimeout:  60 * time.Second,
		IdleTimeout:  60 * time.Second,
		WriteTimeout: 5 * time.Minute,
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			account, err := s.peerAccount(conn)
			if err != nil {
				s.log.Warn("user interface: unattributed connection", "error", err)
				return ctx
			}
			return context.WithValue(ctx, accountKey{}, account)
		},
	}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	s.log.Info("user interface listening", "socket", socketPath)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// peerLimitedListener admits only a bounded number of Unix connections in
// total and from one UID. Rejected connections are closed before net/http
// allocates a request for them.
type peerLimitedListener struct {
	net.Listener
	budget connectionBudget
}

func (l *peerLimitedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		creds, err := peerCredentials(conn)
		if err != nil || !l.budget.acquire(creds.Uid) {
			_ = conn.Close()
			continue
		}
		return &budgetedConn{
			Conn: conn,
			release: func() {
				l.budget.release(creds.Uid)
			},
		}, nil
	}
}

type budgetedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *budgetedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

type connectionBudget struct {
	mu        sync.Mutex
	total     int
	byUID     map[uint32]int
	maxTotal  int
	maxPerUID int
}

func (b *connectionBudget) acquire(uid uint32) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.byUID == nil {
		b.byUID = map[uint32]int{}
	}
	if b.total >= b.maxTotal || b.byUID[uid] >= b.maxPerUID {
		return false
	}
	b.total++
	b.byUID[uid]++
	return true
}

func (b *connectionBudget) release(uid uint32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.byUID[uid] <= 1 {
		delete(b.byUID, uid)
	} else {
		b.byUID[uid]--
	}
	if b.total > 0 {
		b.total--
	}
}

// UserHandler is what a cPanel account sees: its own backups, and nothing
// else on the server.
func (s *Server) UserHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.userPage(s.handleUserHome))
	mux.HandleFunc("GET /browse", s.userPage(s.handleUserBrowse))
	mux.HandleFunc("GET /logs", s.userPage(s.handleUserLogs))
	mux.HandleFunc("POST /restore", s.userPage(s.userGuard(s.handleUserRestore)))
	mux.HandleFunc("GET /download", s.userPage(s.handleUserDownload))

	outer := http.NewServeMux()
	if s.daSessions != nil {
		// No capability to issue: the plugin carries the DirectAdmin
		// session itself, and names the page in the query.
		outer.Handle("/", s.requireUserCapability(directAdminPages(mux)))
		return outer
	}
	outer.HandleFunc("POST "+capabilityEndpoint, s.issueUserCapability)
	outer.Handle("/", s.requireUserCapability(mux))
	return outer
}

// userPage refuses anything that cannot be attributed to an account, and
// holds one account to one request at a time.
//
// Listing a repository or walking a snapshot runs restic against the
// destination. Without a limit, anything running as the account — a cron
// job, a compromised site — could keep this server and the backup server
// busy on its behalf indefinitely.
func (s *Server) userPage(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account := accountOf(r)
		if account == "" {
			http.Error(w, "Gniza could not tell which account this is. "+
				"Open it from inside your control panel.", http.StatusForbidden)
			return
		}
		if !s.userBusy.enter(account) {
			http.Error(w, "Gniza is already working on a request for this account. "+
				"Wait for it to finish.", http.StatusTooManyRequests)
			return
		}
		defer s.userBusy.leave(account)
		allowed, err := s.userFeatures.allowed(r.Context(), account)
		if err != nil {
			s.log.Error("account feature check failed", "account", account, "error", err)
			http.Error(w, "Gniza could not verify that this feature is enabled. "+
				"Ask your host.", http.StatusServiceUnavailable)
			return
		}
		if !allowed {
			http.Error(w, "Gniza is not enabled for this account.", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// inFlight counts what each account has running.
type inFlight struct {
	mu    sync.Mutex
	count map[string]int
	// limit is how many at once. One: a person clicking through a file
	// tree makes one request at a time, and anything making more is not
	// a person.
	limit int
}

func (f *inFlight) enter(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.count == nil {
		f.count = map[string]int{}
	}
	limit := f.limit
	if limit <= 0 {
		limit = 1
	}
	if f.count[key] >= limit {
		return false
	}
	f.count[key]++
	return true
}

func (f *inFlight) leave(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.count[key] <= 1 {
		delete(f.count, key)
		return
	}
	f.count[key]--
}

// userView is one account's own page.
type userView struct {
	Account      string
	Repositories []userRepository
	// Links is where this page's links point, which the panel decides.
	Links userLinks
	// Downloads says a copy can be had here, one way or the other. Where
	// it cannot, every control that would offer one stays off the page.
	Downloads bool
	// DeliversToHome says the copy is written into the account's own home
	// directory rather than handed to the browser.
	DeliversToHome bool
	// PutsBack says this panel can write a part of the account back into
	// the live account. Where it cannot, the page offers copies only.
	PutsBack bool
	// Panel names the control panel, for the copy that has to.
	Panel    string
	Restores []restoreRow
	// Backups is this account's own backup history, on the logs page.
	Backups     []userBackupRow
	Kinds       []granular.Kind
	Err         string
	TotalPoints int
	Ready       int
	Latest      time.Time

	// The browser, when one is open.
	Repository string
	Snapshot   string
	SnapshotAt time.Time
	Snapshots  []resticrun.Snapshot
	Path       string
	Root       string
	Up         string
	Entries    []browseEntry
	Kind       granular.Kind
	// Items is what this part of the account holds, for the parts that
	// are not files and so cannot be listed by browsing the snapshot.
	Items    []inventory.Item
	ItemsErr string

	// Basket is what has been chosen out of this restore point so far,
	// across all of its categories.
	Basket nodestore.Basket
}

// BasketRows is the basket in the order the categories are offered, so it
// reads the same way as the page above it.
func (v userView) BasketRows() []basketRow {
	return basketRows(v.Basket, userKinds, v.KindTitle)
}

// BasketCount is how many things are in the basket.
func (v userView) BasketCount() int { return v.Basket.Count() }

// BasketLabel is the button that starts the whole basket.
func (v userView) BasketLabel() string {
	if count := v.Basket.Count(); count != 1 {
		return fmt.Sprintf("Restore these %d items", count)
	}
	return "Restore this item"
}

// BasketCanApply reports whether everything in the basket can go back into
// the account.
func (v userView) BasketCanApply() bool { return v.PutsBack && basketCanApply(v.Basket) }

// BasketBlocker names the part that keeps the basket from being put back,
// so the reason is on the page rather than left to be guessed at.
func (v userView) BasketBlocker() string {
	return basketBlocker(v.Basket, userKinds, v.KindTitle)
}

// InBasket reports whether a category has already been chosen, so the
// button on it can say so.
func (v userView) InBasket(kind granular.Kind) bool { return inBasket(v.Basket, kind) }

// userRepository is one destination as an account sees it: how many
// backups of theirs are in it, and when the last one was taken.
type userRepository struct {
	ID        string
	Name      string
	Snapshots int
	Latest    time.Time
}

// userKindAccount is a UI choice rather than a granular plan: it rebuilds
// the complete cpmove archive, always as a download. It is the one choice a
// customer can never have written back for them: the whole archive goes to
// cPanel's restorepkg, which runs as root over a home directory the customer
// controls, and that is an operator's decision.
const userKindAccount granular.Kind = "account"

// KindTitle names account recovery choices in customer-facing language.
func (v userView) KindTitle(k granular.Kind) string {
	switch k {
	case userKindAccount:
		return "Full account"
	case granular.KindFiles:
		return "Home directory"
	case granular.KindMailbox:
		return "Email accounts"
	case granular.KindDatabase:
		return "Databases"
	case granular.KindDBUsers:
		return "Database users"
	}
	return k.Title()
}

// KindDescription gives each recovery choice enough context to be selected
// without opening it first.
func (v userView) KindDescription(k granular.Kind) string {
	switch k {
	case userKindAccount:
		return "Everything in this backup as one " + v.Panel + " account archive."
	case granular.KindFiles:
		return "The whole home directory, or only selected files and folders."
	case granular.KindCron:
		return "The account's scheduled cron jobs."
	case granular.KindDatabase:
		return "Choose one or more MySQL or MariaDB databases."
	case granular.KindDBUsers:
		return "Database users, authentication data, and grants."
	case granular.KindDomains:
		return "Domains, DNS zones, and their web-server configuration."
	case granular.KindDNS:
		return "DNS zone records for the account's domains."
	case granular.KindSSL:
		return "Certificates, private keys, and AutoSSL metadata."
	case granular.KindMailbox:
		return "Choose a mailbox or all mail for a domain."
	case granular.KindFTP:
		return "The account's FTP users and access configuration."
	}
	return "Recover this part of the account."
}

func (v userView) Selected(k granular.Kind) bool { return v.Kind == k }

// CanApply reports whether the chosen part of the account can be written
// back into it, rather than only handed over as a copy.
func (v userView) CanApply() bool { return v.PutsBack && v.Kind.CanApply() }

// HomeFile is where a delivered copy is, in the account's own terms: the
// directory under their home and the file in it.
func (v userView) HomeFile(row restoreRow) string {
	if row.DeliveredPath == "" {
		return ""
	}
	return filepath.Base(filepath.Dir(row.DeliveredPath)) + "/" + filepath.Base(row.DeliveredPath)
}

// HomeFolderURL opens the panel's file manager on the directory the
// copies are written into. DirectAdmin's own address for its file
// manager, which every skin answers at.
func (v userView) HomeFolderURL(row restoreRow) string {
	if row.DeliveredPath == "" {
		return ""
	}
	return "/CMD_FILE_MANAGER/" + filepath.Base(filepath.Dir(row.DeliveredPath))
}

// Here is where the picker is, in the account's own terms. Path is where
// the backup put it -- a home directory as it was named on the server that
// took it, or a staging directory belonging to root -- and neither is a
// place the customer has ever seen or can do anything with.
func (v userView) Here() string {
	label := "This backup"
	switch v.Kind {
	case granular.KindFiles:
		label = "Home directory"
	case granular.KindMailbox:
		label = "Mail"
	case granular.KindDatabase:
		label = "Databases"
	}
	if v.Root == "" || v.Path == "" {
		return label
	}
	below := strings.TrimPrefix(strings.TrimPrefix(v.Path, v.Root), "/")
	if below == "" {
		return label
	}
	return label + "/" + below
}

// PutBackLabel names the button that writes this part of the account back,
// in the words of the thing being restored.
func (v userView) PutBackLabel() string {
	switch v.Kind {
	case granular.KindDatabase:
		return "Restore these databases"
	case granular.KindMailbox:
		return "Restore this mail"
	case granular.KindFiles:
		return "Restore these files"
	case granular.KindDBUsers:
		return "Restore these database users"
	}
	return "Restore into my account"
}

// PutBackNote says what asking for this restore will do, for the kinds
// chosen whole rather than item by item.
func (v userView) PutBackNote() string {
	switch {
	case v.Kind == granular.KindDBUsers && v.PutsBack:
		return "The users come back with the passwords they had and the access they had " +
			"to this account's databases: a user that still exists is set back to the " +
			"password in the backup. Your own " + v.Panel + " login keeps its current password."
	case v.CanApply():
		return "This can go straight back into the account, or be downloaded as a copy."
	case v.DeliversToHome:
		return "A copy is saved in your home directory, where you can download it; send it to " +
			"your host, who can put it back."
	}
	return "Download it here and send it to your host, who can put it back."
}

func (v userView) NeedsNames() bool { return v.Kind.NeedsNames() }

// PicksItems reports whether the listed items can be chosen between,
// rather than only read.
func (v userView) PicksItems() bool { return v.Kind.PicksItems() }

// ItemsNote says what the list under a category means, which differs by
// whether the choice narrows the restore or only shows what is in it.
func (v userView) ItemsNote() string {
	if v.Kind.PicksItems() {
		return "Tick the ones you want. Leave every box empty to recover all of them."
	}
	if v.Kind == granular.KindCron {
		return "This is what the backup holds. Putting them back replaces your " +
			"scheduled jobs with these: they are lines in a single file, so a job " +
			"you added since this backup was taken would go. A copy of the jobs " +
			"being replaced is kept on the server, so your host can put one back."
	}
	return "This is what the backup holds. They come back together: a crontab and " +
		"the FTP logins are each one file, and handing back part of one would hand " +
		"back a file that is not the one in the backup."
}

// ItemsEmpty is what to say when a category holds nothing in this restore
// point, in the words of that category.
func (v userView) ItemsEmpty() string {
	switch v.Kind {
	case granular.KindDNS:
		return "This restore point holds no DNS zones for your account."
	case granular.KindSSL:
		return "This restore point holds no certificates for your account."
	case granular.KindDomains:
		return "This restore point holds no domains for your account."
	case granular.KindCron:
		return "There were no cron jobs on the account when this backup was taken."
	case granular.KindFTP:
		return "There were no FTP logins on the account when this backup was taken."
	case granular.KindDBUsers:
		return "This restore point holds no database users for your account."
	}
	return "This restore point holds nothing here."
}
func (v userView) RestoreTitle(row restoreRow) string {
	if selections := row.Selections(); len(selections) > 0 {
		titles := make([]string, 0, len(selections))
		for _, selection := range selections {
			titles = append(titles, v.KindTitle(granular.Kind(selection.Kind)))
		}
		return granular.JoinAnd(titles)
	}
	if row.Kind == protocol.RestoreAccount {
		return v.KindTitle(userKindAccount)
	}
	return "Recovery package"
}

// userViewFor starts a page for the account behind a request, with what
// every account page needs to know about the panel it is served on.
func (s *Server) userViewFor(r *http.Request) userView {
	return userView{
		Account:        accountOf(r),
		Kinds:          s.offeredUserKinds(),
		Links:          s.userLinks(),
		Downloads:      s.accountCopies(),
		DeliversToHome: s.deliversToHome(),
		PutsBack:       s.putsBack(),
		Panel:          s.panelName(),
	}
}

func (s *Server) handleUserHome(w http.ResponseWriter, r *http.Request) {
	view := s.userViewFor(r)
	destinations, err := s.destinationViews()
	if err != nil {
		s.failUser(w, r, http.StatusInternalServerError, err)
		return
	}
	for _, dest := range destinations {
		if dest.Repository.ID == "" {
			continue
		}
		row := userRepository{ID: dest.Repository.ID, Name: dest.Name}
		snapshots, err := s.engine.UserSnapshots(r.Context(), dest.Repository.ID, view.Account)
		if err != nil {
			s.log.Error("list account snapshots", "account", view.Account,
				"repository", dest.Repository.ID, "error", err)
			view.Err = "One of your backup destinations could not be read. Ask your host to check it."
		}
		row.Snapshots = len(snapshots)
		view.TotalPoints += len(snapshots)
		for _, snapshot := range snapshots {
			if snapshot.Time.After(row.Latest) {
				row.Latest = snapshot.Time
			}
			if snapshot.Time.After(view.Latest) {
				view.Latest = snapshot.Time
			}
		}
		view.Repositories = append(view.Repositories, row)
	}

	// Only this account's own restores, and only the ones with something
	// to collect.
	restores, err := s.engine.Store().Restores(50)
	if err != nil {
		s.failUser(w, r, http.StatusInternalServerError, err)
		return
	}
	for _, restore := range restores {
		if restore.Account != view.Account {
			continue
		}
		// The name is not the account. What the customer before them
		// recovered stays on the server for whoever runs it, and is not
		// listed here to the next holder of the name.
		if !s.engine.BelongsToCurrentHolder(restore) {
			continue
		}
		collectable := s.collectable(restore)
		view.Restores = append(view.Restores, restoreRow{
			Restore:     accountSafeRestore(restore),
			Collectable: collectable,
		})
		if collectable {
			view.Ready++
		}
	}
	s.renderUser(w, r, "user_home.html", view)
}

// userKinds are the parts of their own account a customer can ask for.
// The server's own settings are not among them: they are not this
// account's, and no account may see them.
var userKinds = []granular.Kind{
	userKindAccount,
	granular.KindFiles,
	granular.KindCron,
	granular.KindDatabase,
	granular.KindDBUsers,
	granular.KindDomains,
	granular.KindDNS,
	granular.KindSSL,
	granular.KindMailbox,
	granular.KindFTP,
}

// offeredUserKinds is the part of that list this panel can do anything
// with. Where a copy can be had, every kind can: a copy is what the
// kinds that cannot be put back are for. Where it cannot, a kind that
// can only be handed over as a copy is nothing to offer: a card that
// leads to no button is a question with no answer.
func (s *Server) offeredUserKinds() []granular.Kind {
	if s.accountCopies() {
		return userKinds
	}
	if !s.putsBack() {
		return nil
	}
	var offered []granular.Kind
	for _, kind := range userKinds {
		if kind != userKindAccount && kind.CanApply() {
			offered = append(offered, kind)
		}
	}
	return offered
}

// offersUserKind says whether this panel's account page shows a kind.
func (s *Server) offersUserKind(kind granular.Kind) bool {
	for _, offered := range s.offeredUserKinds() {
		if offered == kind {
			return true
		}
	}
	return false
}

func (s *Server) handleUserBrowse(w http.ResponseWriter, r *http.Request) {
	view := s.userViewFor(r)
	view.Repository = r.URL.Query().Get("repository")
	view.Snapshot = r.URL.Query().Get("snapshot")
	view.Path = r.URL.Query().Get("path")
	view.Kind = granular.Kind(r.URL.Query().Get("item"))
	if view.Repository == "" {
		destinations, err := s.destinationViews()
		if err != nil {
			s.failUser(w, r, http.StatusInternalServerError, err)
			return
		}
		for _, destination := range destinations {
			if destination.Repository.ID != "" && destination.Repository.InitialisedAt != nil {
				view.Repository = destination.Repository.ID
				break
			}
		}
	}
	if view.Repository == "" {
		view.Err = "No backup destination is ready for your account yet."
		s.renderUser(w, r, "user_browse.html", view)
		return
	}

	snapshots, err := s.engine.UserSnapshots(r.Context(), view.Repository, view.Account)
	if err != nil {
		s.log.Error("browse account snapshots", "account", view.Account,
			"repository", view.Repository, "error", err)
		view.Err = "This backup destination could not be read. Ask your host to check it."
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Time.After(snapshots[j].Time) })
	view.Snapshots = snapshots
	if len(snapshots) == 0 {
		view.Err = "No restore points are available for your account in this destination yet."
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	if view.Snapshot == "" && len(snapshots) > 0 {
		view.Snapshot = snapshots[0].ID
	}

	snapshot, found := findSnapshot(snapshots, view.Snapshot)
	if !found {
		view.Err = "That backup is not in this destination."
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	view.SnapshotAt = snapshot.Time
	if basket, err := s.engine.Store().Basket(nodestore.BasketOfAccount, view.Account, view.Repository, view.Snapshot); err != nil {
		s.log.Error("read the recovery basket", "account", view.Account, "error", err)
	} else {
		view.Basket = basket
	}
	if view.Kind == "" {
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	if !s.offersUserKind(view.Kind) {
		// Nothing of it is drawn: a form for a kind the page does not
		// offer would carry buttons that lead nowhere.
		view.Err = "That recovery option is not available for your account."
		view.Kind = ""
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	// The whole account is one thing; there is nothing in it to pick.
	if view.Kind == userKindAccount {
		s.renderUser(w, r, "user_browse.html", view)
		return
	}

	parts, err := reassemble.Classify(snapshot.Paths)
	if err != nil {
		s.log.Error("classify account snapshot", "account", view.Account,
			"repository", view.Repository, "snapshot", snapshot.ID, "error", err)
		view.Err = "This backup has an unexpected layout. Ask your host to check it."
		s.renderUser(w, r, "user_browse.html", view)
		return
	}

	// The parts of an account that are not files -- its DNS zones, its
	// certificates, its cron jobs, its FTP logins, its database users --
	// are inside one archive or one file in the backup, so they are read
	// rather than browsed. A page that named the category and nothing in
	// it could not answer the question somebody actually has, which is
	// whether the thing they lost is in this restore point.
	if !view.Kind.NeedsNames() {
		if view.Kind.ListsItems() {
			items, err := s.engine.Items(r.Context(), view.Repository, snapshot.ID, parts, view.Kind)
			if err != nil {
				s.log.Error("list what a backup holds", "account", view.Account,
					"repository", view.Repository, "snapshot", snapshot.ID,
					"kind", view.Kind, "error", err)
				view.ItemsErr = "What this restore point holds here could not be read. " +
					"You can still recover this part of your account, or ask your host to check the backup."
			}
			view.Items = items
		}
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	root := userPickerRoot(view.Kind, parts)
	if root == "" {
		view.Err = "This backup does not contain that part of your account."
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	view.Path = root
	view.Root = root
	if asked := path.Clean(r.URL.Query().Get("path")); asked != "." && asked != "" {
		if asked == root || strings.HasPrefix(asked, root+"/") {
			view.Path = asked
		}
	}
	view.Up = parentWithin(view.Path, root)

	entries, err := s.engine.Browse(r.Context(), view.Repository, snapshot.ID, view.Path)
	if err != nil {
		s.log.Error("browse account backup", "account", view.Account,
			"repository", view.Repository, "snapshot", snapshot.ID, "error", err)
		view.Err = "That part of the backup could not be read. Ask your host to check it."
		s.renderUser(w, r, "user_browse.html", view)
		return
	}
	if view.Kind == granular.KindMailbox && view.Path == root {
		view.Entries = append(view.Entries, browseEntry{
			Name: "The account mailbox and all addresses",
			Path: root, Item: ".",
		})
	}
	for _, entry := range entries {
		if entry.Path == view.Path {
			continue
		}
		if view.Kind == granular.KindMailbox && view.Path == root &&
			(!entry.IsDir() || maildirInternal(entry.Name)) {
			continue
		}
		item := itemName(view.Kind, entry, parts)
		if view.Kind == granular.KindDatabase && item == "" {
			continue
		}
		view.Entries = append(view.Entries, browseEntry{
			Name: entry.Name, Path: entry.Path, Size: entry.Size,
			Dir: entry.IsDir(), Item: item,
		})
	}
	sort.Slice(view.Entries, func(i, j int) bool {
		if view.Entries[i].Dir != view.Entries[j].Dir {
			return view.Entries[i].Dir
		}
		return view.Entries[i].Name < view.Entries[j].Name
	})
	s.renderUser(w, r, "user_browse.html", view)
}

func userPickerRoot(kind granular.Kind, parts reassemble.Parts) string {
	switch kind {
	case granular.KindFiles:
		return parts.Homedir
	case granular.KindMailbox:
		if parts.Homedir == "" {
			return ""
		}
		return path.Join(parts.Homedir, "mail")
	case granular.KindDatabase:
		return parts.Databases
	default:
		return ""
	}
}

// handleUserRestore queues a restore of part of the account that asked for
// it. The account comes from the socket, never from the form, so a request
// cannot be pointed at anybody else.
func (s *Server) handleUserRestore(w http.ResponseWriter, r *http.Request) {
	account := accountOf(r)
	// What an account may ask for is the list the page offers, checked
	// here rather than only rendered there. Raw panel settings are not a
	// customer-facing granular choice: that subset carries shadow,
	// digestshadow and cPanel internals without the context of a complete
	// cPanel account archive. A complete archive is handled explicitly.
	asked := granular.Kind(r.PostFormValue("item"))
	// Choosing across categories is a basket, not a restore: it is put
	// together over several pages and started once.
	switch action := r.PostFormValue("action"); action {
	case "add", "remove", "empty":
		s.changeUserBasket(w, r, action, asked)
		return
	}
	// Two buttons on one form: put it back, or hand me a copy. Anything
	// else that arrives here is a copy, because that is the one that
	// changes nothing.
	apply := r.PostFormValue("action") == "restore"
	// The page makes the tick box a condition of the button. Checking it
	// here as well means a request that reached this handler another way
	// cannot replace an account's data without one.
	if apply && r.PostFormValue("confirm") == "" {
		redirectUser(w, s.userLinks(), "/", "error",
			"Tick the box to confirm before restoring into your account.")
		return
	}
	// Where no copy can be had, a copy is a file nobody can collect; and
	// where the panel cannot put things back, asking it to is asking for
	// a refusal. The page offers no such button either way; a request
	// that asks anyway is told the same thing.
	if !apply && !s.accountCopies() {
		redirectUser(w, s.userLinks(), "/", "error",
			"Downloads are not available here. Tick the box to put it back into your account instead.")
		return
	}
	if apply && !s.putsBack() {
		redirectUser(w, s.userLinks(), "/", "error",
			"Putting a part of your account back is not available on "+s.panelName()+
				" yet. Leave the box unticked to save a copy instead.")
		return
	}
	var (
		restore    nodestore.Restore
		err        error
		fromBasket = r.PostFormValue("basket") != ""
		repository = r.PostFormValue("repository")
		snapshotID = r.PostFormValue("snapshot")
	)
	if fromBasket {
		var basket nodestore.Basket
		if basket, err = s.engine.Store().Basket(nodestore.BasketOfAccount, account, repository, snapshotID); err == nil {
			restore, err = userBasketRestore(account, basket, apply)
		}
	} else {
		restore, err = userRestoreRequest(account, repository, snapshotID,
			asked, r.PostForm["name"], apply)
	}
	if err != nil {
		if errors.Is(err, errUserRestoreNeedsNames) {
			back := "/browse?repository=" + url.QueryEscape(r.PostFormValue("repository")) +
				"&snapshot=" + url.QueryEscape(r.PostFormValue("snapshot")) +
				"&item=" + url.QueryEscape(string(asked))
			redirectUser(w, s.userLinks(), back, "error", "Choose at least one item to recover.")
			return
		}
		redirectUser(w, s.userLinks(), "/", "error", "That is not something you can restore here.")
		return
	}

	// The backup has to be one of theirs. A name that has changed hands
	// still has the previous owner's snapshots in the repository, and
	// hiding them from the page while restoring them on request would be
	// no protection at all.
	if err := s.engine.OwnsSnapshot(r.Context(), restore.RepositoryID, account,
		restore.SnapshotID); err != nil {
		redirectUser(w, s.userLinks(), "/", "error", "That backup is not one of yours.")
		return
	}
	// A copy on a panel that cannot hand one to the browser goes into
	// the home directory instead, as the account, when it is ready.
	restore.DeliverToHome = !restore.Apply && s.deliversToHome()
	if _, err := s.engine.QueueRestore(restore); err != nil {
		s.log.Error("queue account restore", "account", account, "error", err)
		message := "The restore could not be started. Ask your host to check it."
		if strings.Contains(err.Error(), "already has work in flight") {
			message = "A backup or restore is already running for your account. Wait for it to finish."
		}
		redirectUser(w, s.userLinks(), "/", "error", message)
		return
	}
	if fromBasket {
		// It has been asked for, so it is no longer a choice being made.
		// Leaving it would offer the same restore again on the next visit.
		if err := s.engine.Store().EmptyBasket(nodestore.BasketOfAccount, account, repository, snapshotID); err != nil {
			s.log.Error("empty the recovery basket", "account", account, "error", err)
		}
	}
	if restore.Apply {
		redirectUser(w, s.userLinks(), "/", "ok",
			"Started. When it finishes, this part of your account will be back as it "+
				"was in that backup.")
		return
	}
	if restore.DeliverToHome {
		redirectUser(w, s.userLinks(), "/", "ok",
			"Started. When it finishes, the copy will be in your home directory under "+
				"gniza-downloads, and listed below; nothing on your account is changed.")
		return
	}
	redirectUser(w, s.userLinks(), "/", "ok",
		"Started. What it recovers will appear below to download; nothing on your account "+
			"is changed.")
}

// collectable says whether a finished copy is there for the account to
// take: on the server, to download, or in their home directory.
func (s *Server) collectable(restore nodestore.Restore) bool {
	if s.deliversToHome() {
		return restore.DeliveredPath != "" && onDisk(restore.DeliveredPath)
	}
	return restore.ArchivePath != "" && onDisk(restore.ArchivePath)
}

// redirectUser keeps account-side navigation in cPanel's .live.php entry
// points. The WHM interface uses a single CGI and a ?p= route; using that
// redirect here would send a GET to restore.live.php after a POST, which is
// not a valid account capability and strands the user on a 403 page.
func redirectUser(w http.ResponseWriter, links userLinks, route, kind, message string) {
	route = strings.TrimPrefix(route, "/")
	query := url.Values{}
	if base, extra, found := strings.Cut(route, "?"); found {
		route = base
		if parsed, err := url.ParseQuery(extra); err == nil {
			query = parsed
		}
	}
	if route != "browse" {
		route = "index"
	}
	query.Set("kind", kind)
	query.Set("msg", message)
	w.Header().Set("Location", links.withQuery(route, query))
	w.WriteHeader(http.StatusSeeOther)
}

// userLinks is where the account pages' links point, which the panel
// decides. On cPanel each page is its own .live.php file and the proxy
// behind it forwards the path. On DirectAdmin a plugin has one address
// and only the query travels -- the admin page measured ?p=accounts
// arriving as QUERY_STRING at /CMD_PLUGINS_ADMIN/gniza/index.html and
// inside Evolution's /evo/plugin?src= wrapper alike -- so the page is
// named in it as p=, which directAdminPages turns back into the route.
type userLinks struct{ cpanel bool }

func (s *Server) userLinks() userLinks { return userLinks{cpanel: s.daSessions == nil} }

// URL is the address of one account page. Pairs are name, value, name,
// value; a value that is not a string is written as text.
func (l userLinks) URL(route string, pairs ...any) string {
	query := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		query.Set(fmt.Sprint(pairs[i]), fmt.Sprint(pairs[i+1]))
	}
	return l.withQuery(route, query)
}

// PageField is what a GET form carries so that it lands on the page it
// was shown on. A browser builds a GET form's address from the form's
// fields alone and drops the query of whatever it submits to, so on
// DirectAdmin, where the page is named in the query, a form that does not
// carry p= as a field of its own arrives with no page named and is
// answered with the overview -- which is what changing the restore point
// did until this field was added. On cPanel the page is in the path and
// the form needs nothing.
func (l userLinks) PageField(route string) template.HTML {
	if l.cpanel {
		return ""
	}
	return template.HTML(`<input type="hidden" name="p" value="` + template.HTMLEscapeString(route) + `">`)
}

func (l userLinks) withQuery(route string, query url.Values) string {
	route = strings.Trim(route, "/")
	if route == "" {
		route = "index"
	}
	if l.cpanel {
		if len(query) == 0 {
			return route + ".live.php"
		}
		return route + ".live.php?" + query.Encode()
	}
	query.Set("p", route)
	return "?" + query.Encode()
}

// userRoutes are the account pages as a DirectAdmin query names them.
var userRoutes = map[string]string{
	"index":    "/",
	"browse":   "/browse",
	"logs":     "/logs",
	"restore":  "/restore",
	"download": "/download",
}

// directAdminPages turns the one address a DirectAdmin plugin has into
// the account interface's routes: p= says which page, and the rest of the
// query is the page's own. The query is left whole, p= included, because
// the page's live refresh is fetched from the address it arrived at
// (liveURLFor) and has to name the same page.
func directAdminPages(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		page := r.URL.Query().Get("p")
		if page == "" {
			page = "index"
		}
		route, known := userRoutes[page]
		if !known {
			http.NotFound(w, r)
			return
		}
		routed := new(http.Request)
		*routed = *r
		address := *r.URL
		address.Path = route
		routed.URL = &address
		next.ServeHTTP(w, routed)
	})
}

// accountDownloads says whether the page can hand the browser a file.
// DirectAdmin wraps everything a plugin prints in its skin, so a restore
// archive cannot reach the browser as itself there.
func (s *Server) accountDownloads() bool { return s.daSessions == nil }

// panelFacts is what the engine would say about the panel, for a test
// that has no engine to ask.
type panelFacts struct {
	deliversToHome bool
	putsBack       bool
}

// deliversToHome says whether a copy is written into the account's own
// home directory instead, where its file manager and FTP already look.
func (s *Server) deliversToHome() bool {
	if s.facts != nil {
		return s.facts.deliversToHome
	}
	return s.engine != nil && s.engine.DeliversToHome()
}

// accountCopies says whether an account can have a copy at all, one way
// or the other.
func (s *Server) accountCopies() bool { return s.accountDownloads() || s.deliversToHome() }

// putsBack says whether this panel can write a part of an account back
// into the live account. Where it cannot, the page offers copies only.
func (s *Server) putsBack() bool {
	if s.facts != nil {
		return s.facts.putsBack
	}
	return s.engine == nil || s.engine.PutsBack()
}

var errUserRestoreNeedsNames = errors.New("account recovery needs at least one item")

// userRestoreRequest converts the account page's narrow vocabulary into an
// engine request. In particular, "account" is the full archive flow rather
// than an unrecognised granular kind.
//
// apply is the customer asking for the backup to replace what is live
// rather than to be handed over as a copy. It is honoured only for the
// parts of an account that can be written back, and never for the complete
// account archive: that one goes to cPanel's restorepkg, which runs as root
// over an archive whose home directory the customer controls, and it is not
// a decision the customer makes on their own. An apply that cannot be
// granted is refused rather than downgraded to a download, because the
// request said "replace this" and reporting success over a copy would tell
// somebody their site was fixed when it was not.
func userRestoreRequest(account, repository, snapshot string, asked granular.Kind,
	names []string, apply bool) (nodestore.Restore, error) {

	if !isUserKind(asked) {
		return nodestore.Restore{}, fmt.Errorf("account recovery kind %q is not allowed", asked)
	}
	if apply && !asked.CanApply() {
		return nodestore.Restore{}, fmt.Errorf(
			"account recovery kind %q cannot be written into the live account", asked)
	}
	restore := nodestore.Restore{
		Account: account, RepositoryID: repository, SnapshotID: snapshot,
		Apply: apply,
	}
	if asked == userKindAccount {
		restore.Kind = protocol.RestoreAccount
		return restore, nil
	}
	restore.Kind = protocol.RestoreItems
	restore.ItemKind = string(asked)
	for _, name := range names {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			restore.ItemNames = append(restore.ItemNames, trimmed)
		}
	}
	if asked.NeedsNames() && len(restore.ItemNames) == 0 {
		return nodestore.Restore{}, errUserRestoreNeedsNames
	}
	// A kind that lists what it holds without being able to hand back part
	// of it -- a crontab is one file, and so is the FTP password file --
	// takes no names. Carrying them would say the choice was honoured.
	if !asked.PicksItems() {
		restore.ItemNames = nil
	}
	if err := usableItemNames(asked, restore.ItemNames); err != nil {
		return nodestore.Restore{}, err
	}
	return restore, nil
}

// accountSafeRestore removes root-side diagnostics before a restore record
// is rendered to a customer. WHM retains the original record and the logs
// retain its error; repository addresses and staging paths do not belong in
// an account-facing page.
func accountSafeRestore(restore nodestore.Restore) nodestore.Restore {
	restore.ArchivePath = ""
	restore.RestoredTo = ""
	if restore.Error == "" {
		return restore
	}
	// Hint is the one thing written for the customer, and it is kept: when
	// there is something they can do about the failure, telling them to ask
	// their host instead is the wrong answer.
	restore.Error = "The restore failed. Ask your host to check the backup service."
	if restore.Hint != "" {
		restore.Error = restore.Hint
	}
	restore.Detail = ""
	return restore
}

// handleUserDownload hands over a restore this account asked for.
// isUserKind reports whether a customer may ask for this part of their
// own account.
func isUserKind(kind granular.Kind) bool {
	for _, offered := range userKinds {
		if offered == kind {
			return true
		}
	}
	return false
}

func (s *Server) handleUserDownload(w http.ResponseWriter, r *http.Request) {
	restore, err := s.engine.Store().Restore(r.URL.Query().Get("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if restore.Account != accountOf(r) || !s.engine.BelongsToCurrentHolder(restore) {
		// Not theirs -- either another account's, or the same name's
		// previous holder's. Say nothing about whose it is.
		http.NotFound(w, r)
		return
	}
	if !s.accountDownloads() {
		// Nothing on this panel links here, and the panel would wrap
		// the archive in its skin if something did.
		http.NotFound(w, r)
		return
	}
	s.handleDownload(w, r)
}

// changeUserBasket adds a category to the basket, takes one out, or empties
// it, and returns to the page it was chosen on.
//
// Choosing runs across several pages and the pages remember nothing between
// them, so the basket is kept on this server against the restore point it
// was chosen from. What is stored is a list of names; it belongs to one
// cPanel account, which is the account this request arrived as and never a
// name in the form.
func (s *Server) changeUserBasket(w http.ResponseWriter, r *http.Request,
	action string, asked granular.Kind) {

	account := accountOf(r)
	repository := r.PostFormValue("repository")
	snapshot := r.PostFormValue("snapshot")
	back := "/browse?repository=" + url.QueryEscape(repository) +
		"&snapshot=" + url.QueryEscape(snapshot)
	if asked != "" {
		back += "&item=" + url.QueryEscape(string(asked))
	}

	if action == "empty" {
		if err := s.engine.Store().EmptyBasket(nodestore.BasketOfAccount, account, repository, snapshot); err != nil {
			s.log.Error("empty the recovery basket", "account", account, "error", err)
			redirectUser(w, s.userLinks(), back, "error", "That could not be changed. Try again.")
			return
		}
		redirectUser(w, s.userLinks(), back, "ok", "Your recovery basket is empty again.")
		return
	}

	if !s.offersUserKind(asked) || asked == userKindAccount {
		// A whole account is not one thing among others: it is everything,
		// and it is a download an operator decides on. And a kind this
		// panel does not offer is not one to collect either: on
		// DirectAdmin it would make the basket a download, which there
		// is no way to hand over.
		redirectUser(w, s.userLinks(), back, "error", "That is not something you can add to the basket.")
		return
	}
	if action == "remove" {
		if _, err := s.engine.Store().TakeFromBasket(nodestore.BasketOfAccount, account, repository, snapshot,
			string(asked)); err != nil {
			s.log.Error("change the recovery basket", "account", account, "error", err)
			redirectUser(w, s.userLinks(), back, "error", "That could not be changed. Try again.")
			return
		}
		redirectUser(w, s.userLinks(), back, "ok", "Taken out of your recovery basket.")
		return
	}

	var names []string
	for _, name := range r.PostForm["name"] {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	// A kind that lists what it holds without being able to hand back part
	// of it takes no names, and one that is nothing without them cannot be
	// added until some are chosen.
	if !asked.PicksItems() {
		names = nil
	}
	if asked.NeedsNames() && len(names) == 0 {
		redirectUser(w, s.userLinks(), back, "error", "Choose at least one item first.")
		return
	}
	if err := usableItemNames(asked, names); err != nil {
		redirectUser(w, s.userLinks(), back, "error", "That is not something you can restore here.")
		return
	}

	basket, err := s.engine.Store().PutInBasket(nodestore.BasketOfAccount, account, repository, snapshot,
		nodestore.RestoreSelection{Kind: string(asked), Names: names})
	if err != nil {
		s.log.Error("change the recovery basket", "account", account, "error", err)
		redirectUser(w, s.userLinks(), back, "error", "That could not be added. Try again.")
		return
	}
	redirectUser(w, s.userLinks(), back, "ok", fmt.Sprintf(
		"Added. Your recovery basket now holds %d.", basket.Count()))
}

// userBasketRestore turns a basket into the one restore that empties it.
//
// It is one restore rather than several because the parts of an account
// depend on each other -- a database its user cannot open is what two
// restores produce when the second fails -- and because one account may
// only have one job in flight, so several would run one after another with
// gaps in between.
func userBasketRestore(account string, basket nodestore.Basket, apply bool) (nodestore.Restore, error) {
	if basket.Empty() {
		return nodestore.Restore{}, errors.New("the recovery basket is empty")
	}
	restore := nodestore.Restore{
		Account:      account,
		RepositoryID: basket.RepositoryID,
		SnapshotID:   basket.SnapshotID,
		Kind:         protocol.RestoreItems,
		Apply:        apply,
	}
	for _, item := range orderedSelections(basket, userKinds) {
		kind := granular.Kind(item.Kind)
		if !isUserKind(kind) || kind == userKindAccount {
			return nodestore.Restore{}, fmt.Errorf(
				"account recovery kind %q is not allowed", kind)
		}
		if apply && !kind.CanApply() {
			return nodestore.Restore{}, fmt.Errorf(
				"account recovery kind %q cannot be written into the live account", kind)
		}
		if err := usableItemNames(kind, item.Names); err != nil {
			return nodestore.Restore{}, err
		}
		restore.Items = append(restore.Items, nodestore.RestoreSelection{
			Kind: item.Kind, Names: item.Names,
		})
	}
	// A basket of one is still one selection, and reads on the history
	// page the same way a single restore always has.
	if len(restore.Items) == 1 {
		restore.ItemKind = restore.Items[0].Kind
		restore.ItemNames = restore.Items[0].Names
	}
	return restore, nil
}
