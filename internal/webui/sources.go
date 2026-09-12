package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/panel"
)

// The choosing of what to back up, on a server whose accounts are the
// operator's choice rather than a panel's list (ADR 0025). The same
// tables are drawn in two places: on the What to back up page, where a
// ticked row is added, and in the schedule form, where they say what
// one schedule covers and a fresh row ticked there is added on save.
// The handlers serve the socket, the browser door and the terminal.
//
// The tables are one tab per kind -- folders, MySQL, PostgreSQL,
// containers -- each a table of what there is with a box per row, and
// each ticked row is a source of its own.

// chooseTabs are the kinds, in the order the tabs show them.
var chooseTabs = []string{"folders", "mysql", "postgresql", "containers"}

// chooseView is what a page shows about choosing.
type chooseView struct {
	Candidates panel.Candidates
	// Mode is "add" on the What to back up page, where a row already
	// chosen is greyed out because it cannot be chosen twice, or
	// "schedule" in the schedule form, where such a row is a box that
	// puts the source under the schedule and posts its name.
	Mode string
	// Adding opens the form: asked for, or nothing chosen yet, or a
	// choice was refused and the form is shown again with the reason.
	Adding    bool
	FormError string
	// Tab is the kind the form opens on.
	Tab string
	// Refused says the form is drawn again after a refusal, so the boxes
	// are as they were ticked rather than as they are by default.
	Refused bool
	// Submitted is what the operator typed when the form was refused.
	Submitted map[string]string
	// Ticked is which boxes were ticked then, keyed "folder/<path>",
	// "mysql/<name>", "postgresql/<name>" and "container/<engine>/<name>".
	Ticked map[string]bool
	// Offers says whether any candidate at all was found, so an empty
	// form can say why it is empty.
	Offers bool
	// Sources is what has been chosen already. Extra are the ones no
	// candidate row stands for -- a folder outside the roots, a
	// container that is gone -- which the schedule form draws as rows
	// of their own, or it could not put them under a schedule.
	Sources []panel.Source
	Extra   []panel.Source
	// Chosen is which sources the schedule form draws ticked, by name.
	Chosen map[string]bool
	// Fresh says nothing has been chosen yet, so the schedule form ticks
	// the databases and containers the way the What to back up page
	// does, and the first schedule saved backs something up.
	Fresh bool
	// containerAs names the sources made from each container, comma
	// joined, keyed "<engine>/<name>".
	containerAs map[string]string
}

// FolderSources is every source with a folder that no container, stack
// or engine row stands for: the rows of the folders tab.
func (v chooseView) FolderSources() []panel.Source {
	elsewhere := map[string]bool{}
	for _, stack := range v.Candidates.Stacks {
		elsewhere[stack.ChosenAs] = true
	}
	for _, engine := range v.Candidates.Engines {
		elsewhere[engine.ChosenAs] = true
	}
	// A source from a container that is still there is that container's
	// row; one whose container is gone keeps its folder row here.
	for _, container := range v.Candidates.Containers {
		for _, name := range strings.Split(v.ContainerAs(container.Engine, container.Name), ",") {
			elsewhere[name] = true
		}
	}
	var folders []panel.Source
	for _, source := range v.Sources {
		if source.Path != "" && !elsewhere[source.Name] {
			folders = append(folders, source)
		}
	}
	return folders
}

func (v chooseView) Field(name string) string { return v.Submitted[name] }

// On says whether a box is ticked when the form is drawn: as it was
// when the form was refused, or else as the kind has it by default --
// every database and container on, since backing them up is what the
// operator came for, and every folder off, since the roots hold things
// like /opt/containerd too.
func (v chooseView) On(key string, byDefault bool) bool {
	if v.Refused {
		return v.Ticked[key]
	}
	return byDefault
}

// tickBox is one box in a table: what it posts and how it is drawn.
type tickBox struct {
	Name, Value, Label string
	Checked, Disabled  bool
	// Existing marks a box that stands for a source already chosen. The
	// schedule form ticks every box with the same value together, since
	// one source can stand behind several rows.
	Existing bool
}

