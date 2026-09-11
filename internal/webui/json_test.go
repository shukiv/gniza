package webui_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/answer"
)

func getJSON(t *testing.T, client *http.Client, path string) (int, answer.Page) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "http://ui"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", answer.Header)
	resp, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, answer.Header) {
		t.Fatalf("GET %s as data answered %q: %s", path, got, body)
	}
	var page answer.Page
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("GET %s as data is not a page: %v: %s", path, err, body)
	}
	return resp.StatusCode, page
}

// TestEveryPageAnswersAsData: the terminal interface reads the same pages
// the browser does, from the same handlers, and gets the page's own view
// rather than the HTML that draws it.
func TestEveryPageAnswersAsData(t *testing.T) {
	client, _, _ := newUI(t)

	for path, nav := range map[string]string{
		"/": "dashboard", "/destinations": "destinations", "/schedule": "schedule",
		"/accounts": "accounts", "/restore": "restore", "/logs": "logs", "/settings": "settings",
	} {
		status, page := getJSON(t, client, path)
		if status != http.StatusOK {
			t.Errorf("GET %s as data = %d", path, status)
			continue
		}
		if page.Nav != nav {
			t.Errorf("GET %s as data names itself %q, not %q", path, page.Nav, nav)
		}
		if page.CSRF == "" {
			t.Errorf("GET %s as data carries no form token", path)
		}
		if page.Panel == "" || page.Version == "" {
			t.Errorf("GET %s as data says no panel or version: %+v", path, page)
		}
		if len(page.Data) == 0 || string(page.Data) == "null" {
			t.Errorf("GET %s as data has no view", path)
		}
	}

	var overview struct {
		Hostname string
	}
	_, page := getJSON(t, client, "/")
	if err := json.Unmarshal(page.Data, &overview); err != nil || overview.Hostname == "" {
		t.Errorf("the overview's data has no hostname: %v %s", err, page.Data)
	}
}

// TestTheBrowserStillGetsHTML: a request that does not ask for data gets
// the page it always got.
func TestTheBrowserStillGetsHTML(t *testing.T) {
	client, _, _ := newUI(t)
	resp, err := client.Get("http://ui/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("a browser's request answered %q", got)
	}
}

// TestARefusalAnswersAsDataWithItsStatus: a page that failed says so in
// the status, and the reason is the page's data. The HTML error page
// used to be written after the status, which dropped its content type.
func TestARefusalAnswersAsDataWithItsStatus(t *testing.T) {
	client, _, _ := newUI(t)

	status, page := getJSON(t, client, "/?p=../etc")
	if status != http.StatusBadRequest {
		t.Errorf("a bad route as data = %d", status)
	}
	var reason string
	if err := json.Unmarshal(page.Data, &reason); err != nil || !strings.Contains(reason, "not part of gniza") {
		t.Errorf("the refusal's data is not the reason: %v %s", err, page.Data)
	}

	// The same for the browser: the error page carries its content type.
	resp, err := client.Get("http://ui/?p=../etc")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a bad route = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("the error page answered %q", got)
	}
}

// TestARefusedFormComesBackAsData: a form the handler re-renders with the
// reason comes back as data with the reason in it, and a form that
// redirects carries its message in the Location the client reads.
func TestARefusedFormComesBackAsData(t *testing.T) {
	client, _, _ := newUI(t)
	_, page := getJSON(t, client, "/destinations")

	form := strings.NewReader("csrf=" + page.CSRF + "&type=local&root=/tmp")
	request, err := http.NewRequest(http.MethodPost, "http://ui/destinations/add", form)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", answer.Header)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var refused answer.Page
	if err := json.Unmarshal(body, &refused); err != nil {
		t.Fatalf("a refused form as data is not a page: %v: %s", err, body)
	}
	var view struct{ FormError string }
	if err := json.Unmarshal(refused.Data, &view); err != nil || !strings.Contains(view.FormError, "name") {
		t.Errorf("the refused form does not say why: %v %s", err, refused.Data)
	}
}
