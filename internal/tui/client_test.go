package tui

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shukiv/gniza/internal/answer"
)

// fakeService is a service that answers pages as data the way the real
// one does, with a token it can rotate under the client.
type fakeService struct {
	socket string
	token  atomic.Value
	posted chan url.Values
}

func serve(t *testing.T) *fakeService {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "ui.sock")
	service := &fakeService{socket: socket, posted: make(chan url.Values, 8)}
	service.token.Store("first")

	mux := http.NewServeMux()
	page := func(w http.ResponseWriter, status int, nav string, data any) {
		body, _ := json.Marshal(data)
		out := answer.Page{Title: nav, Nav: nav, CSRF: service.token.Load().(string),
			Data: body, Version: "test", Panel: "Plain server"}
		w.Header().Set("Content-Type", answer.Header+"; charset=utf-8")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(out)
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), answer.Header) {
			http.Error(w, "<html>a page</html>", http.StatusOK)
			return
		}
		page(w, http.StatusOK, "dashboard", map[string]string{"Hostname": "lamp.example"})
	})
	mux.HandleFunc("GET /broken", func(w http.ResponseWriter, r *http.Request) {
		page(w, http.StatusBadRequest, "Problem", "that address is not part of gniza")
	})
	mux.HandleFunc("POST /accounts/backup", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostFormValue("csrf") != service.token.Load().(string) {
			page(w, http.StatusForbidden, "Problem", "this page expired when the service restarted; reload and try again")
			return
		}
		service.posted <- r.PostForm
		query := url.Values{"p": {"accounts"}, "kind": {"ok"}, "msg": {"Backup of " + r.PostFormValue("account") + " queued."}}
		w.Header().Set("Location", "?"+query.Encode())
		w.WriteHeader(http.StatusSeeOther)
	})
	mux.HandleFunc("POST /destinations/add", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		page(w, http.StatusOK, "destinations", map[string]string{"FormError": "Give the destination a name."})
	})

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return service
}

// TestAPageIsReadAsData: the client asks for data and gets the page's
// view, with the token every form needs.
func TestAPageIsReadAsData(t *testing.T) {
	service := serve(t)
	client, err := Dial(service.socket)
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.Page("/")
	if err != nil {
		t.Fatal(err)
	}
	var view struct{ Hostname string }
	if err := json.Unmarshal(page.Data, &view); err != nil || view.Hostname != "lamp.example" {
		t.Errorf("the page's data = %s", page.Data)
	}
	if page.CSRF != "first" || page.Panel != "Plain server" {
		t.Errorf("the page = %+v", page)
	}
}

// TestARefusedPageIsItsReason: an error page's data is the reason, and
// the client says it rather than "400".
func TestARefusedPageIsItsReason(t *testing.T) {
	service := serve(t)
	client, err := Dial(service.socket)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Page("/broken")
	if err == nil || !strings.Contains(err.Error(), "not part of gniza") {
		t.Errorf("a refused page = %v", err)
	}
}

// TestARedirectIsItsMessage: a form that redirects comes back as the kind
// and message the redirect carried, without following it.
func TestARedirectIsItsMessage(t *testing.T) {
	service := serve(t)
	client, err := Dial(service.socket)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := client.Post("/accounts/backup", url.Values{"account": {"shop"}})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Kind != "ok" || outcome.Message != "Backup of shop queued." || outcome.Page != nil {
		t.Errorf("the outcome = %+v", outcome)
	}
	if sent := <-service.posted; sent.Get("csrf") != "first" || sent.Get("account") != "shop" {
		t.Errorf("the form sent = %v", sent)
	}
}

// TestAStaleTokenIsRefreshedOnce: the service restarted between the page
// and the post, so the token is stale. The client fetches a fresh one and
// posts again, and the operator never sees the 403.
func TestAStaleTokenIsRefreshedOnce(t *testing.T) {
	service := serve(t)
	client, err := Dial(service.socket)
	if err != nil {
		t.Fatal(err)
	}
	service.token.Store("second")
	outcome, err := client.Post("/accounts/backup", url.Values{"account": {"shop"}})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Kind != "ok" {
		t.Errorf("the outcome = %+v", outcome)
	}
	if sent := <-service.posted; sent.Get("csrf") != "second" {
		t.Errorf("the retried form carried %q", sent.Get("csrf"))
	}
}

// TestARefusedFormIsThePageItCameBackAs: a handler that re-renders the
// form with the reason answers with a page, and the outcome is that page.
func TestARefusedFormIsThePageItCameBackAs(t *testing.T) {
	service := serve(t)
	client, err := Dial(service.socket)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := client.Post("/destinations/add", url.Values{"type": {"local"}})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Page == nil {
		t.Fatalf("the outcome = %+v", outcome)
	}
	var view struct{ FormError string }
	if err := json.Unmarshal(outcome.Page.Data, &view); err != nil || view.FormError == "" {
		t.Errorf("the refused form's data = %s", outcome.Page.Data)
	}
}

// TestNoServiceIsSaidInWords: a socket nobody listens on is the usual
// first mistake, and "connection refused" does not say what to do.
func TestNoServiceIsSaidInWords(t *testing.T) {
	_, err := Dial(filepath.Join(t.TempDir(), "ui.sock"))
	if err == nil || !strings.Contains(err.Error(), "Is the gniza service running") {
		t.Errorf("dialing nothing = %v", err)
	}
}
