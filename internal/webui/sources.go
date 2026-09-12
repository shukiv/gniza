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

// chooseView is what the accounts page shows about choosing.
type chooseView struct {
	Candidates panel.Candidates
	// Adding opens the form: asked for, or nothing chosen yet, or a
	// choice was refused and the form is shown again with the reason.
	Adding    bool
	FormError string
	// Submitted is what the operator typed when the form was refused.
	Submitted map[string]string
	// Ticked is which databases and containers were ticked then, keyed
	// "mysql/<name>", "postgresql/<name>" and "container/<engine>/<name>".
	Ticked map[string]bool
	// Offers says whether any candidate at all was found, so an empty
	// form can say why it is empty.
	Offers bool
}

func (v chooseView) Field(name string) string { return v.Submitted[name] }
func (v chooseView) Was(key string) bool      { return v.Ticked[key] }

// chooseViewFor reads the candidates and folds the submitted form, if
// any, into the view.
func (s *Server) chooseViewFor(r *http.Request, chooser panel.Chooser, sources int, formError string) chooseView {
	view := chooseView{Submitted: map[string]string{}, Ticked: map[string]bool{}, FormError: formError}
	asked := r.URL.Query().Get("add") != ""
	view.Adding = asked || sources == 0 || formError != ""
	// What there is to choose from costs a look at the databases and the
	// containers: a size query on each MySQL and PostgreSQL client and a
	// docker ps. It is read when the form is asked for or shown again
	// after a refusal, and when a person opens an empty page -- not on
	// the live refresh every three seconds while a backup runs, nor on
	// the terminal's five-second read, which asks with add=1 when the
	// operator presses a or c.
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
			case "csrf":
				continue
			case "mysql", "postgresql", "container":
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

// handleAddSource records what the operator ticked: a folder with its
// databases as one source, and each container ticked as the sources its
// mounts make.
func (s *Server) handleAddSource(w http.ResponseWriter, r *http.Request) {
	chooser, ok := s.engine.Chooser()
	if !ok {
		s.redirect(w, r, "/accounts", "error", "Accounts on this server are "+s.panelName()+"'s to list, not chosen here.")
		return
	}
	source := panel.Source{
		Name:       strings.TrimSpace(r.PostFormValue("name")),
		Path:       strings.TrimSpace(r.PostFormValue("path")),
		MySQL:      r.PostForm["mysql"],
		PostgreSQL: r.PostForm["postgresql"],
	}
	containers := r.PostForm["container"]
	var added []string
	if source.Path != "" || len(source.MySQL)+len(source.PostgreSQL) > 0 || source.Name != "" {
		if err := chooser.AddSource(r.Context(), source); err != nil {
			s.refuseSource(w, r, chooser, err)
			return
		}
		if source.Name == "" {
			// The provider named it; say which name it got.
			if sources, err := chooser.Sources(r.Context()); err == nil {
				for _, chosen := range sources {
					if chosen.Path == source.Path && source.Path != "" {
						source.Name = chosen.Name
					}
				}
			}
		}
		added = append(added, source.Name)
	}
	for _, ticked := range containers {
		engine, name, found := strings.Cut(ticked, "/")
		if !found {
			s.refuseSource(w, r, chooser, fmt.Errorf("%q does not name a container", ticked))
			return
		}
		made, err := chooser.AddContainer(r.Context(), engine, name)
		if err != nil {
			s.refuseSource(w, r, chooser, err)
			return
		}
		for _, source := range made {
			added = append(added, source.Name)
		}
	}
	if len(added) == 0 {
		s.refuseSource(w, r, chooser, fmt.Errorf("Tick something, or name a folder: nothing was chosen."))
		return
	}
	noun := "source"
	if len(added) != 1 {
		noun = "sources"
	}
	s.redirect(w, r, "/accounts", "ok", fmt.Sprintf("Added %d %s: %s. The next scheduled run backs them up; Back up now does it sooner.",
		len(added), noun, strings.Join(added, ", ")))
}

// refuseSource shows the form again with the reason and what was typed.
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
