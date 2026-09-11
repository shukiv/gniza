package webui_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/agent"
	"github.com/shukiv/gniza/internal/nodestore"
)

func TestFooterShowsRunningVersionNotAvailableUpdate(t *testing.T) {
	was := agent.Version
	agent.Version = "v1.2.3-4-g1234567-dirty"
	t.Cleanup(func() { agent.Version = was })
	client, _, engine := newUI(t)
	if err := engine.Store().SaveUpdateState(nodestore.UpdateState{Version: "v9.9.9"}); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/", "/settings?tab=version", "/restore", "/report"} {
		_, page := get(t, client, route)
		footer := regexp.MustCompile(`(?s)<p class="[^"]*" data-rail-version>(.*?)</p>`).FindStringSubmatch(page)
		if len(footer) != 2 || !strings.Contains(footer[1], "Version") || !strings.Contains(footer[1], agent.Version) || strings.Contains(footer[1], "v9.9.9") {
			t.Fatalf("%s: footer does not show the running build", route)
		}
		if strings.Index(page, `data-rail-version>`) > strings.Index(page, "</aside>") {
			t.Fatalf("%s: version is outside the sidebar", route)
		}
	}
}

func TestReportDialogStaysInsidePluginStyleScope(t *testing.T) {
	client, _, _ := newUI(t)
	_, page := get(t, client, "/settings?tab=version")
	start := strings.Index(page, `<div class="gniza `)
	dialog := strings.Index(page, `<dialog id="report-problem"`)
	if start < 0 || dialog <= start {
		t.Fatal("missing plugin wrapper or report dialog")
	}
	// The report used to follow the wrapper's closing tag, so every
	// .gniza-scoped rule missed it despite being present in the stylesheet.
	prefix := page[start:dialog]
	depth := len(regexp.MustCompile(`<div(?:\s|>)`).FindAllStringIndex(prefix, -1)) - strings.Count(prefix, "</div>")
	if depth != 1 {
		t.Fatalf("report dialog should be directly inside the plugin wrapper; div depth=%d", depth)
	}
	end := strings.Index(page[dialog:], "</dialog>")
	if end < 0 {
		t.Fatal("report dialog is not closed")
	}
	markup := page[dialog : dialog+end]
	for _, want := range []string{`class="cpr:modal" data-report-dialog`, `aria-labelledby="report-problem-title"`, `aria-describedby="report-problem-intro"`, `name="csrf"`, `action="?p=report/send"`, `maxlength="20000"`, "Show me the report", "Open full page"} {
		if !strings.Contains(markup, want) {
			t.Errorf("report dialog missing %q", want)
		}
	}
	if strings.Contains(markup, `name="send"`) || strings.Contains(markup, `name="download"`) {
		t.Error("the modal must show the report first, not hand it over unseen")
	}
}
