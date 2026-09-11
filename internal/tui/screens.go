package tui

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/shukiv/gniza/internal/human"
)

// The pages' views, as much of each as the screens read. A field the
// page has and this does not is ignored, which is how the two sides can
// move at different speeds.

type account struct {
	User       string
	HomeDir    string
	Databases  []string
	SizeBytes  uint64
	LastBackup *time.Time
	LastStatus string
	Running    bool
	Condition  string
	Because    string
	Stored     uint64
	Runs       int
	Succeeded  int
	LastError  string
}

type overview struct {
	Hostname      string
	Sentence      string
	Severity      string
	Accounts      []account
	Destinations  []destinationRow
	Policies      []json.RawMessage
	Protected     int
	Stale         int
	Unprotected   int
	Failed        int
	Attention     []account
	NextRun       string
	NextRunPolicy string
	NextRunIn     string
	StagingFree   uint64
	SpaceTight    bool
	Managed       uint64
	LastRun       *struct {
		Policy     string
		StartedAt  time.Time
		FinishedAt time.Time
		Accounts   int
		Succeeded  int
		Failed     int
		Partial    int
		BytesAdded uint64
	}
	Held struct {
		Copies int
		Stored uint64
	}
}

type destinationRow struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	Endpoint       string
	Status         string
	LastCheckError string `json:"last_check_error"`
	Repository     struct {
		ID              string     `json:"id"`
		InitialisedAt   *time.Time `json:"initialised_at"`
		RecoveryNotedAt *time.Time `json:"recovery_noted_at"`
	}
	Space struct {
		TotalBytes  uint64 `json:"total_bytes"`
		FreeBytes   uint64 `json:"free_bytes"`
		Error       string `json:"error"`
		Unsupported bool   `json:"unsupported"`
	} `json:"space"`
}

type destinationsPage struct {
	Destinations []destinationRow
	Hostname     string
	Unnoted      []string
}

type policyRow struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	ScheduleCron  string   `json:"schedule_cron"`
	PayloadMode   string   `json:"payload_mode"`
	Enabled       bool     `json:"enabled"`
	IncludeSystem bool     `json:"include_system"`
	RepositoryIDs []string `json:"repository_ids"`
	Accounts      []string `json:"accounts"`
	Retention     struct {
		KeepDaily   int `json:"keep_daily"`
		KeepWeekly  int `json:"keep_weekly"`
		KeepMonthly int `json:"keep_monthly"`
	} `json:"retention"`
	LastRunAt    *time.Time `json:"last_run_at"`
	AccountCount int
	Next         time.Time
}

type schedulesPage struct {
	Policies     []policyRow
	Destinations []destinationRow
	Accounts     []account
}

type accountsPage struct {
	Accounts    []account
	Warnings    []string
	Protected   int
	Unprotected int
	RunAll      *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
}

