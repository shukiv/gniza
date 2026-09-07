package webui_test

import (
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/bugreport"
	"github.com/shukiv/gniza/internal/job"
	"github.com/shukiv/gniza/internal/nodestore"
)

func reportHidden(t *testing.T, page, name string) string {
	t.Helper()
	match := regexp.MustCompile(`name="` + name + `" value="([^"]*)"`).FindStringSubmatch(page)
	if len(match) != 2 {
		t.Fatalf("report has no %s field", name)
	}
	return html.UnescapeString(match[1])
}

func postReport(t *testing.T, client *http.Client, fields map[string][]string) string {
	t.Helper()
	resp, err := client.PostForm("http://ui/report/send", fields)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST report: %d %v", resp.StatusCode, err)
	}
	return string(body)
}

// TestTheReportIsPreparedHereAndFiledByHand is the whole of reporting a bug
// now: the server builds the report, shows it, and hands it over. It does
// not send it anywhere, and it needs no credential installed to be useful.
// The operator files it as an issue on the tracker, which is a page a
// person fills in -- so the page has to name that tracker.
func TestTheReportIsPreparedHereAndFiledByHand(t *testing.T) {
	client, _, engine := newUI(t)

	// No key is installed anywhere, and none is asked for. A server that
	// has never been given a credential must reach the same page as one
	// that has, because there is no longer any such credential.
	settings, err := engine.Store().Settings()
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(settings.ConfigDir); err == nil {
		for _, entry := range entries {
			if strings.Contains(entry.Name(), "intake") {
				t.Fatalf("the config directory still holds %s", entry.Name())
			}
		}
	}

	_, form := get(t, client, "/report")
	if regexp.MustCompile(`<button\b[^>]*\bname="send"`).MatchString(form) {
		t.Fatal("the form still offers to send the report from this server")
	}
	if !strings.Contains(form, bugreport.PublicReportURL) {
		t.Errorf("the form does not name %q", bugreport.PublicReportURL)
	}
	for _, gone := range []string{"intake key", "Send to intake", "Sending is not configured"} {
		if strings.Contains(form, gone) {
			t.Errorf("the form still talks about %q", gone)
		}
	}

	fields := map[string][]string{"csrf": {csrfToken(t, form)},
		"subject": {"Restore failed"}, "body": {"A restore did not finish. password=remove-me"}}
	preview := postReport(t, client, fields)
	if !strings.Contains(preview, "Download it") {
		t.Fatal("the preview does not offer the download")
	}
	fields["prepared"] = []string{reportHidden(t, preview, "prepared")}
	fields["signature"] = []string{reportHidden(t, preview, "signature")}
	if strings.Contains(fields["prepared"][0], "remove-me") {
		t.Fatal("the signed diagnostics contain an unredacted password")
	}

	// What is downloaded is what was reviewed, not what the server has
	// come to know since. That guarantee is why the preview is signed,
	// and it is the only guarantee left once nothing is transmitted.
	if _, err := engine.Store().PutJob(nodestore.Job{Account: "new-customer",
		Status: job.StatusFailed, StagingErr: "after-preview-private-detail"}); err != nil {
		t.Fatal(err)
	}
	fields["download"] = []string{"1"}
	download := postReport(t, client, fields)
	if !strings.HasPrefix(download, "# Restore failed") || strings.Contains(download, "after-preview-private-detail") {
		t.Fatal("the download did not preserve the reviewed diagnostic snapshot")
	}

	// A send that a stale page still asks for is answered with the
	// preview, not an error and not a request off this server.
	delete(fields, "download")
	fields["send"] = []string{"1"}
	stale := postReport(t, client, fields)
	if !strings.Contains(stale, "Download it") || strings.Contains(stale, "Sent to") {
		t.Fatal("a stale send was not answered with the report itself")
	}
}

