package webui

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/shukiv/gniza/internal/agent"
	"github.com/shukiv/gniza/internal/bugreport"
	"github.com/shukiv/gniza/internal/job"
)

// gatherReport collects what a maintainer needs to act on a report.
//
// Everything here is chosen rather than swept up: it goes into an issue
// that may be read by anybody. No credential reaches it by construction --
// the settings section names fields one at a time, and the destinations
// section carries kinds and names, never their configuration. The service
// log is the one part that is copied rather than built, so it goes through
// the redactor.
func (s *Server) gatherReport(ctx context.Context, subject, body string) bugreport.Report {
	report := bugreport.Report{Subject: subject, Body: body}
	add := func(title, text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		report.Sections = append(report.Sections, bugreport.Section{
			Title: title,
			Text:  bugreport.Clip(bugreport.Redact(text), bugreport.MaxSectionBytes),
		})
	}

	add("Versions and environment", s.reportEnvironment(ctx))
	add("Recent failures", s.reportFailures())
	add("Settings", s.reportSettings())
	add("Service log", s.reportLog(ctx))
	return report.Safe()
}

func (s *Server) reportEnvironment(ctx context.Context) string {
	var out strings.Builder
	fmt.Fprintf(&out, "gniza       %s\n", agent.Version)
	fmt.Fprintf(&out, "go           %s (%s/%s)\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if version, err := os.ReadFile("/usr/local/cpanel/version"); err == nil {
		fmt.Fprintf(&out, "cpanel       %s\n", strings.TrimSpace(string(version)))
	}
	if release, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(release), "\n") {
			if name, value, found := strings.Cut(line, "="); found && name == "PRETTY_NAME" {
				fmt.Fprintf(&out, "os           %s\n", strings.Trim(value, `"`))
			}
		}
	}
	if settings, err := s.engine.Store().Settings(); err == nil {
		binary := settings.ResticBinary
		if binary == "" {
			binary = "restic"
		}
		if version, err := exec.CommandContext(ctx, binary, "version").Output(); err == nil {
			fmt.Fprintf(&out, "restic       %s\n", strings.TrimSpace(string(version)))
		}
		fmt.Fprintf(&out, "hostname     %s\n", settings.Hostname)
	}
	return out.String()
}

// reportFailures is what this server has recently failed to do. A report
// that arrives with the errors already in it is one nobody has to ask
// three questions about first.
func (s *Server) reportFailures() string {
	store := s.engine.Store()
	var lines []string

	jobs, err := store.Jobs(0)
	if err == nil {
		sort.Slice(jobs, func(i, j int) bool { return jobs[i].QueuedAt.After(jobs[j].QueuedAt) })
		shown := 0
		for _, run := range jobs {
			if run.Status != job.StatusFailed && run.Status != job.StatusPartialSuccess {
				continue
			}
			trouble := failureOf(run)
			lines = append(lines, fmt.Sprintf("backup  %s  %s  %s  %s",
				stampOf(run.QueuedAt), run.Account, run.Status, trouble))
			if shown++; shown >= 10 {
				break
			}
		}
	}
	restores, err := store.Restores(0)
	if err == nil {
		sort.Slice(restores, func(i, j int) bool {
			return restores[i].QueuedAt.After(restores[j].QueuedAt)
		})
		shown := 0
		for _, run := range restores {
			if run.Status != job.StatusFailed {
				continue
			}
			lines = append(lines, fmt.Sprintf("restore %s  %s  %s  %s",
				stampOf(run.QueuedAt), run.Account, restoreRow{Restore: run}.Parts(), run.Error))
			if shown++; shown >= 10 {
				break
			}
		}
	}
	if len(lines) == 0 {
		return "nothing has failed recently"
	}
	return strings.Join(lines, "\n")
}

func stampOf(at time.Time) string { return at.UTC().Format("2006-01-02 15:04") }

// reportSettings names the settings that shape behaviour, one at a time.
// Building it field by field is what keeps a credential out of it: there
// is no path here that prints something merely because it is stored.
func (s *Server) reportSettings() string {
	settings, err := s.engine.Store().Settings()
	if err != nil {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "staging root           %s\n", settings.StagingRoot)
	fmt.Fprintf(&out, "accounts at once       %d\n", settings.MaxConcurrent)
	fmt.Fprintf(&out, "safety margin          %.2f\n", settings.SafetyMargin)
	fmt.Fprintf(&out, "keep restored files    %d days\n", keepDays(settings))
	fmt.Fprintf(&out, "keep deleted accounts  %d days\n", deletedDays(settings))
	fmt.Fprintf(&out, "block unsafe removal   %t\n", settings.ProtectAccountRemoval)
	fmt.Fprintf(&out, "back up on suspension  %t\n", settings.BackupOnSuspension)

	if destinations, err := s.engine.Store().Destinations(); err == nil {
		fmt.Fprintf(&out, "\ndestinations (%d)\n", len(destinations))
		for _, dest := range destinations {
			fmt.Fprintf(&out, "  %s  kind=%s  append_only=%t  last_error=%q\n",
				dest.Name, dest.Type, dest.AppendOnly, dest.LastCheckError)
		}
	}
	if policies, err := s.engine.Store().Policies(); err == nil {
		fmt.Fprintf(&out, "\nschedules (%d)\n", len(policies))
		for _, policy := range policies {
			fmt.Fprintf(&out, "  %s  cron=%q  enabled=%t  accounts=%d\n",
				policy.Name, policy.ScheduleCron, policy.Enabled, len(policy.Accounts))
		}
	}
	return out.String()
}

