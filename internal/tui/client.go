// Package tui is the operator's interface for a server that has no
// control panel: the same pages the plugins draw, read over the same unix
// socket and drawn in a terminal instead.
//
// It reads pages as data (internal/answer) and posts the same forms the
// plugins post. Nothing here opens the state database or the master key:
// bolt is a single-writer store, and the service is the writer.
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/shukiv/gniza/internal/answer"
)

// Outcome is what a posted form came to: a redirect's message, or a page
// the handler drew instead of redirecting -- a refused form with the
// reason in it, a recovery key to read, a host key to agree to.
type Outcome struct {
	Kind    string
	Message string
	// Page is set when the handler answered with a page rather than a
	// redirect. Kind and Message are empty then.
	Page *answer.Page
}

// Client reads and posts to the service over its admin socket.
type Client struct {
	socket string
	http   *http.Client
	csrf   string
}

// Dial connects to the service's admin socket.
func Dial(socket string) (*Client, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	client := &Client{socket: socket, http: &http.Client{
		Transport: transport,
		Timeout:   5 * time.Minute,
		// A redirect carries the message; following it would lose it.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
	if _, err := client.Page("/"); err != nil {
		return nil, err
	}
	return client, nil
}

// Page reads one page as data.
func (c *Client) Page(path string) (answer.Page, error) {
	request, err := http.NewRequest(http.MethodGet, "http://gniza"+path, nil)
	if err != nil {
		return answer.Page{}, err
	}
	request.Header.Set("Accept", answer.Header)
	resp, err := c.http.Do(request)
	if err != nil {
		return answer.Page{}, c.unreachable(err)
	}
	defer resp.Body.Close()
	page, err := decodePage(resp)
	if err != nil {
		return answer.Page{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return answer.Page{}, fmt.Errorf("%s: %s", path, reasonOf(page))
	}
	c.csrf = page.CSRF
	return page, nil
}

// Post sends a form. The token the last page carried goes with it; a
// form refused for a stale token, which is what a restarted service
// answers, is sent once more with a fresh one.
func (c *Client) Post(path string, form url.Values) (Outcome, error) {
	if c.csrf == "" {
		if _, err := c.Page("/"); err != nil {
			return Outcome{}, err
		}
	}
	outcome, status, err := c.post(path, form)
	if err == nil && status == http.StatusForbidden {
		if _, err := c.Page("/"); err != nil {
			return Outcome{}, err
		}
		outcome, status, err = c.post(path, form)
	}
	if err != nil {
		return Outcome{}, err
	}
	if status >= http.StatusBadRequest {
		reason := "the service refused it"
		if outcome.Page != nil {
			reason = reasonOf(*outcome.Page)
		}
		return Outcome{}, fmt.Errorf("%s: %s", path, reason)
	}
	return outcome, nil
}

func (c *Client) post(path string, form url.Values) (Outcome, int, error) {
	sent := url.Values{}
	for name, values := range form {
		sent[name] = values
	}
	sent.Set("csrf", c.csrf)
	request, err := http.NewRequest(http.MethodPost, "http://gniza"+path, strings.NewReader(sent.Encode()))
	if err != nil {
		return Outcome{}, 0, err
	}
	request.Header.Set("Accept", answer.Header)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(request)
	if err != nil {
		return Outcome{}, 0, c.unreachable(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusSeeOther {
		location, err := url.Parse(resp.Header.Get("Location"))
		if err != nil {
			return Outcome{}, resp.StatusCode, fmt.Errorf("%s: unreadable redirect: %w", path, err)
		}
		query := location.Query()
		return Outcome{Kind: query.Get("kind"), Message: query.Get("msg")}, resp.StatusCode, nil
	}
	page, err := decodePage(resp)
	if err != nil {
		return Outcome{}, resp.StatusCode, err
	}
	if page.CSRF != "" {
		c.csrf = page.CSRF
	}
	return Outcome{Page: &page}, resp.StatusCode, nil
}

func decodePage(resp *http.Response) (answer.Page, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return answer.Page{}, err
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), answer.Header) {
		text := strings.TrimSpace(string(body))
		if len(text) > 200 {
			text = text[:200] + "…"
		}
		return answer.Page{}, fmt.Errorf("the service answered %d with something that is not a page: %s",
			resp.StatusCode, text)
	}
	var page answer.Page
	if err := json.Unmarshal(body, &page); err != nil {
		return answer.Page{}, fmt.Errorf("unreadable page: %w", err)
	}
	return page, nil
}

// reasonOf is what an error page says: its data is the reason.
func reasonOf(page answer.Page) string {
	var reason string
	if err := json.Unmarshal(page.Data, &reason); err == nil && reason != "" {
		return reason
	}
	return page.Title
}

// unreachable says why the socket could not be used, in terms of what to
// do about it. The socket is root's; the usual reason is not being root.
func (c *Client) unreachable(err error) error {
	var pathErr *os.PathError
	switch {
	case errors.Is(err, os.ErrPermission):
		return fmt.Errorf("cannot open %s: permission denied. Run this as root", c.socket)
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("nothing is listening at %s. Is the gniza service running? (systemctl status gniza)", c.socket)
	case errors.As(err, &pathErr):
		return fmt.Errorf("cannot open %s: %v", c.socket, pathErr.Err)
	}
	return fmt.Errorf("cannot reach the gniza service at %s: %w", c.socket, err)
}