// TestFilingCarriesWhatWasTyped. Two fields typed here and typed again on
// the tracker is how a bug report stops being filed at all, so filing
// hands the tracker's form what is in the fields.
//
// It is a submit and not a link because a link's target is decided when
// the page is drawn, which is before anything has been typed into it. The
// button posts the form and is answered with a redirect to the tracker.
func TestFilingCarriesWhatWasTyped(t *testing.T) {
	client, _, _ := newUI(t)
	_, form := get(t, client, "/report")

	stay := *client
	stay.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := stay.PostForm("http://ui/report/send", map[string][]string{
		"csrf": {csrfToken(t, form)}, "file": {"1"},
		"subject": {"Restore failed on studio"},
		"body":    {"It stopped at exit status 2. password=remove-me"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("filing answered %d, not a redirect to the tracker", resp.StatusCode)
	}

	target, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("the filing redirect does not parse: %v", err)
	}
	if !strings.HasPrefix(resp.Header.Get("Location"), bugreport.PublicReportURL) {
		t.Fatalf("filing goes to %q, not the tracker", resp.Header.Get("Location"))
	}
	if got := target.Query().Get("title"); got != "Restore failed on studio" {
		t.Errorf("the tracker's form opens with title %q", got)
	}
	if got := target.Query().Get("body"); !strings.Contains(got, "exit status 2") {
		t.Errorf("the tracker's form opens with body %q", got)
	}

	// What the operator reviewed is redacted, so what the link carries to
	// a public tracker has to be redacted too, or the preview's promise is
	// worth nothing.
	if strings.Contains(resp.Header.Get("Location"), "remove-me") {
		t.Error("filing carries an unredacted password to the tracker")
	}
}

// TestSettingsHoldNothingAboutReporting keeps the settings page to
// settings. Where reports go is compiled in and cannot be typed over, so a
// box holding it is a control that does nothing sitting among controls
// that do -- and reporting needs nothing installed for it to explain.
func TestSettingsHoldNothingAboutReporting(t *testing.T) {
	client, _, _ := newUI(t)
	_, settings := get(t, client, "/settings")
	for _, gone := range []string{bugreport.PublicReportURL, "Where bug reports go",
		"bugs-intake.key", "mode 0600", "/api/v1/intake"} {
		if strings.Contains(settings, gone) {
			t.Errorf("settings still carry %q", gone)
		}
	}
}

// TestNothingAsksForAnIntakeKeyOnDisk fails while any code path still reads
// a credential for reporting. The file name is the one that was documented,
// so a server that has one is not silently still using it.
func TestNothingAsksForAnIntakeKeyOnDisk(t *testing.T) {
	client, _, engine := newUI(t)
	settings, err := engine.Store().Settings()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(settings.ConfigDir, 0700); err != nil {
		t.Fatal(err)
	}
	// A leftover key from an earlier release must change nothing.
	if err := os.WriteFile(filepath.Join(settings.ConfigDir, "bugs-intake.key"),
		[]byte("leftover-credential-1234567890"), 0600); err != nil {
		t.Fatal(err)
	}
	_, form := get(t, client, "/report")
	if regexp.MustCompile(`<button\b[^>]*\bname="send"`).MatchString(form) ||
		strings.Contains(form, "leftover-credential") {
		t.Fatal("a leftover key file brought sending back")
	}
}

// TestFilingItIsOnTheSamePageAsDownloadingIt keeps the two halves of the
// one action together. The report is downloaded here and filed somewhere
// else, so a page that hands over a file without saying where it goes has
// only done half of it -- and an operator who has just pressed Download is
// looking at the button bar, not at the paragraph above the form.
func TestFilingItIsOnTheSamePageAsDownloadingIt(t *testing.T) {
	client, _, _ := newUI(t)
	_, form := get(t, client, "/report")

	bar := form[strings.Index(form, `name="download"`):]
	if end := strings.Index(bar, "</div>"); end >= 0 {
		bar = bar[:end]
	}
	if !strings.Contains(bar, `name="file"`) {
		t.Error("the button that downloads the report is not beside the one that files it")
	}
}
