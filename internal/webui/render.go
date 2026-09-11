package webui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shukiv/gniza/internal/agent"
	"github.com/shukiv/gniza/internal/answer"
	"github.com/shukiv/gniza/internal/human"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/notify"
	"github.com/shukiv/gniza/internal/update"
)

// page is the data every template receives.
type page struct {
	Title string
	Nav   string
	CSRF  string
	Flash *flash
	Data  any
	// RunningVersion is this binary's build, not the latest published release.
	RunningVersion string
	// Running is every backup and restore happening now. It is on the
	// page rather than in one place that lists runs, because somebody
	// who has just asked for one and sees nothing asks again.
	Running runningSet
	// Update is a published release newer than this build, when there is
	// one. It is on every operator page rather than on a page somebody
	// would have to think to visit: a backup program running a version
	// with a known fault should say so where it is being used.
	Update *updateNotice
	Assets assets
	// LiveURL is where the page fetches itself from to keep current, when
	// the browser's own address would not answer with it. See liveURLFor.
	LiveURL string
	// Fonts are the @font-face rules for this request. Where the
	// typefaces are fetched from depends on the panel, so they are
	// written per request rather than embedded in the stylesheet.
	Fonts template.CSS
	// Panel is what the control panel this runs on is called. Templates
	// that name it say {{$.Panel}}: the same page is served on cPanel and
	// on DirectAdmin, and it used to say cPanel on both.
	Panel string
	// Cpanel says the panel is cPanel, for the pages that name one of
	// its mechanisms -- pkgacct, restorepkg, Restricted Restore, the WHM
	// plugin, the cPanel hooks. Those are not products with another
	// name on DirectAdmin: they do not exist there, so the copy that
	// names them is written twice rather than substituted.
	Cpanel bool
}

// updateNotice is a release newer than the one running here.
type updateNotice struct {
	Version string
	Current string
	URL     string
}

// flash is a one-shot message shown at the top of a page.
type flash struct {
	Kind    string // ok, warn, error
	Message string
}

// render writes a page, or an error page if the template fails.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name, title, nav string, data any) {
	s.renderWithCSRF(w, r, name, title, nav, data, s.csrfToken)
}

func (s *Server) renderWithCSRF(
	w http.ResponseWriter, r *http.Request, name, title, nav string, data any, csrf string,
) {
	s.renderStatus(w, r, http.StatusOK, name, title, nav, data, csrf)
}

