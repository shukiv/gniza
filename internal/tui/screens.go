package tui

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The pages' views, as much of each as the screens read. A field the
// page has and this does not is ignored, which is how the two sides can
// move at different speeds.

type account struct {
	User       string
	HomeDir    string
	Databases  []string
	Source     *sourceRow
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
	// Choose is on the page of a server without a panel, where the
	// accounts are what the operator chose (ADR 0025).
	Choose *struct {
		Candidates candidates
	}
}

// sourceRow is what an account on a server without a panel was chosen
// as: a folder, databases, a container's mount.
type sourceRow struct {
	Path       string   `json:"path"`
	MySQL      []string `json:"mysql"`
	PostgreSQL []string `json:"postgresql"`
	Container  *struct {
		Engine string `json:"engine"`
		Name   string `json:"name"`
		Mount  string `json:"mount"`
	} `json:"container"`
}

// candidates is what could be chosen now.
type candidates struct {
	Roots           []string
	Folders         []string
	MySQL           []string
	MySQLError      string
	PostgreSQL      []string
	PostgreSQLError string
	Containers      []struct {
		Engine, Name, Image, Status string
		Chosen                      bool
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

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
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
		return sMuted.Render("Reading…")
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

// --- destinations ---

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
		{"local", "Local disk"},
		{"sftp", "SFTP server"},
		{"s3", "S3"},
		{"rest", "REST server"},
	}
	return newForm("Add a destination", "/destinations/add",
		textField("name", "Name", "", "What to call it on these screens."),
		withHelp(choiceField("type", "Type", types, "local"),
			"A local disk or mounted NAS; another Linux server over SFTP; an S3 or S3-compatible bucket; a restic REST server."),
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

func withHelp(f field, help string) field {
	f.help = help
	return f
}

// --- schedules ---

func (m Model) currentPolicy() (policyRow, bool) {
	v := decode[schedulesPage](m, screenSchedules)
	i := m.cursor[screenSchedules]
	if i < 0 || i >= len(v.Policies) {
		return policyRow{}, false
	}
	return v.Policies[i], true
}

// withoutRecoveryKey names the destinations whose recovery key has not
// been noted as stored off this server: the ones a schedule cannot be
// saved for yet.
func withoutRecoveryKey(destinations []destinationRow) []string {
	var names []string
	for _, d := range destinations {
		if d.Repository.ID != "" && d.Repository.RecoveryNotedAt == nil {
			names = append(names, d.Name)
		}
	}
	return names
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
		withHelp(choiceField("mode", "Shape", []choice{{"split", "split"}, {"monolithic", "monolithic"}}, p.PayloadMode),
			"split reads the files where they lie and keeps the dumps beside them; monolithic makes one archive first. A server with no panel has only split."),
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

func (m Model) accountsKey(key string) (tea.Model, tea.Cmd) {
	v := decode[accountsPage](m, screenAccounts)
	if v.Choose != nil {
		switch key {
		case "a", "c":
			m.busy = "Looking at what there is to choose from…"
			return m, m.offer(key)
		case "d":
			i := m.cursor[screenAccounts]
			if i < 0 || i >= len(v.Accounts) {
				return m, nil
			}
			a := v.Accounts[i]
			m.confirm = &confirmation{
				question: fmt.Sprintf("Stop backing up %q?", a.User),
				warning:  "The backups already taken of it stay at the destinations, and its history stays here.",
				path:     "/accounts/remove", form: url.Values{"account": {a.User}}, intent: intentAct,
			}
			m.mode = modeConfirm
			return m, nil
		}
	}
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

// sourceForm chooses a folder and the databases beside it. The folders
// under the roots are said in the help rather than offered as a choice,
// because any folder on the server can be named.
// chooseWith opens the form the key asked for, with what the server
// offered just now.
func (m Model) chooseWith(key string) (tea.Model, tea.Cmd) {
	m.busy = ""
	v := decode[accountsPage](m, screenAccounts)
	if v.Choose == nil {
		return m, nil
	}
	switch key {
	case "a":
		m.form = sourceForm(v.Choose.Candidates)
	case "c":
		form, ok := containerForm(v.Choose.Candidates)
		if !ok {
			m.err = "No container is left to choose: none is running here, or every one is chosen already."
			return m, nil
		}
		m.form = form
	default:
		return m, nil
	}
	m.mode = modeForm
	return m, nil
}

func sourceForm(offered candidates) *form {
	folders := "Any folder on this server, from /."
	if len(offered.Folders) > 0 {
		folders = "Under the roots there is: " + strings.Join(offered.Folders, ", ") + ". Any other folder can be named too."
	}
	fields := []field{
		textField("path", "Folder", "", folders+" Leave it empty for a source of databases only."),
		textField("name", "Name", "", "How it is called on these screens and in the backups. Empty names it after the folder."),
	}
	for _, name := range offered.MySQL {
		fields = append(fields, toggleField("mysql", "MySQL "+name, name, false))
	}
	for _, name := range offered.PostgreSQL {
		fields = append(fields, toggleField("postgresql", "PostgreSQL "+name, name, false))
	}
	f := newForm("Back up a folder, with its databases", "/accounts/add", fields...)
	switch {
	case offered.MySQLError != "" && offered.PostgreSQLError != "":
		f.fields[0].help += " No MySQL or PostgreSQL client answered, so no database is offered."
	case offered.MySQLError != "":
		f.fields[0].help += " No MySQL client answered."
	case offered.PostgreSQLError != "":
		f.fields[0].help += " No PostgreSQL client answered."
	}
	return f
}

// containerForm chooses a container that is not chosen yet. Each of its
// mounts becomes a source.
func containerForm(offered candidates) (*form, bool) {
	var choices []choice
	for _, c := range offered.Containers {
		if c.Chosen {
			continue
		}
		choices = append(choices, choice{c.Engine + "/" + c.Name, fmt.Sprintf("%s (%s · %s · %s)", c.Name, c.Engine, c.Image, c.Status)})
	}
	if len(choices) == 0 {
		return nil, false
	}
	return newForm("Back up a container", "/accounts/add",
		withHelp(choiceField("container", "Container", choices, choices[0].value),
			"Each volume and bind mount of it becomes a source, read where it lies on this machine, "+
				"with the container's description and compose file kept beside it. A database inside "+
				"a running container is not consistent when read this way: choose its database as a "+
				"folder source's database instead, or stop the container for the backup."),
	), true
}

// --- logs ---

// --- restore ---

// --- settings ---

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
			warning: "Backups already queued stay queued and run afterwards. The service " +
				"restarts, so this screen is unreachable for a few seconds.",
			path:   "/settings/update/install",
			form:   url.Values{"version": {v.Update.Latest}, "confirm": {"1"}},
			intent: intentAct,
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
