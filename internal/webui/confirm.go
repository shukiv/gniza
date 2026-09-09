package webui

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/shukiv/gniza/internal/granular"
)

// confirmation is the page that stands between a button and something
// that cannot be taken back.
//
// It is a page rather than a window.confirm for the two reasons the same
// shape already stands in front of forgetting an account's backups: the
// recovery centre works with scripting switched off, and a dialog nobody
// saw is no confirmation at all. The tick arrives as a form field the
// handler requires, so a request that reached the handler another way --
// a repeated POST, a script, a form saved from an earlier page -- is
// refused as well.
//
// It names what would be replaced rather than warning about restores in
// general. An operator who has just picked three databases out of
// Tuesday's backup should be reading those three names back, because that
// is the mistake this page exists to catch.
type confirmation struct {
	// Title is the question, in the form the answer is given to.
	Title string
	// Warning is what happens if it is answered yes.
	Warning string
	// Detail names what would be replaced, one thing per line.
	Detail []string
	// Action is where the confirmed request goes. It is the address this
	// request already arrived at, so the second request is the first one
	// with the tick added.
	Action string
	Tick   string
	Button string
	// Cancel is where "no" leads, which is the page the button was on.
	Cancel string
	// Section is which tab stays lit behind the question. Restores are
	// what usually asks, so that is what an empty one means.
	Section string
	Fields  []confirmField
}

// confirmField is one value of the held request, carried forward as a
// hidden field.
type confirmField struct{ Name, Value string }

// confirmed reports whether the operator has ticked the box.
//
// Checked in the handler and not only on the page: the page makes the tick
// a condition of the button, and this makes it a condition of the restore.
func confirmed(r *http.Request) bool { return r.PostFormValue("confirm") == "1" }

// askFirst renders the confirmation instead of doing the thing. The caller
// returns straight afterwards.
func (s *Server) askFirst(w http.ResponseWriter, r *http.Request, ask confirmation) {
	// The route travels in "p" and has already been unpacked into the
	// path by the time a handler runs, so the address to come back to is
	// the one this request arrived at. Deriving it here rather than
	// taking it from each caller is one thing that cannot disagree with
	// the handler it belongs to.
	ask.Action = linkTo(r.URL.Path)
	ask.Fields = heldFields(r)
	section := ask.Section
	if section == "" {
		section = "restore"
	}
	s.render(w, r, "confirm.html", "Confirm", section, ask)
}

// heldFields is the submitted request without the two fields the
// confirmation page writes for itself.
func heldFields(r *http.Request) []confirmField {
	names := make([]string, 0, len(r.PostForm))
	for name := range r.PostForm {
		if name == "csrf" || name == "confirm" {
			continue
		}
		names = append(names, name)
	}
	// Sorted so the page a test reads is the page an operator gets.
	sort.Strings(names)

	fields := make([]confirmField, 0, len(names))
	for _, name := range names {
		for _, value := range r.PostForm[name] {
			fields = append(fields, confirmField{Name: name, Value: value})
		}
	}
	return fields
}

// linkTo turns a route the handlers speak -- "/restore?account=x" -- into
// the address a browser needs. WHM serves the plugin behind a per-session
// token in the path and will not route anything after the .cgi name, so
// the route travels in "p". See docs/adr/0008.
func linkTo(path string) string {
	route := strings.TrimPrefix(path, "/")
	query := url.Values{}
	if base, extra, found := strings.Cut(route, "?"); found {
		route = base
		if parsed, err := url.ParseQuery(extra); err == nil {
			query = parsed
		}
	}
	query.Set("p", route)
	return "?" + query.Encode()
}

// restorePoint says which backup is about to be used, in the words the
// operator chose it by. An empty snapshot means the newest there is,
// which is what asking for an account rather than a hash means.
func restorePoint(snapshot string) string {
	if snapshot == "" {
		return "Restore point: the most recent backup there is."
	}
	if len(snapshot) > 12 {
		snapshot = snapshot[:12]
	}
	return "Restore point: " + snapshot + "."
}

// confirmWholeAccount describes handing a backup to the panel's own
// restore, which is the largest thing this interface can be asked to do.
//
// An account the panel no longer has is a different question with the same
// button: nothing on this server is replaced, because there is nothing
// here to replace. Saying "there is no undo" about that would be a warning
// the operator has to read past, and warnings that are read past stop
// being read.
//
// panelName is what the restore about to run is called. This warning sits
// above the one irreversible button here, and on a DirectAdmin server it
// named cPanel.
func confirmWholeAccount(panelName, account, snapshot, cancel string,
	gone, unrestricted bool) confirmation {
	ask := confirmation{
		Detail: []string{restorePoint(snapshot)},
		Cancel: cancel,
	}
	if unrestricted {
		ask.Detail = append(ask.Detail,
			"cPanel's restricted-restore checks are switched off for this run, so the "+
				"archive is restored as root without them.")
	}
	if gone {
		ask.Title = fmt.Sprintf("Create %s again?", account)
		ask.Warning = fmt.Sprintf("This hands the backup to %s's own restore, which "+
			"creates the account again with the files, databases, mail and settings the "+
			"backup holds. %s is not on this server now, so nothing here is replaced.",
			panelName, account)
		ask.Tick = fmt.Sprintf("Yes, create %s again", account)
		ask.Button = fmt.Sprintf("Create %s", account)
		return ask
	}
	ask.Title = fmt.Sprintf("Overwrite %s?", account)
	ask.Warning = fmt.Sprintf("This hands the backup to %s's own restore, which replaces "+
		"the live account. %s goes back to the files, databases, mail and settings the backup "+
		"holds, and anything added since the backup was taken is gone. There is no undo.",
		panelName, account)
	ask.Tick = fmt.Sprintf("Yes, overwrite %s", account)
	ask.Button = fmt.Sprintf("Overwrite %s", account)
	return ask
}