// renderStatus draws a page under a status of the caller's choosing. The
// status is written after the headers and before the body: an error page
// whose status was written first went out without its content type.
func (s *Server) renderStatus(
	w http.ResponseWriter, r *http.Request, status int, name, title, nav string, data any, csrf string,
) {
	// These pages show repository passwords and private keys. A shared
	// browser, a back button, or a proxy that keeps a copy would each be
	// a way for the key to the backups to outlive the session that was
	// allowed to see it.
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	view := page{
		Title: title, Nav: nav, CSRF: csrf,
		Flash: flashFrom(r), Data: data, Assets: s.assets,
		Fonts:          fontFaces(fontBaseFor(r)),
		LiveURL:        liveURLFor(r),
		RunningVersion: agent.Version,
		Panel:          s.panelName(),
		Cpanel:         s.panelName() == "cPanel",
	}
	// An account-facing request is confined to the account that opened
	// the socket, here as everywhere: another customer's restore is not
	// theirs to see. An operator's request carries no account and sees
	// the server's work.
	//
	// A store that cannot be read is not a reason to fail the page: the
	// strip is a convenience, and every page still says what it says.
	if s.engine != nil {
		if running, err := runningWorkFor(s.engine.Store(), accountOf(r)); err != nil {
			s.log.Error("read what is running", "error", err)
		} else {
			view.Running = running
		}
		// Operators only. A customer cannot upgrade this server and has
		// no use for knowing which version it runs.
		if accountOf(r) == "" {
			view.Update = s.updateNotice()
		}
	}

	if wantsData(r) {
		s.answer(w, status, view)
		return
	}

	// Rendered to a buffer first: a template that fails halfway through
	// would otherwise leave a half-written page with a 200 on it.
	set, known := s.templates[name]
	if !known {
		s.log.Error("no such template", "template", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var buffer bytes.Buffer
	if err := set.ExecuteTemplate(&buffer, "layout", view); err != nil {
		s.log.Error("render template", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The interface is served through WHM; nothing here should be framed
	// by anything else or sniffed into another type.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.WriteHeader(status)
	_, _ = buffer.WriteTo(w)
}

// wantsData says the request asked for the page as data rather than as
// HTML. The terminal interface asks; a browser never does.
func wantsData(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), answer.Header)
}

// answer writes the page as data: the same view the template would have
// drawn, without the stylesheet, the fonts or the navigation.
func (s *Server) answer(w http.ResponseWriter, status int, view page) {
	data, err := json.Marshal(view.Data)
	if err != nil {
		s.log.Error("answer a page as data", "page", view.Nav, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := answer.Page{
		Title: view.Title, Nav: view.Nav, CSRF: view.CSRF, Data: data,
		Version: view.RunningVersion, Panel: view.Panel,
	}
	if view.Flash != nil {
		out.Flash = &answer.Flash{Kind: view.Flash.Kind, Message: view.Flash.Message}
	}
	for _, work := range view.Running {
		out.Running = append(out.Running, answer.Running{
			Account: work.Account, Doing: work.Doing, Waiting: work.Waiting,
			Detail: work.Detail, Percent: work.Percent, Known: work.Known,
		})
	}
	if view.Update != nil {
		out.Update = &answer.Update{Version: view.Update.Version, Current: view.Update.Current, URL: view.Update.URL}
	}
	body, err := json.Marshal(out)
	if err != nil {
		s.log.Error("answer a page as data", "page", view.Nav, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", answer.Header+"; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// updateNotice reports a release newer than this build, or nil.
//
// Nothing here asks GitHub: the answer is whatever the daily check stored,
// so drawing a page never waits on somebody else's service.
func (s *Server) updateNotice() *updateNotice {
	// Turning the check off turns the banner off with it. Otherwise the
	// last answer it ever got would sit at the top of every page for
	// ever, and the tick would look broken.
	if settings, err := s.engine.Store().Settings(); err == nil && settings.NoUpdateCheck {
		return nil
	}
	state, err := s.engine.Store().UpdateState()
	if err != nil {
		s.log.Error("read the last update check", "error", err)
		return nil
	}
	if !update.Newer(agent.Version, state.Version) {
		return nil
	}
	return &updateNotice{Version: state.Version, Current: agent.Version, URL: state.URL}
}

// redirect sends the operator back to a page with a message.
//
// The target is a bare query string, so the browser resolves it against the
// current script. WHM serves the plugin behind a per-session token in the
// path, and cpsrvd will not route anything after the .cgi name — so every
// route travels in "p" rather than in the path. See docs/adr/0008.
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, path, kind, message string) {
	route := strings.TrimPrefix(path, "/")

	// A caller may pass "restore?account=x"; keep those parameters.
	query := url.Values{}
	if base, extra, found := strings.Cut(route, "?"); found {
		route = base
		if parsed, err := url.ParseQuery(extra); err == nil {
			query = parsed
		}
	}
	query.Set("p", route)
	query.Set("kind", kind)
	query.Set("msg", message)

	// Written directly rather than through http.Redirect, which resolves a
	// query-only reference against the request path and emits an absolute
	// one. WHM serves the plugin behind a /cpsessNNN token in the path, so
	// an absolute path drops the token and the browser lands on a 401.
	w.Header().Set("Location", "?"+query.Encode())
	w.WriteHeader(http.StatusSeeOther)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, err error) {
	s.log.Error("request failed", "path", r.URL.Path, "error", err)
	s.renderStatus(w, r, status, "error.html", "Problem", "", err.Error(), s.csrfToken)
}

// failUser keeps an account-side failure on the account side of the trust
// boundary. Backend errors can contain repository addresses and root-owned
// paths, and the WHM error layout is allowed to carry privileged controls.
func (s *Server) failUser(w http.ResponseWriter, r *http.Request, status int, err error) {
	s.log.Error("account request failed", "account", accountOf(r),
		"path", r.URL.Path, "error", err)
	http.Error(w, "Gniza could not complete that request.", status)
}

func flashFrom(r *http.Request) *flash {
	message := r.URL.Query().Get("msg")
	if message == "" {
		return nil
	}
	kind := r.URL.Query().Get("kind")
	if kind != "ok" && kind != "warn" && kind != "error" {
		kind = "ok"
	}
	return &flash{Kind: kind, Message: message}
}

// renderUser draws an account-facing page. It uses the same machinery as
// the operator's pages and its own layout: a customer is not an operator,
// and none of the operator's navigation means anything to them.
func (s *Server) renderUser(w http.ResponseWriter, r *http.Request, name string, data any) {
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	nav := "overview"
	switch name {
	case "user_browse.html":
		nav = "restore"
	case "user_logs.html":
		nav = "logs"
	}
	s.renderWithCSRF(w, r, name, "Backups", nav, data, s.userCSRFToken(accountOf(r)))
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"bytes": humanBytes,
		// kindTitle and eventTitle name a stored channel's kind and
		// events the way the operator chose them, rather than showing
		// the identifiers they are stored as.
		"kindTitle": func(kind string) string { return notify.Kind(kind).Title() },
		"add":       func(a, b int) int { return a + b },
		// hostKeyFile turns an ssh key type into the name of the public
		// key file on the far side, so the page can say exactly what to
		// run there to check the fingerprint.
		"hostKeyFile": func(keyType string) string {
			name := strings.TrimPrefix(keyType, "ssh-")
			if base, _, found := strings.Cut(name, "-"); found {
				name = base
			}
			return name
		},
		// keepsSet and keepsSaid describe a retention policy in the words
		// an operator wrote it in, rather than as five numbers most of
		// which are zero.
		"keepsSet": func(keeps nodestore.Retention) bool {
			return keeps.KeepLast+keeps.KeepDaily+keeps.KeepWeekly+
				keeps.KeepMonthly+keeps.KeepYearly > 0
		},
		"keepsSaid": func(keeps nodestore.Retention) string {
			var parts []string
			for _, said := range []struct {
				count int
				unit  string
			}{
				{keeps.KeepLast, "most recent"}, {keeps.KeepDaily, "daily"},
				{keeps.KeepWeekly, "weekly"}, {keeps.KeepMonthly, "monthly"},
				{keeps.KeepYearly, "yearly"},
			} {
				if said.count > 0 {
					parts = append(parts, fmt.Sprintf("%d %s", said.count, said.unit))
				}
			}
			if len(parts) == 0 {
				return "nothing"
			}
			return strings.Join(parts, ", ")
		},
		"eventTitle": func(event string) string { return notify.Event(event).Title() },
		// channelWhere is the one detail that tells two channels of the
		// same kind apart, without ever showing a credential.
		"channelWhere": func(channel nodestore.Channel) string {
			switch notify.Kind(channel.Kind) {
			case notify.KindSMTP:
				return channel.Config["to"]
			case notify.KindNtfy:
				return channel.Config["topic"]
			case notify.KindTelegram:
				return channel.Config["chat_id"]
			case notify.KindWebhook:
				if parsed, err := url.Parse(channel.Config["url"]); err == nil && parsed.Host != "" {
					return parsed.Host
				}
			}
			return ""
		},
		// barwidth is the CSS for a progress bar's filled part. It is
		// built here rather than interpolated in the template so the
		// value is a number this program produced, not markup.
		"barwidth": func(percent float64) template.CSS {
			switch {
			case percent < 0:
				percent = 0
			case percent > 100:
				percent = 100
			}
			return template.CSS(fmt.Sprintf("width:%.1f%%", percent))
		},
		// pkgacctMeaning explains a probed flag in terms of what it does
		// to a backup. The names are cPanel's and read as though Gniza
		// were leaving something out; it is the opposite.
		"pkgacctMeaning": func(name string) string {
			switch name {
			case "nocompress":
				return "pkgacct can write its archive uncompressed, which is what lets " +
					"restic store only what changed between one night and the next."
			case "skipdb":
				return "pkgacct can leave databases out of its archive, so Gniza can dump " +
					"each one separately instead. They are backed up either way."
			case "skiphomedir":
				return "pkgacct can leave the home directory out of its archive, so Gniza " +
					"can back it up as files. It is backed up either way."
			case "skipmail":
				return "pkgacct can leave the account's mail messages out of its archive, " +
					"which a schedule that skips email needs."
			case "skipmailconfig":
				return "pkgacct can leave the mail configuration out of its archive. This is " +
					"the one that matters: the mailbox names and their password hashes are " +
					"in the configuration, not in the mail itself."
			}
			return ""
		},
		// took renders how long a run lasted, in the unit someone would
		// say it in.
		"took": humanTook,
		"percent": func(value float64) string {
			// Half rounds up: 42.5% reads as 43%, not 42%, which is what
			// anyone watching a bar expects of the number beside it.
			return fmt.Sprintf("%.0f%%", math.Round(value))
		},
		// Sort keys for the tables. What a cell shows and what it sorts
		// by are different things: "2 h ago" and "6.2 MiB new of 152.5
		// MiB" sort as nonsense, so the cell carries a number as well.
		"unix": func(t time.Time) int64 {
			if t.IsZero() {
				return 0
			}
			return t.Unix()
		},
		"unixPtr": func(t *time.Time) int64 {
			if t == nil {
				return 0
			}
			return t.Unix()
		},
		"addedBytes": func(targets []nodestore.JobTarget) uint64 {
			var total uint64
			for _, target := range targets {
				total += target.BytesAdded
			}
			return total
		},
		"ago": humanAgo,
		"agoTime": func(t time.Time) string {
			return humanAgo(&t)
		},
		"stamp": func(t time.Time) string {
			if t.IsZero() {
				return "never"
			}
			return t.Local().Format("2006-01-02 15:04")
		},
		"stampPtr": func(t *time.Time) string {
			if t == nil {
				return "—"
			}
			return t.Local().Format("2006-01-02 15:04")
		},
		"short": func(s string) string {
			if len(s) > 12 {
				return s[:12]
			}
			return s
		},
		"join":  strings.Join,
		"lower": strings.ToLower,
		"title": strings.Title, //nolint:staticcheck // ASCII labels only
	}
}

func humanBytes(value uint64) string { return human.Bytes(value) }

// humanTook renders how long a run lasted, in the unit somebody would say
// it in.
func humanTook(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func humanAgo(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	elapsed := time.Since(*t)
	switch {
	case elapsed < time.Minute:
		return "just now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%d min ago", int(elapsed.Minutes()))
	case elapsed < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(elapsed.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(elapsed.Hours()/24))
	}
}