type jobRow struct {
	ID         string `json:"id"`
	Account    string `json:"account"`
	Status     string `json:"status"`
	StagingErr string `json:"staging_error"`
	Warnings   []string
	Targets    []struct {
		RepositoryID   string  `json:"repository_id"`
		Status         string  `json:"status"`
		BytesAdded     uint64  `json:"bytes_added"`
		BytesProcessed uint64  `json:"bytes_processed"`
		DurationSecs   float64 `json:"duration_seconds"`
		Error          string  `json:"error"`
	} `json:"targets"`
	QueuedAt   time.Time  `json:"queued_at"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

type restoreRow struct {
	ID            string     `json:"id"`
	Account       string     `json:"account"`
	Kind          string     `json:"kind"`
	ItemKind      string     `json:"item_kind"`
	ItemNames     []string   `json:"item_names"`
	Status        string     `json:"status"`
	Apply         bool       `json:"apply"`
	BytesRestored uint64     `json:"bytes_restored"`
	RestoredTo    string     `json:"restored_to"`
	Error         string     `json:"error"`
	QueuedAt      time.Time  `json:"queued_at"`
	FinishedAt    *time.Time `json:"finished_at"`
}

type logsPage struct {
	Tab         string
	Jobs        []jobRow
	System      []jobRow
	Restores    []restoreRow
	Destination map[string]string
	Counts      map[string]int
	Lifecycle   []struct {
		Event   string    `json:"event"`
		Account string    `json:"account"`
		OK      bool      `json:"ok"`
		Detail  string    `json:"detail"`
		At      time.Time `json:"at"`
	}
	Service struct {
		Text  string
		Error string
	}
}

type restorePage struct {
	Restores []restoreRow
	Accounts []account
}

type settingsPage struct {
	Settings struct {
		Hostname       string  `json:"hostname"`
		StagingRoot    string  `json:"staging_root"`
		MaxConcurrent  int     `json:"max_concurrent"`
		SafetyMargin   float64 `json:"safety_margin"`
		ResticBinary   string  `json:"restic_binary"`
		ResticCache    string  `json:"restic_cache"`
		ConfigDir      string  `json:"config_dir"`
		KeepOutputDays int     `json:"keep_output_days"`
		LogLevel       string  `json:"log_level"`
		UpdateChannel  string  `json:"update_channel"`
		NoUpdateCheck  bool    `json:"no_update_check"`
	}
	StagingFree uint64
	OutputBytes uint64
	Version     string
	LastChecked string
	CheckError  string
	Update      struct {
		Running    string
		Latest     string
		ReleaseURL string
		Checked    string
		Newer      bool
		Unreleased bool
		Installing bool
		Error      string
	}
}

func decode[T any](m Model, which screen) T {
	var out T
	if m.loaded[which] {
		_ = json.Unmarshal(m.pages[which].Data, &out)
	}
	return out
}

var logTabs = []string{"backups", "system", "restores", "lifecycle", "service"}

// rowCount is how many rows the cursor can be on.
func (m Model) rowCount(which screen) int {
	switch which {
	case screenDestinations:
		return len(decode[destinationsPage](m, which).Destinations)
	case screenSchedules:
		return len(decode[schedulesPage](m, which).Policies)
	case screenAccounts:
		return len(decode[accountsPage](m, which).Accounts)
	case screenLogs:
		logs := decode[logsPage](m, which)
		switch m.logTab {
		case "system":
			return len(logs.System)
		case "restores":
			return len(logs.Restores)
		case "lifecycle":
			return len(logs.Lifecycle)
		case "service":
			return 0
		}
		return len(logs.Jobs)
	case screenRestore:
		return len(decode[restorePage](m, which).Restores)
	}
	return 0
}

// screenKeys names the keys this screen answers to.
func (m Model) screenKeys() string {
	switch m.screen {
	case screenDestinations:
		return "a add · t test · K show recovery key · n noted · d remove"
	case screenSchedules:
		return "a add · e edit · R run now · d remove"
	case screenAccounts:
		return "b back up · B back up every account"
	case screenLogs:
		return "t next tab"
	case screenSettings:
		return "u check for an update · i install it"
	}
	return ""
}

func (m Model) screenKey(key string) (tea.Model, tea.Cmd) {
	switch m.screen {
	case screenDestinations:
		return m.destinationsKey(key)
	case screenSchedules:
		return m.schedulesKey(key)
	case screenAccounts:
		return m.accountsKey(key)
	case screenLogs:
		if key == "t" {
			for i, tab := range logTabs {
				if tab == m.logTab {
					m.logTab = logTabs[(i+1)%len(logTabs)]
					break
				}
			}
			m.cursor[screenLogs] = 0
			return m, m.load(screenLogs)
		}
	case screenSettings:
		return m.settingsKey(key)
	}
	return m, nil
}

func (m Model) screenView() string {
	if !m.loaded[m.screen] {
		return styleDim.Render("Reading…")
	}
	switch m.screen {
	case screenOverview:
		return m.overviewView()
	case screenDestinations:
		return m.destinationsView()
	case screenSchedules:
		return m.schedulesView()
	case screenAccounts:
		return m.accountsView()
	case screenLogs:
		return m.logsView()
	case screenRestore:
		return m.restoreView()
	case screenSettings:
		return m.settingsView()
	}
	return ""
}

// --- overview ---

func (m Model) overviewView() string {
	v := decode[overview](m, screenOverview)
	var b strings.Builder
	style := styleOK
	switch v.Severity {
	case "warn":
		style = styleWarn
	case "bad":
		style = styleBad
	}
	b.WriteString(style.Render(v.Sentence) + "\n\n")
	fmt.Fprintf(&b, "%-18s %s\n", "Server", v.Hostname)
	fmt.Fprintf(&b, "%-18s %d accounts · %d destinations · %d schedules\n", "Configured",
		len(v.Accounts), len(v.Destinations), len(v.Policies))
	fmt.Fprintf(&b, "%-18s %d protected · %d stale · %d never backed up · %d failing\n", "Coverage",
		v.Protected, v.Stale, v.Unprotected, v.Failed)
	if v.NextRun != "" {
		fmt.Fprintf(&b, "%-18s %s, %s (%s)\n", "Next run", v.NextRunPolicy, v.NextRun, v.NextRunIn)
	} else {
		fmt.Fprintf(&b, "%-18s %s\n", "Next run", "nothing is scheduled")
	}
	staging := human.Bytes(v.StagingFree) + " free"
	if v.SpaceTight {
		staging = styleWarn.Render(staging + " · tight")
	}
	fmt.Fprintf(&b, "%-18s %s\n", "Staging space", staging)
	if v.Held.Copies > 0 {
		fmt.Fprintf(&b, "%-18s %d backups, %s after deduplication\n", "Destinations hold", v.Held.Copies, human.Bytes(v.Held.Stored))
	}
	if run := v.LastRun; run != nil {
		took := ""
		if !run.StartedAt.IsZero() && !run.FinishedAt.IsZero() {
			took = ", took " + since(run.FinishedAt.Sub(run.StartedAt))
		}
		fmt.Fprintf(&b, "%-18s %s: %d of %d accounts succeeded, %d failed, %s added%s\n", "Last run",
			run.Policy, run.Succeeded, run.Accounts, run.Failed, human.Bytes(run.BytesAdded), took)
	}
	if len(v.Attention) > 0 {
		b.WriteString("\n" + styleTitle.Render("Needs attention") + "\n")
		for _, a := range v.Attention {
			fmt.Fprintf(&b, "  %-24s %s\n", a.User, a.Because)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// --- destinations ---

func (m Model) destinationsView() string {
	v := decode[destinationsPage](m, screenDestinations)
	if len(v.Destinations) == 0 {
		return "No destination yet. Nothing is backed up until there is one.\n\nPress a to add one."
	}
	rows := make([][]string, 0, len(v.Destinations))
	for _, d := range v.Destinations {
		key := "saved"
		if d.Repository.RecoveryNotedAt == nil {
			key = "NOT SAVED"
		}
		status := d.Status
		if d.LastCheckError != "" {
			status += ": " + d.LastCheckError
		}
		space := ""
		switch {
		case d.Space.Unsupported:
			space = "n/a"
		case d.Space.TotalBytes > 0:
			space = human.Bytes(d.Space.FreeBytes) + " free"
		}
		rows = append(rows, []string{d.Name, d.Type, key, space, d.Endpoint, status})
	}
	out := table(m.width, m.cursor[screenDestinations],
		[]string{"Name", "Type", "Recovery key", "Space", "Where", "Status"}, rows)
	if len(v.Unnoted) > 0 {
		out += "\n\n" + styleBad.Render(fmt.Sprintf(
			"The recovery key for %s exists nowhere but this server. Press K to read it, then n once it is written down.",
			strings.Join(v.Unnoted, ", ")))
	}
	return out
}

func (m Model) currentDestination() (destinationRow, bool) {
	v := decode[destinationsPage](m, screenDestinations)
	i := m.cursor[screenDestinations]
	if i < 0 || i >= len(v.Destinations) {
		return destinationRow{}, false
	}
	return v.Destinations[i], true
}

func (m Model) destinationsKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "a":
		m.form = destinationForm(decode[destinationsPage](m, screenDestinations).Hostname)
		m.mode = modeForm
		return m, nil
	}
	d, ok := m.currentDestination()
	if !ok {
		return m, nil
	}
	switch key {
	case "t":
		m.busy = "Testing " + d.Name + "…"
		return m, m.post("/destinations/test", url.Values{"id": {d.ID}}, intentAct)
	case "K":
		m.busy = "Reading the key…"
		return m, m.post("/destinations/recovery", url.Values{"repository": {d.Repository.ID}}, intentSubmit)
	case "n":
		m.busy = "Noting…"
		return m, m.post("/destinations/recovery/note", url.Values{"repository": {d.Repository.ID}}, intentAct)
	case "d":
		m.confirm = &confirmation{
			question: fmt.Sprintf("Remove the destination %q?", d.Name),
			warning: "Its repository password is revoked on this server. The backups there stay, " +
				"but can only be read again with the recovery key you wrote down.",
			path: "/destinations/delete", form: url.Values{"id": {d.ID}}, intent: intentAct,
		}
		m.mode = modeConfirm
		return m, nil
	}
	return m, nil
}

func destinationForm(hostname string) *form {
	types := []choice{
		{"local", "Local disk or mounted NAS"},
		{"sftp", "Another Linux server (SFTP)"},
		{"s3", "S3 or S3-compatible"},
		{"rest", "Backup server (restic REST)"},
	}
	return newForm("Add a destination", "/destinations/add",
		textField("name", "Name", "", "What to call it on these screens."),
		choiceField("type", "Type", types, "local"),
		withWhen(textField("root", "Directory", "", "An absolute path on this server, or a mounted share. Created if missing."), whenType("local")),
		withWhen(textField("host", "Host", "", "The server's address."), whenType("sftp")),
		withWhen(textField("port", "Port", "22", ""), whenType("sftp")),
		withWhen(textField("user", "User", "", "The account on that server the backups are written as."), whenType("sftp")),
		withWhen(textField("root", "Remote directory", "", "Where under that account the repository goes."), whenType("sftp")),
		withWhen(secretField("password", "Password", "That user's password, used once to install Gniza's key and then discarded. Leave empty if the key is already installed."), whenType("sftp")),
		withWhen(textField("identity_file", "Existing key", "", "A private key file to use instead of making one. Usually empty."), whenType("sftp")),
		withWhen(textField("bucket", "Bucket", "", ""), whenType("s3")),
		withWhen(textField("endpoint", "Endpoint", "", "Empty for AWS; the service's URL for anything else."), whenType("s3")),
		withWhen(textField("region", "Region", "", ""), whenType("s3")),
		withWhen(textField("access_key_id", "Access key ID", "", ""), whenType("s3")),
		withWhen(secretField("secret_access_key", "Secret access key", ""), whenType("s3")),
		withWhen(textField("base_url", "Server URL", "", "https://backups.example.net:8000/"), whenType("rest")),
		withWhen(textField("username", "Username", "", ""), whenType("rest")),
		withWhen(secretField("password", "Password", ""), whenType("rest")),
		textField("repo_path", "Repository name", hostname, "The directory the repository is in at the destination. This server's name is the usual choice."),
	)
}

func withWhen(f field, when func(*form) bool) field {
	f.when = when
	return f
}

// --- schedules ---

func (m Model) schedulesView() string {
	v := decode[schedulesPage](m, screenSchedules)
	if len(v.Policies) == 0 {
		return "No schedule yet. A schedule is what makes backups happen on their own.\n\nPress a to add one."
	}
	names := map[string]string{}
	for _, d := range v.Destinations {
		names[d.Repository.ID] = d.Name
	}
	rows := make([][]string, 0, len(v.Policies))
	for _, p := range v.Policies {
		state := "on"
		if !p.Enabled {
			state = "off"
		}
		var to []string
		for _, id := range p.RepositoryIDs {
			if name, known := names[id]; known {
				to = append(to, name)
			} else {
				to = append(to, id)
			}
		}
		covers := "every account"
		if len(p.Accounts) > 0 {
			covers = fmt.Sprintf("%d accounts", len(p.Accounts))
		}
		if p.IncludeSystem {
			covers += " + server"
		}
		next := ""
		if !p.Next.IsZero() {
			next = "in " + since(time.Until(p.Next))
		}
		keeps := fmt.Sprintf("%d/%d/%d", p.Retention.KeepDaily, p.Retention.KeepWeekly, p.Retention.KeepMonthly)
		rows = append(rows, []string{p.Name, state, p.ScheduleCron, next, covers, keeps, strings.Join(to, ", ")})
	}
	return table(m.width, m.cursor[screenSchedules],
		[]string{"Name", "On", "When (cron)", "Next", "Covers", "Keep d/w/m", "Writes to"}, rows)
}

func (m Model) currentPolicy() (policyRow, bool) {
	v := decode[schedulesPage](m, screenSchedules)
	i := m.cursor[screenSchedules]
	if i < 0 || i >= len(v.Policies) {
		return policyRow{}, false
	}
	return v.Policies[i], true
}

func (m Model) schedulesKey(key string) (tea.Model, tea.Cmd) {
	v := decode[schedulesPage](m, screenSchedules)
	switch key {
	case "a":
		m.form = scheduleForm(v.Destinations, nil)
		m.mode = modeForm
		return m, nil
	}
	p, ok := m.currentPolicy()
	if !ok {
		return m, nil
	}
	switch key {
	case "e":
		m.form = scheduleForm(v.Destinations, &p)
		m.mode = modeForm
		return m, nil
	case "R":
		m.busy = "Queuing " + p.Name + "…"
		return m, m.post("/schedule/run", url.Values{"id": {p.ID}}, intentAct)
	case "d":
		m.confirm = &confirmation{
			question: fmt.Sprintf("Remove the schedule %q? Existing backups are untouched.", p.Name),
			path:     "/schedule/delete", form: url.Values{"id": {p.ID}}, intent: intentAct,
		}
		m.mode = modeConfirm
		return m, nil
	}
	return m, nil
}

func scheduleForm(destinations []destinationRow, editing *policyRow) *form {
	p := policyRow{Name: "Nightly", ScheduleCron: "0 2 * * *", PayloadMode: "split", Enabled: true, IncludeSystem: true}
	p.Retention.KeepDaily, p.Retention.KeepWeekly, p.Retention.KeepMonthly = 7, 4, 6
	title := "Add a schedule"
	if editing != nil {
		p = *editing
		title = "Edit the schedule"
	}
	writes := map[string]bool{}
	for _, id := range p.RepositoryIDs {
		writes[id] = true
	}
	fields := []field{
		textField("name", "Name", p.Name, ""),
		textField("cron", "When (cron)", p.ScheduleCron, "Five fields: minute hour day month weekday. 0 2 * * * is two in the morning, every day."),
		choiceField("mode", "Shape", []choice{{"split", "split (files read in place)"}, {"monolithic", "monolithic (one archive)"}}, p.PayloadMode),
		toggleField("enabled", "Enabled", "1", p.Enabled),
		toggleField("include_system", "Back up the server's own configuration too", "1", p.IncludeSystem),
		textField("keep_daily", "Keep daily", fmt.Sprint(p.Retention.KeepDaily), "How many of the last daily backups to keep."),
		textField("keep_weekly", "Keep weekly", fmt.Sprint(p.Retention.KeepWeekly), ""),
		textField("keep_monthly", "Keep monthly", fmt.Sprint(p.Retention.KeepMonthly), ""),
	}
	for _, d := range destinations {
		on := writes[d.Repository.ID]
		if editing == nil && len(destinations) == 1 {
			on = true
		}
		fields = append(fields, toggleField("repository", "Write to "+d.Name, d.Repository.ID, on))
	}
	f := newForm(title, "/schedule/save", fields...)
	f.fixed.Set("scope", "all")
	if editing != nil {
		f.fixed.Set("id", editing.ID)
		if len(editing.Accounts) > 0 {
			f.fixed.Set("scope", "selected")
			f.fixed["account"] = editing.Accounts
		}
	}
	return f
}

// --- accounts ---

func (m Model) accountsView() string {
	v := decode[accountsPage](m, screenAccounts)
	if len(v.Accounts) == 0 {
		return "No accounts were found. On a server with no panel, an account is a directory under one of the roots in /etc/gniza/plain.env."
	}
	rows := make([][]string, 0, len(v.Accounts))
	for _, a := range v.Accounts {
		state := a.Condition
		if a.Running {
			state = "running"
		}
		record := ""
		if a.Runs > 0 {
			record = fmt.Sprintf("%d/%d ok", a.Succeeded, a.Runs)
		}
		rows = append(rows, []string{a.User, state, ago(a.LastBackup), human.Bytes(a.SizeBytes),
			fmt.Sprint(len(a.Databases)), record, a.Because})
	}
	out := table(m.width, m.cursor[screenAccounts],
		[]string{"Account", "State", "Last backup", "Size", "DBs", "Record", "Why"}, rows)
	for _, warning := range v.Warnings {
		out += "\n" + styleWarn.Render(pad(warning, m.width-2))
	}
	return out
}

func (m Model) accountsKey(key string) (tea.Model, tea.Cmd) {
	v := decode[accountsPage](m, screenAccounts)
	switch key {
	case "b":
		i := m.cursor[screenAccounts]
		if i < 0 || i >= len(v.Accounts) {
			return m, nil
		}
		a := v.Accounts[i]
		m.busy = "Queuing " + a.User + "…"
		return m, m.post("/accounts/backup", url.Values{"account": {a.User}}, intentAct)
	case "B":
		if v.RunAll == nil {
			m.err = "No enabled schedule covers every account, so there is nothing to run them all under. Add one on the Schedules screen."
			return m, nil
		}
		m.busy = "Queuing every account…"
		return m, m.post("/schedule/run", url.Values{"id": {v.RunAll.ID}}, intentAct)
	}
	return m, nil
}

// --- logs ---

func (m Model) logsView() string {
	v := decode[logsPage](m, screenLogs)
	var tabs []string
	for _, tab := range logTabs {
		label := tab
		if n, counted := v.Counts[tab]; counted && n > 0 {
			label = fmt.Sprintf("%s (%d)", tab, n)
		}
		if tab == m.logTab {
			label = styleActive.Render(" " + label + " ")
		} else {
			label = " " + label + " "
		}
		tabs = append(tabs, label)
	}
	out := strings.Join(tabs, "") + "\n\n"
	switch m.logTab {
	case "backups", "system":
		jobs := v.Jobs
		if m.logTab == "system" {
			jobs = v.System
		}
		if len(jobs) == 0 {
			return out + "Nothing has run yet."
		}
		rows := make([][]string, 0, len(jobs))
		for _, j := range jobs {
			var added uint64
			var secs float64
			problem := j.StagingErr
			var to []string
			for _, t := range j.Targets {
				added += t.BytesAdded
				secs += t.DurationSecs
				if t.Error != "" && problem == "" {
					problem = t.Error
				}
				if name, known := v.Destination[t.RepositoryID]; known {
					to = append(to, name)
				}
			}
			if problem == "" && len(j.Warnings) > 0 {
				problem = j.Warnings[0]
			}
			took := ""
			if secs > 0 {
				took = since(time.Duration(secs * float64(time.Second)))
			}
			started := j.StartedAt
			if started == nil {
				started = &j.QueuedAt
			}
			rows = append(rows, []string{when(started), j.Account, j.Status, human.Bytes(added), took, strings.Join(to, ", "), problem})
		}
		return out + table(m.width, m.cursor[screenLogs],
			[]string{"Started", "Account", "Status", "Added", "Took", "To", "Problem"}, rows)
	case "restores":
		return out + restoreTable(m.width, m.cursor[screenLogs], v.Restores)
	case "lifecycle":
		if len(v.Lifecycle) == 0 {
			return out + "No account has been created, changed or removed since Gniza was installed."
		}
		rows := make([][]string, 0, len(v.Lifecycle))
		for _, e := range v.Lifecycle {
			ok := "ok"
			if !e.OK {
				ok = "failed"
			}
			rows = append(rows, []string{when(&e.At), e.Event, e.Account, ok, e.Detail})
		}
		return out + table(m.width, m.cursor[screenLogs], []string{"At", "Event", "Account", "Result", "Detail"}, rows)
	case "service":
		if v.Service.Error != "" {
			return out + styleWarn.Render(v.Service.Error)
		}
		lines := strings.Split(strings.TrimRight(v.Service.Text, "\n"), "\n")
		keep := max(m.height-10, 5)
		if len(lines) > keep {
			lines = lines[len(lines)-keep:]
		}
		for i, line := range lines {
			lines[i] = pad(line, m.width-2)
		}
		return out + strings.Join(lines, "\n")
	}
	return out
}

func restoreTable(width, cursor int, restores []restoreRow) string {
	if len(restores) == 0 {
		return "No restore has been asked for yet."
	}
	rows := make([][]string, 0, len(restores))
	for _, r := range restores {
		what := r.Kind
		if r.ItemKind != "" {
			what = r.ItemKind
			if len(r.ItemNames) > 0 {
				what += ": " + strings.Join(r.ItemNames, ", ")
			}
		}
		where := r.RestoredTo
		if r.Error != "" {
			where = r.Error
		}
		rows = append(rows, []string{when(&r.QueuedAt), r.Account, what, r.Status, human.Bytes(r.BytesRestored), where})
	}
	return table(width, cursor, []string{"Asked", "Account", "What", "Status", "Size", "Where / problem"}, rows)
}

// --- restore ---

func (m Model) restoreView() string {
	v := decode[restorePage](m, screenRestore)
	out := restoreTable(m.width, m.cursor[screenRestore], v.Restores)
	return out + "\n\n" + styleDim.Render(
		"Asking for a restore from the terminal is not written yet. Until it is, the restore "+
			"forms are posted with curl as docs/guide/plain-server.md shows; what they do shows here.")
}

// --- settings ---

func (m Model) settingsView() string {
	v := decode[settingsPage](m, screenSettings)
	var b strings.Builder
	row := func(label, value string) { fmt.Fprintf(&b, "%-22s %s\n", label, value) }
	row("Version", v.Version)
	switch {
	case v.Update.Installing:
		row("Update", styleWarn.Render("installing "+v.Update.Latest+"…"))
	case v.Update.Error != "":
		row("Update", styleBad.Render(v.Update.Error))
	case v.Update.Newer:
		row("Update", styleWarn.Render(v.Update.Latest+" is published; press i to install it"))
	case v.Update.Latest != "":
		row("Update", "none: "+v.Update.Latest+" is the newest")
	}
	if v.LastChecked != "" {
		checked := v.LastChecked
		if v.CheckError != "" {
			checked += " · " + v.CheckError
		}
		row("Last checked", checked)
	}
	b.WriteString("\n")
	row("Hostname", v.Settings.Hostname)
	row("Staging directory", fmt.Sprintf("%s (%s free)", v.Settings.StagingRoot, human.Bytes(v.StagingFree)))
	row("Accounts at once", fmt.Sprint(v.Settings.MaxConcurrent))
	row("restic", v.Settings.ResticBinary)
	row("restic cache", v.Settings.ResticCache)
	row("Configuration", v.Settings.ConfigDir)
	if v.Settings.LogLevel != "" {
		row("Log level", v.Settings.LogLevel)
	}
	if v.Settings.UpdateChannel != "" {
		row("Update channel", v.Settings.UpdateChannel)
	}
	b.WriteString("\n" + styleDim.Render("Changing these from the terminal is not written yet; the settings form is posted with curl as the guide shows."))
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) settingsKey(key string) (tea.Model, tea.Cmd) {
	v := decode[settingsPage](m, screenSettings)
	switch key {
	case "u":
		m.busy = "Asking about releases…"
		return m, m.post("/settings/update/check", url.Values{}, intentAct)
	case "i":
		if !v.Update.Newer {
			m.err = "There is no newer release to install. Press u to check again."
			return m, nil
		}
		m.confirm = &confirmation{
			question: fmt.Sprintf("Install %s over %s?", v.Update.Latest, v.Version),
			warning:  "The service restarts on the new build. A backup running now finishes first.",
			path:     "/settings/update/install",
			form:     url.Values{"version": {v.Update.Latest}, "confirm": {"1"}},
			intent:   intentAct,
		}
		m.mode = modeConfirm
		return m, nil
	}
	return m, nil
}

// --- forms ---

func (m Model) formKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "esc" {
		m.form = nil
		m.mode = modeList
		return m, nil
	}
	if !m.form.update(msg) {
		return m, nil
	}
	m.form.err = ""
	m.busy = "Sending…"
	return m, m.post(m.form.path, m.form.values(), intentSubmit)
}
