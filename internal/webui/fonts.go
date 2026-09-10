package webui

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
)

// The typefaces travel with the plugin; see fonts/README.md. Where they
// are fetched from depends on the panel, so the @font-face rules are
// written per request rather than sitting in app.css.
//
// On WHM the address is query-only, so it resolves against gniza.cgi
// itself -- session token and all -- whatever path WHM is serving the
// plugin from.
//
// DirectAdmin cannot do that. Everything a plugin prints is wrapped in
// DirectAdmin's own skin before it reaches the browser, so a font asked
// for through the plugin script arrives as HTML with a woff2 inside it
// and the browser refuses it. DirectAdmin does serve a plugin's images
// directory as files, which is where every other plugin on the server
// keeps its own web fonts, so that is where these go.
const daFontBase = "/CMD_PLUGINS_ADMIN/gniza/images/fonts/"

// fontFaces writes the five @font-face rules against a base address.
//
// base is joined to the file name, so it is either a directory ending in
// a slash or the query prefix WHM uses.
func fontFaces(base string) template.CSS {
	faces := make([]string, 0, len(fontFiles))
	for name := range fontFiles {
		family, weight := "Fira Sans", strings.TrimSuffix(
			strings.TrimPrefix(name, "fira-sans-"), ".woff2")
		if name == "fira-code.woff2" {
			family, weight = "Fira Code", "400 500"
		}
		faces = append(faces, fmt.Sprintf(
			"@font-face{font-family:%q;font-style:normal;font-weight:%s;"+
				"font-display:swap;src:url(%q) format(\"woff2\")}",
			family, weight, base+name))
	}
	// A fixed order: the same page drawn twice must be the same bytes,
	// and the live refresh compares them.
	sort.Strings(faces)
	return template.CSS(strings.Join(faces, "\n"))
}

// fontBaseFor is where this request should fetch the typefaces from.
// liveURLFor is where a page served through DirectAdmin can fetch itself
// from to keep current. Evolution opens a plugin through its own wrapper,
// so the address in the browser is DirectAdmin's rather than the
// plugin's, and fetching that address gets Evolution's shell back with
// none of the page's regions in it -- the Version card said
// "Downloading" for as long as it was open while the release it named
// was installed and running underneath. The plugin's own address, at the
// level the session has, answers with the page. Empty everywhere else:
// there the browser's own address is the right one.
func liveURLFor(r *http.Request) string {
	who := daPrincipalOf(r)
	if who.Username == "" {
		return ""
	}
	base := "/CMD_PLUGINS/gniza/index.html"
	switch who.UserType {
	case "admin":
		base = "/CMD_PLUGINS_ADMIN/gniza/index.html"
	case "reseller":
		base = "/CMD_PLUGINS_RESELLER/gniza/index.html"
	}
	if r.URL.RawQuery == "" {
		return base
	}
	return base + "?" + r.URL.RawQuery
}

func fontBaseFor(r *http.Request) string {
	if daPrincipalOf(r).Username != "" {
		return daFontBase
	}
	return "?p=font&name="
}