// namedAccounts lists what a bulk restore would go through, up to a point.
// Nineteen names is the list an operator wants to read back; two hundred
// is a wall, and a wall is scrolled past.
func namedAccounts(accounts []string) []string {
	const shown = 20
	if len(accounts) <= shown {
		return accounts
	}
	listed := append([]string(nil), accounts[:shown]...)
	return append(listed, fmt.Sprintf("and %d more", len(accounts)-shown))
}

// confirmManyAccounts describes the bulk restore, which is the one where a
// name nobody meant to tick is easiest to miss.
func confirmManyAccounts(panelName string, accounts []string, asOf, cancel string,
	unrestricted bool) confirmation {
	point := "Restore point: the most recent backup of each."
	if asOf != "" {
		point = "Restore point: the newest backup of each taken on or before " + asOf + "."
	}
	ask := confirmation{
		Title: fmt.Sprintf("Restore %s onto this server?", counted(len(accounts), "account")),
		Warning: fmt.Sprintf("Each of these is handed to %s's own restore. An account of "+
			"the same name on this server is replaced by the backup's copy, and anything "+
			"added since the backup was taken is gone. There is no undo.", panelName),
		Detail: append([]string{point}, namedAccounts(accounts)...),
		Tick:   fmt.Sprintf("Yes, restore %s onto this server", counted(len(accounts), "account")),
		Button: fmt.Sprintf("Restore %s", counted(len(accounts), "account")),
		Cancel: cancel,
	}
	if unrestricted {
		ask.Detail = append(ask.Detail,
			"cPanel's restricted-restore checks are switched off for this run.")
	}
	return ask
}

// confirmOnePart describes putting one part of an account back.
func confirmOnePart(account string, kind granular.Kind, names []string,
	snapshot, cancel string) confirmation {

	// The kind's own name starts a heading elsewhere -- "A database" --
	// and this is the middle of a sentence.
	title := lowerFirst(kind.Title())
	detail := []string{restorePoint(snapshot)}
	if len(names) == 0 {
		detail = append(detail, "Everything this backup holds in "+title+".")
	} else {
		detail = append(detail, names...)
	}
	return confirmation{
		Title: fmt.Sprintf("Put %s back into %s?", title, account),
		Warning: fmt.Sprintf("This replaces that part of the live account with the backup's "+
			"copy. Anything changed in it since the backup was taken is gone, and the rest "+
			"of %s is left as it is. There is no undo.", account),
		Detail: detail,
		Tick:   fmt.Sprintf("Yes, put %s back into %s", title, account),
		Button: "Put it back",
		Cancel: cancel,
	}
}

// lowerFirst drops the capital off a name written to start a line, so it
// can be read in the middle of one.
func lowerFirst(s string) string {
	for i, r := range s {
		return string(unicode.ToLower(r)) + s[i+utf8.RuneLen(r):]
	}
	return s
}

// confirmBasket describes putting a whole basket back, which is several
// parts of an account written as one restore.
func confirmBasket(account string, rows []basketRow, snapshot, cancel string) confirmation {
	detail := []string{restorePoint(snapshot)}
	for _, row := range rows {
		if len(row.Names) == 0 {
			detail = append(detail, row.Title+": all of it")
			continue
		}
		detail = append(detail, row.Title+": "+strings.Join(row.Names, ", "))
	}
	return confirmation{
		Title: fmt.Sprintf("Put the basket back into %s?", account),
		Warning: "Everything in the basket goes back into the live account as one restore. " +
			"What is there now is replaced by the backup's copy, and anything added since " +
			"the backup was taken is gone. There is no undo.",
		Detail: detail,
		Tick:   fmt.Sprintf("Yes, put all of it back into %s", account),
		Button: "Put the basket back",
		Cancel: cancel,
	}
}

// counted writes "1 account" and "19 accounts", so the page reads as a
// sentence rather than as a template with a number in it.
func counted(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// accountIsGone reports whether cPanel has removed the account, which
// decides what restoring it means. A store that cannot be read says no:
// the warning that assumes a live account is the careful one of the two.
func (s *Server) accountIsGone(account string) bool {
	identity, err := s.engine.Store().Identity(account)
	if err != nil {
		return false
	}
	return identity.RetiredAt != nil
}