// reportLog is the tail of what the service logged. It is the most useful
// section and the only one copied rather than built, which is why it goes
// through the redactor on the way out.
func (s *Server) reportLog(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "journalctl", "-u", "gniza",
		"-n", "200", "--no-pager", "--output", "short-iso").Output()
	if err != nil {
		return "the service log could not be read: " + err.Error()
	}
	return string(out)
}

// reportView is the bug-report form and the report it prepares.
type reportView struct {
	Subject string
	Body    string
	// ReportURL is the tracker this is filed on. The page names it
	// because this server files nothing itself -- the operator does,
	// on the tracker's own form.
	ReportURL string
	// Preview is the whole report, shown before it is downloaded because
	// it is the operator who then carries it somewhere public.
	Preview   string
	Prepared  string
	Signature string
	Error     string
}

// handleReport draws the bug-report form.
//
// It is a page as well as a dialog: the dialog fetches this and lifts the
// form out of it, and a browser with scripting switched off gets the same
// form on a page of its own. Nothing about reporting a bug should need
// JavaScript -- the thing being reported may be the reason it is off.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	view := s.newReportView(r.URL.Query().Get("subject"), "")
	s.render(w, r, "report.html", "Report a problem", "", view)
}

func (s *Server) newReportView(subject, body string) reportView {
	return reportView{Subject: subject, Body: body,
		ReportURL: bugreport.PublicReportURL}
}

type preparedReport struct {
	Report  bugreport.Report
	Expires int64
}

func (s *Server) reportSignature(prepared string) string {
	mac := hmac.New(sha256.New, s.reportPreviewKey)
	_, _ = mac.Write([]byte(prepared))
	return hex.EncodeToString(mac.Sum(nil))
}

// handleSendReport gathers the debug information and either shows it or
// hands it over as a file. Nothing here transmits: the file is the report,
// and the operator files it on the public form.
//
// A release before this one could send from the server, and a page left
// open from then still asks for it. Such a request is answered with the
// report itself rather than an error -- what it wanted has not failed, it
// has moved.
func (s *Server) handleSendReport(w http.ResponseWriter, r *http.Request) {
	subject := strings.TrimSpace(r.PostFormValue("subject"))
	body := strings.TrimSpace(r.PostFormValue("body"))
	view := s.newReportView(subject, body)
	if subject == "" || body == "" {
		view.Error = "A report needs a subject and a description of what happened."
		s.render(w, r, "report.html", "Report a problem", "", view)
		return
	}
	if err := (bugreport.Report{Subject: subject, Body: body}).Validate(); err != nil {
		view.Error = err.Error()
		s.render(w, r, "report.html", "Report a problem", "", view)
		return
	}

	// Filing is a submit rather than a link because a link's target is
	// fixed when the page is drawn, which is before anything has been
	// typed into the form. The redirect is built from the fields as they
	// were posted, and redacted, because what it carries becomes public
	// and the operator reviewed a redacted copy.
	if r.PostFormValue("file") == "1" {
		typed := (bugreport.Report{Subject: subject, Body: body}).Safe()
		http.Redirect(w, r, bugreport.NewIssueURL(typed.Subject, typed.Body), http.StatusSeeOther)
		return
	}

	var report bugreport.Report
	if r.PostFormValue("download") == "1" && r.PostFormValue("prepared") != "" {
		// Hand over precisely the diagnostic snapshot the operator reviewed,
		// not newly gathered logs that might now say something different.
		view.Prepared, view.Signature = r.PostFormValue("prepared"), r.PostFormValue("signature")
		var prepared preparedReport
		valid := hmac.Equal([]byte(view.Signature), []byte(s.reportSignature(view.Prepared))) &&
			json.Unmarshal([]byte(view.Prepared), &prepared) == nil && prepared.Expires >= time.Now().Unix()
		typed := (bugreport.Report{Subject: subject, Body: body}).Safe()
		if !valid || prepared.Report.Subject != typed.Subject || prepared.Report.Body != typed.Body {
			view.Error = "Preview this report again before downloading; the preview expired or its contents changed."
			view.Prepared, view.Signature = "", ""
			s.render(w, r, "report.html", "Report a problem", "", view)
			return
		}
		report = prepared.Report
	} else {
		report = s.gatherReport(r.Context(), subject, body)
		prepared, err := json.Marshal(preparedReport{Report: report, Expires: time.Now().Add(20 * time.Minute).Unix()})
		if err != nil {
			view.Error = "The report could not be prepared. Try previewing it again."
			s.render(w, r, "report.html", "Report a problem", "", view)
			return
		}
		view.Prepared = string(prepared)
		view.Signature = s.reportSignature(view.Prepared)
	}
	view.Preview = report.Markdown()

	// The file is how the report leaves: the operator attaches it to the
	// public form. It is the whole report, the same text as the preview.
	if r.PostFormValue("download") == "1" {
		name := fmt.Sprintf("gniza-report-%s.md", time.Now().UTC().Format("20060102-1504"))
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fmt.Fprintf(w, "# %s\n\n%s", report.Subject, view.Preview)
		return
	}

	s.render(w, r, "report.html", "Report a problem", "", view)
}
