package webui

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/shukiv/gniza/internal/panel"
)

// The choosing of what to back up, on a server whose accounts are the
// operator's choice rather than a panel's list (ADR 0025). The accounts
// page carries it when the provider is a panel.Chooser, and the same
// handlers serve the socket, the browser door and the terminal.
//
// The form is one tab per kind -- folders, MySQL, PostgreSQL, containers
// -- each a table of what there is with a box per row, and each ticked
// row becomes a source of its own.

// chooseTabs are the kinds, in the order the tabs show them.
var chooseTabs = []string{"folders", "mysql", "postgresql", "containers"}

// chooseView is what the accounts page shows about choosing.
type chooseView struct {
	Candidates panel.Candidates
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

// chooseViewFor reads the candidates and folds the submitted form, if
// any, into the view.
func (s *Server) chooseViewFor(r *http.Request, chooser panel.Chooser, sources int, formError string) chooseView {
	view := chooseView{Submitted: map[string]string{}, Ticked: map[string]bool{}, FormError: formError, Refused: formError != ""}
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
		candidates, err := chooser.Candidates(r.Context())
		if err != nil {
			if formError == "" {
				view.FormError = "Could not see what there is to choose from: " + err.Error()
			}
		}
		view.Candidates = candidates
		view.Offers = len(candidates.Folders)+len(candidates.MySQL)+len(candidates.PostgreSQL)+len(candidates.Containers) > 0
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

func knownTab(name string) bool {
	for _, tab := range chooseTabs {
		if tab == name {
			return true
		}
	}
	return false
}

// handleAddSource records what the operator ticked on one tab: each
// folder, each database and each container as a source of its own, and
// a folder typed in by hand. What could be added is added; what could
// not is said, with the rest.
func (s *Server) handleAddSource(w http.ResponseWriter, r *http.Request) {
	chooser, ok := s.engine.Chooser()
	if !ok {
		s.redirect(w, r, "/accounts", "error", "Accounts on this server are "+s.panelName()+"'s to list, not chosen here.")
		return
	}
	var added, refused []string
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
	if path, name := strings.TrimSpace(r.PostFormValue("path")), strings.TrimSpace(r.PostFormValue("name")); path != "" || name != "" {
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
