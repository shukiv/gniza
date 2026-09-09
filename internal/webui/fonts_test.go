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
