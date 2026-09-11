// Package answer is the shape a page takes when it is asked for as data
// rather than as HTML.
//
// The operator's interface is served over a unix socket as pages, and the
// terminal interface reads the same pages from the same handlers. What it
// needs from one is here: the page's own data, the token every form needs,
// the flash a redirect carried, and the strip every page shows. The
// stylesheet, the fonts and the navigation are not: they are how a browser
// draws it, not what it says.
package answer

import "encoding/json"

// Header is what a client sends to be answered with a Page.
const Header = "application/json"

// Page is one page as data.
type Page struct {
	Title string `json:"title"`
	// Nav names the page in the interface's own terms: dashboard,
	// destinations, schedule, accounts, restore, logs, settings.
	Nav string `json:"nav"`
	// CSRF is the token every form must carry. It changes when the
	// service restarts, and a form posted with a stale one is refused
	// with 403.
	CSRF  string `json:"csrf"`
	Flash *Flash `json:"flash,omitempty"`
	// Data is the page's own view, in whatever shape the page has. A
	// client decodes the fields it reads and ignores the rest.
	Data    json.RawMessage `json:"data"`
	Running []Running       `json:"running,omitempty"`
	Update  *Update         `json:"update,omitempty"`
	// Version is the build answering, and Panel what it calls the panel
	// it runs on: "cPanel", "DirectAdmin" or "Plain server".
	Version string `json:"version"`
	Panel   string `json:"panel"`
}

// Flash is a one-shot message: ok, warn or error, and the words.
type Flash struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Running is one backup or restore in flight.
type Running struct {
	Account string  `json:"account"`
	Doing   string  `json:"doing"`
	Waiting bool    `json:"waiting"`
	Detail  string  `json:"detail,omitempty"`
	Percent float64 `json:"percent"`
	Known   bool    `json:"known"`
}

// Update is a published release newer than the one answering.
type Update struct {
	Version string `json:"version"`
	Current string `json:"current"`
	URL     string `json:"url"`
}
