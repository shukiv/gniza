package webui

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTheTypefacesComeFromTheServerAndNotFromAFontHost(t *testing.T) {
	faces := string(fontFaces(fontBaseFor(httptest.NewRequest("GET", "/", nil))))
	if strings.Contains(faces, "//") {
		t.Fatalf("a typeface is fetched from somewhere else: %s", faces)
	}
	for name := range fontFiles {
		if !strings.Contains(faces, "?p=font&name="+name) {
			t.Errorf("%s is not asked for where WHM serves it", name)
		}
	}
}

// Everything a DirectAdmin plugin prints is wrapped in DirectAdmin's own
// skin, so a woff2 fetched through the plugin script arrives as HTML with
// a font inside it and the browser drops it -- which is what the console
// on 1.709 said. DirectAdmin serves a plugin's images directory as files,
// and that is where every other plugin on the server keeps its fonts.
func TestDirectAdminFetchesTheTypefacesAsFilesRatherThanThroughThePlugin(t *testing.T) {
	request := httptest.NewRequest("GET", "/", nil)
	request = request.WithContext(context.WithValue(request.Context(),
		daPrincipalKey{}, daPrincipal{Username: "admin", UserType: "admin"}))

	faces := string(fontFaces(fontBaseFor(request)))
	if strings.Contains(faces, "?p=font") {
		t.Fatalf("the page still asks the plugin for a font: %s", faces)
	}
	for name := range fontFiles {
		if !strings.Contains(faces, "/CMD_PLUGINS_ADMIN/gniza/images/fonts/"+name) {
			t.Errorf("%s is not asked for where DirectAdmin serves files", name)
		}
	}
}

// Evolution opens a plugin page through its own wrapper, so the address
// in the browser is DirectAdmin's, not the plugin's, and a page that
// fetched itself by that address to keep current got Evolution's shell
// back with none of its own regions in it. The Version card said
// "Downloading v0.3.5" for as long as it was open while v0.3.5 was
// installed and running underneath it. So a page served through
// DirectAdmin says where it can be fetched from: the plugin's own
// address, at the level the session has, with the route it is on.
func TestAPageServedThroughDirectAdminSaysWhereToFetchItself(t *testing.T) {
	for userType, base := range map[string]string{
		"admin":    "/CMD_PLUGINS_ADMIN/gniza/index.html",
		"reseller": "/CMD_PLUGINS_RESELLER/gniza/index.html",
		"user":     "/CMD_PLUGINS/gniza/index.html",
	} {
		request := httptest.NewRequest("GET", "/?p=settings&tab=version", nil)
		request = request.WithContext(context.WithValue(request.Context(),
			daPrincipalKey{}, daPrincipal{Username: "someone", UserType: userType}))
		if got, want := liveURLFor(request), base+"?p=settings&tab=version"; got != want {
			t.Errorf("%s: the page says to fetch %q, want %q", userType, got, want)
		}
	}
	if got := liveURLFor(httptest.NewRequest("GET", "/?p=settings", nil)); got != "" {
		t.Errorf("a page not served through DirectAdmin names %q; its own address is the right one there", got)
	}
}