// Box is the box for one row: on the What to back up page a chosen row
// is disabled and a fresh one posts its kind; in the schedule form a
// chosen row posts the source's name and a fresh one posts its kind, to
// be added on save.
func (v chooseView) Box(kind, value, chosenAs string, byDefault bool, label string) tickBox {
	box := tickBox{Name: kind, Value: value, Label: label}
	if v.Mode != "schedule" {
		if chosenAs != "" {
			box.Disabled = true
			return box
		}
		box.Checked = v.On(kind+"/"+value, byDefault)
		return box
	}
	if chosenAs != "" {
		box.Name, box.Value, box.Existing = "source", chosenAs, true
		for _, name := range strings.Split(chosenAs, ",") {
			if v.Chosen[name] {
				box.Checked = true
			}
		}
		return box
	}
	box.Checked = v.Fresh && byDefault
	return box
}

// Dim says a row is drawn greyed out: chosen already, on the page where
// that means it cannot be chosen again.
func (v chooseView) Dim(chosenAs string) bool { return v.Mode != "schedule" && chosenAs != "" }

// ContainerAs names the sources a container was chosen as, comma
// joined, or is empty.
func (v chooseView) ContainerAs(engine, name string) string {
	return v.containerAs[engine+"/"+name]
}

// load reads what there is to choose from and what has been chosen.
func (v *chooseView) load(ctx context.Context, chooser panel.Chooser) {
	candidates, err := chooser.Candidates(ctx)
	if err != nil && v.FormError == "" {
		v.FormError = "Could not see what there is to choose from: " + err.Error()
	}
	v.Candidates = candidates
	v.Offers = len(candidates.Folders)+len(candidates.MySQL)+len(candidates.PostgreSQL)+len(candidates.Containers) > 0
	sources, err := chooser.Sources(ctx)
	if err != nil {
		return
	}
	v.Sources = sources
	v.containerAs = map[string]string{}
	for _, source := range sources {
		if source.Container == nil {
			continue
		}
		key := source.Container.Engine + "/" + source.Container.Name
		if v.containerAs[key] != "" {
			v.containerAs[key] += ","
		}
		v.containerAs[key] += source.Name
	}
}

// extra is every source of databases only that no database row stands
// for, drawn as a row of its own on its tab. A source with a folder is
// a row of the folders tab already (FolderSources).
func (v chooseView) extra() []panel.Source {
	databaseRow := map[string]bool{}
	for _, database := range v.Candidates.MySQL {
		databaseRow[database.ChosenAs] = true
	}
	for _, database := range v.Candidates.PostgreSQL {
		databaseRow[database.ChosenAs] = true
	}
	var extra []panel.Source
	for _, source := range v.Sources {
		if source.Path == "" && source.Container == nil && !databaseRow[source.Name] {
			extra = append(extra, source)
		}
	}
	return extra
}

// ExtraMySQL and ExtraPostgreSQL split the extra sources by the tab
// they belong on.
func (v chooseView) ExtraMySQL() []panel.Source      { return v.extraOf(true) }
func (v chooseView) ExtraPostgreSQL() []panel.Source { return v.extraOf(false) }

func (v chooseView) extraOf(mysql bool) []panel.Source {
	var of []panel.Source
	for _, source := range v.Extra {
		if (len(source.MySQL) > 0) == mysql {
			of = append(of, source)
		}
	}
	return of
}

// chooseViewFor reads the candidates for the What to back up page and
// folds the submitted form, if any, into the view.
func (s *Server) chooseViewFor(r *http.Request, chooser panel.Chooser, sources int, formError string) chooseView {
	view := chooseView{Mode: "add", Submitted: map[string]string{}, Ticked: map[string]bool{}, FormError: formError, Refused: formError != ""}
	asked := r.URL.Query().Get("add") != ""
	view.Adding = asked || sources == 0 || formError != ""
	view.Tab = r.URL.Query().Get("tab")
	if formError != "" {
		view.Tab = r.PostFormValue("tab")
	}
	if !knownTab(view.Tab) {
		view.Tab = chooseTabs[0]
	}
	// What there is to choose from costs a look at the databases and the
	// containers: a size query on each MySQL and PostgreSQL client and a
	// docker ps. It is read when the form is asked for or shown again
	// after a refusal, and when a person opens an empty page -- not on
	// the live refresh every three seconds while a backup runs, nor on
	// the terminal's five-second read, which asks with add=1 when the
	// operator presses a key to choose.
	if view.Adding && r.Header.Get("X-Gniza-Live") == "" && (asked || formError != "" || !wantsData(r)) {
		view.load(r.Context(), chooser)
	}
	if formError != "" {
		for name, values := range r.PostForm {
			switch name {
			case "csrf", "tab":
				continue
			case "folder", "mysql", "postgresql", "container":
				for _, value := range values {
					view.Ticked[name+"/"+value] = true
				}
			default:
				if len(values) > 0 {
					view.Submitted[name] = values[0]
				}
			}
		}
	}
	return view
}

// chooseForSchedule draws the same tables as the schedule form, where
// what is ticked is what the schedule covers. A new schedule starts
// with every source ticked; one being edited with what it covers. When nothing has been chosen yet the databases and
// containers come up ticked, so the first schedule backs something up.
// The candidates are read for a person only, not for the terminal's
// five-second read of the page.
func (s *Server) chooseForSchedule(r *http.Request, chooser panel.Chooser, editing *nodestore.Policy) chooseView {
	view := chooseView{Mode: "schedule", Tab: chooseTabs[0], Submitted: map[string]string{}, Chosen: map[string]bool{}}
	if wantsData(r) || r.Header.Get("X-Gniza-Live") != "" {
		return view
	}
	view.load(r.Context(), chooser)
	view.Fresh = len(view.Sources) == 0
	for _, source := range view.Sources {
		view.Chosen[source.Name] = editing == nil || editing.AllAccounts()
	}
	if editing != nil {
		for _, name := range editing.Accounts {
			view.Chosen[name] = true
		}
	}
	view.Extra = view.extra()
	return view
}

func knownTab(name string) bool {
	for _, tab := range chooseTabs {
		if tab == name {
			return true
		}
	}
	return false
}

// addTicked records every fresh row the form ticked as a source of its
// own -- each folder, each database and each container -- and a folder
// typed in by hand. It says what was added, by name, and what was not,
// with why.
func (s *Server) addTicked(r *http.Request, chooser panel.Chooser) (added, refused []string) {
	try := func(shown string, add func() ([]string, error)) {
		names, err := add()
		if err != nil {
			refused = append(refused, fmt.Sprintf("%s: %v", shown, err))
			return
		}
		added = append(added, names...)
	}
	one := func(source panel.Source) func() ([]string, error) {
		return func() ([]string, error) {
			if err := chooser.AddSource(r.Context(), source); err != nil {
				return nil, err
			}
			return []string{s.nameOf(r, chooser, source)}, nil
		}
	}
	for _, path := range r.PostForm["folder"] {
		path := strings.TrimSpace(path)
		try(path, one(panel.Source{Path: path}))
	}
	if path, name := strings.TrimSpace(r.PostFormValue("folder_path")), strings.TrimSpace(r.PostFormValue("folder_name")); path != "" || name != "" {
		shown := path
		if shown == "" {
			shown = name
		}
		try(shown, one(panel.Source{Path: path, Name: name}))
	}
	for _, name := range r.PostForm["mysql"] {
		try("MySQL "+name, one(panel.Source{MySQL: []string{name}}))
	}
	for _, name := range r.PostForm["postgresql"] {
		try("PostgreSQL "+name, one(panel.Source{PostgreSQL: []string{name}}))
	}
	for _, ticked := range r.PostForm["container"] {
		engine, name, found := strings.Cut(ticked, "/")
		try(ticked, func() ([]string, error) {
			if !found {
				return nil, fmt.Errorf("does not name a container")
			}
			made, err := chooser.AddContainer(r.Context(), engine, name)
			if err != nil {
				return nil, err
			}
			names := make([]string, len(made))
			for i, source := range made {
				names[i] = source.Name
			}
			return names, nil
		})
	}
	return added, refused
}

// scheduleChoices reads what the schedule form ticked: the sources
// named outright, and the fresh rows, which are added first. The names
// come back in the order ticked, each once.
func (s *Server) scheduleChoices(r *http.Request, chooser panel.Chooser) (names, refused []string, err error) {
	sources, err := chooser.Sources(r.Context())
	if err != nil {
		return nil, nil, err
	}
	known := map[string]bool{}
	for _, source := range sources {
		known[source.Name] = true
	}
	seen := map[string]bool{}
	keep := func(name string) {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	// The TUI and a script name what is covered as account=, the way a
	// panel's schedule form does; the page's rows post source=.
	for _, ticked := range append(r.PostForm["source"], r.PostForm["account"]...) {
		for _, name := range strings.Split(ticked, ",") {
			name = strings.TrimSpace(name)
			switch {
			case name == "":
			case !known[name]:
				refused = append(refused, name+": nothing of that name is chosen")
			default:
				keep(name)
			}
		}
	}
	added, notAdded := s.addTicked(r, chooser)
	for _, name := range added {
		keep(name)
	}
	return names, append(refused, notAdded...), nil
}

// handleBrowseFolders answers the folder browser: the directories under
// the one asked for, as data, with the ones chosen already marked.
func (s *Server) handleBrowseFolders(w http.ResponseWriter, r *http.Request) {
	chooser, ok := s.engine.Chooser()
	browser, browses := chooser.(panel.Browser)
	if !ok || !browses {
		s.fail(w, r, http.StatusNotFound, fmt.Errorf("the folders on this server are not browsed here: %s lists the accounts", s.panelName()))
		return
	}
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		dir = "/"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	listing, err := browser.Browse(r.Context(), dir)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(listing)
}

// handleAddSource records what the operator ticked: what could be
// added is added; what could not is said, with the rest.
func (s *Server) handleAddSource(w http.ResponseWriter, r *http.Request) {
	chooser, ok := s.engine.Chooser()
	if !ok {
		s.redirect(w, r, "/accounts", "error", "Accounts on this server are "+s.panelName()+"'s to list, not chosen here.")
		return
	}
	added, refused := s.addTicked(r, chooser)
	switch {
	case len(added) == 0 && len(refused) == 0:
		s.refuseSource(w, r, chooser, fmt.Errorf("Tick something, or name a folder: nothing was chosen."))
	case len(added) == 0:
		s.refuseSource(w, r, chooser, fmt.Errorf("Nothing was added. %s.", strings.Join(refused, "; ")))
	case len(refused) > 0:
		s.redirect(w, r, "/accounts", "warn", fmt.Sprintf("Added %s. Not added: %s.", strings.Join(added, ", "), strings.Join(refused, "; ")))
	default:
		noun := "source"
		if len(added) != 1 {
			noun = "sources"
		}
		s.redirect(w, r, "/accounts", "ok", fmt.Sprintf("Added %d %s: %s. The next scheduled run backs them up; Back up now does it sooner.",
			len(added), noun, strings.Join(added, ", ")))
	}
}

// nameOf says what a source just added is called, since the provider
// names the ones the operator did not.
func (s *Server) nameOf(r *http.Request, chooser panel.Chooser, source panel.Source) string {
	if source.Name != "" {
		return source.Name
	}
	sources, err := chooser.Sources(r.Context())
	if err != nil {
		return source.Path
	}
	for _, chosen := range sources {
		switch {
		case source.Path != "" && chosen.Path == source.Path && chosen.Container == nil:
			return chosen.Name
		case len(source.MySQL) == 1 && len(chosen.MySQL) == 1 && chosen.MySQL[0] == source.MySQL[0] && chosen.Path == "":
			return chosen.Name
		case len(source.PostgreSQL) == 1 && len(chosen.PostgreSQL) == 1 && chosen.PostgreSQL[0] == source.PostgreSQL[0] && chosen.Path == "":
			return chosen.Name
		}
	}
	return source.Path
}

// refuseSource shows the form again with the reason and what was ticked.
func (s *Server) refuseSource(w http.ResponseWriter, r *http.Request, chooser panel.Chooser, cause error) {
	accounts, warnings, err := s.accountViews(r)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	view := accountsView{Accounts: accounts, Warnings: warnings, FormError: cause.Error()}
	choose := s.chooseViewFor(r, chooser, len(accounts), cause.Error())
	view.Choose = &choose
	s.render(w, r, "accounts.html", "What to back up", "accounts", view)
}

// handleRemoveSource forgets a choice. Its backups stay where they are.
func (s *Server) handleRemoveSource(w http.ResponseWriter, r *http.Request) {
	chooser, ok := s.engine.Chooser()
	if !ok {
		s.redirect(w, r, "/accounts", "error", "Accounts on this server are "+s.panelName()+"'s to list, not chosen here.")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("account"))
	if err := chooser.RemoveSource(r.Context(), name); err != nil {
		s.redirect(w, r, "/accounts", "error", err.Error())
		return
	}
	s.redirect(w, r, "/accounts", "ok", fmt.Sprintf("%s is no longer backed up. The backups already taken of it stay at the destinations.", name))
}
