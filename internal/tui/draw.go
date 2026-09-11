package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/shukiv/gniza/internal/human"
)

// The screens drawn. Every screen is the same frame: a title bar, the
// tabs, one panel, and a foot that says what is happening and what the
// keys do. What changes is the panel.

func (m Model) View() string {
	head := m.header()
	foot := m.footer()
	// The panel takes what the frame leaves, so the foot sits at the
	// bottom of the terminal rather than under the last line of content.
	bodyHeight := m.height - lipgloss.Height(head) - lipgloss.Height(foot) - 3
	var body string
	switch m.mode {
	case modeForm:
		body = m.form.view(m.innerWidth())
	case modeConfirm:
		body = m.confirmView()
	case modeReveal:
		body = m.revealView()
	default:
		body = m.screenView()
	}
	panel := sPanel.Width(max(m.width-2, 20))
	if bodyHeight > lipgloss.Height(body) {
		panel = panel.Height(bodyHeight)
	}
	return head + "\n" + panel.Render(body) + "\n" + foot
}

// innerWidth is what a panel's content may use.
func (m Model) innerWidth() int { return max(m.width-6, 20) }

func (m Model) header() string {
	page := m.pages[m.screen]
	left := sBrand.Render("Gniza")
	if host := m.hostname(); host != "" {
		left += "  " + sTitle.Render(host)
	}
	var right []string
	if page.Panel != "" {
		right = append(right, page.Panel)
	}
	if page.Version != "" {
		right = append(right, page.Version)
	}
	rightText := sMuted.Render(strings.Join(right, " · "))
	if page.Update != nil {
		rightText = sWarn.Render("update "+page.Update.Version+" available") + "  " + rightText
	}
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(rightText)
	if gap < 2 {
		gap = 2
	}
	title := left + strings.Repeat(" ", gap) + rightText

	var tabs []string
	for which := screen(0); which < screenCount; which++ {
		label := fmt.Sprintf("%d %s", which+1, screenNames[which])
		if which == m.screen {
			tabs = append(tabs, sTabOn.Render(label))
		} else {
			tabs = append(tabs, sTabOff.Render(label))
		}
	}
	return title + "\n" + strings.Join(tabs, "")
}

// hostname is the server's name, from whichever page has said it.
func (m Model) hostname() string {
	if m.loaded[screenOverview] {
		return decode[overview](m, screenOverview).Hostname
	}
	if m.loaded[screenDestinations] {
		return decode[destinationsPage](m, screenDestinations).Hostname
	}
	return ""
}

func (m Model) footer() string {
	width := max(m.width-2, 20)
	var lines []string
	for _, work := range m.pages[m.screen].Running {
		line := sWarn.Render("▶ ") + sTitle.Render(work.Doing+" "+work.Account)
		if work.Detail != "" {
			line += sMuted.Render(" · " + work.Detail)
		}
		if work.Known {
			line += "  " + bar(work.Percent, 20) + fmt.Sprintf(" %3.0f%%", work.Percent)
		} else if work.Waiting {
			line += sMuted.Render("  waiting")
		}
		lines = append(lines, " "+line)
	}
	if m.busy != "" {
		lines = append(lines, " "+sMuted.Render("… "+m.busy))
	}
	if m.err != "" {
		lines = append(lines, indent(banner("error", m.err, width-1)))
	}
	if m.flash != nil {
		lines = append(lines, indent(banner(m.flash.Kind, m.flash.Message, width-1)))
	}
	lines = append(lines, " "+m.keysHelp())
	return strings.Join(lines, "\n")
}

func (m Model) keysHelp() string {
	switch m.mode {
	case modeForm:
		return chips("tab", "next field", "shift+tab", "back", "space", "toggle / cycle",
			"enter", "next, submit on the last", "ctrl+s", "submit", "esc", "cancel")
	case modeConfirm:
		return chips("y", "yes", "n", "no")
	case modeReveal:
		return chips("n", "I have written it down", "esc", "back")
	}
	common := chips("1-7", "screens", "j/k", "move", "r", "reread", "q", "quit")
	if keys := m.screenKeys(); keys != "" {
		return keys + "   " + common
	}
	return common
}

// screenKeys names the keys this screen answers to.
func (m Model) screenKeys() string {
	switch m.screen {
	case screenDestinations:
		return chips("a", "add", "t", "test", "K", "show recovery key", "n", "noted", "d", "remove")
	case screenSchedules:
		return chips("a", "add", "e", "edit", "R", "run now", "d", "remove")
	case screenAccounts:
		return chips("b", "back up", "B", "back up every account")
	case screenLogs:
		return chips("t", "next tab")
	case screenSettings:
		return chips("u", "check for an update", "i", "install it")
	}
	return ""
}

func (m Model) confirmView() string {
	c := m.confirm
	width := min(m.innerWidth(), 72)
	body := sTitle.Render(clipWrap(c.question, width-2)) + "\n"
	if c.warning != "" {
		body += "\n" + banner("warn", c.warning, width-2) + "\n"
	}
	body += "\n" + chips("y", "yes, do it", "n", "no")
	box := sCard.Width(width).Render(body)
	return lipgloss.PlaceHorizontal(m.innerWidth(), lipgloss.Center, box)
}

func (m Model) revealView() string {
	card := m.reveal.card
	width := min(m.innerWidth(), 96)
	var b strings.Builder
	if m.reveal.justCreated {
		b.WriteString(sOK.Render("✓ ") + sTitle.Render("The destination is ready.") + "  This is its recovery key.\n\n")
	} else {
		b.WriteString(sTitle.Render("Recovery key") + "\n\n")
	}
	b.WriteString(banner("bad", "It exists nowhere but this server until you write it down somewhere else. "+
		"Without it, the backups on this destination cannot be read if this server is lost.", width-2) + "\n\n")
	rows := [][2]string{
		{"Server", card.Hostname},
		{"Destination", card.Destination},
		{"Repository", card.Repository},
		{"Restic URI", card.URI},
	}
	if card.ResticOptions != "" {
		rows = append(rows, [2]string{"Restic options", card.ResticOptions})
	}
	for _, r := range rows {
		b.WriteString(row(r[0], r[1]) + "\n")
	}
	b.WriteString(row("Password", sKey.Render(card.Password)) + "\n\n")
	b.WriteString(sMuted.Render("Keep it in a password manager, not in a file on this machine.") + "\n")
	b.WriteString(sMuted.Render("Press ") + sKey.Render("n") + sMuted.Render(" once it is written down; the warning on the destinations screen stops then."))
	return sAlert.Width(width).Render(b.String())
}

// grid draws rows under headers with the cursor's row lit, in the width
// there is. Columns take their natural width; when that is more than
// there is, the widest gives way first, so a long path or error shrinks
// before a name or a status does. Status words are coloured wherever
// they appear.
func grid(width int, cursor int, headers []string, rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	const gap = 2
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = lipgloss.Width(h)
	}
	for _, r := range rows {
		for i, cell := range r {
			if i < len(widths) {
				widths[i] = max(widths[i], lipgloss.Width(cell))
			}
		}
	}
	total := func() int {
		sum := gap * (len(widths) - 1)
		for _, w := range widths {
			sum += w
		}
		return sum
	}
	for total() > width {
		widest := 0
		for i, w := range widths {
			if w > widths[widest] {
				widest = i
			}
		}
		if widths[widest] <= 6 {
			break
		}
		widths[widest]--
	}
	line := func(cells []string, style func(i int, cell string) string, between string) string {
		parts := make([]string, len(widths))
		for i := range widths {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			parts[i] = style(i, pad(cell, widths[i]))
		}
		return strings.Join(parts, between)
	}
	space := strings.Repeat(" ", gap)
	var b strings.Builder
	b.WriteString(" " + line(headers, func(_ int, cell string) string { return sHeader.Render(cell) }, space) + "\n")
	b.WriteString(sBorder.Render(strings.Repeat("─", min(total()+2, width))) + "\n")
	for i, r := range rows {
		selected := i == cursor
		between := space
		if selected {
			between = sSelected.Render(space)
		}
		text := line(r, func(_ int, cell string) string {
			if t, is := tone(cell); is {
				if selected {
					return t.Inherit(sSelected).Render(cell)
				}
				return t.Render(cell)
			}
			if selected {
				return sSelected.Render(cell)
			}
			return cell
		}, between)
		if selected {
			text = sSelected.Render(" ") + text + sSelected.Render(" ")
		} else {
			text = " " + text + " "
		}
		b.WriteString(text + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// indent puts one space before every line.
func indent(text string) string {
	return " " + strings.ReplaceAll(text, "\n", "\n ")
}

func clipWrap(text string, width int) string {
	return lipgloss.NewStyle().Width(width).Render(text)
}

// --- overview ---

func (m Model) overviewView() string {
	v := decode[overview](m, screenOverview)
	width := m.innerWidth()
	var b strings.Builder
	b.WriteString(banner(v.Severity, sTitle.Render(v.Sentence), width) + "\n\n")

	next := "nothing scheduled"
	nextNote := ""
	if v.NextRun != "" {
		next = v.NextRun
		nextNote = v.NextRunPolicy + ", " + v.NextRunIn
	}
	staging := human.Bytes(v.StagingFree)
	stagingNote := "free for staging"
	if v.SpaceTight {
		stagingNote = sWarn.Render("tight: a large account may not fit")
	}
	coverage := fmt.Sprintf("%d protected", v.Protected)
	switch {
	case v.Unprotected > 0:
		coverage = sBad.Render(fmt.Sprintf("%d never backed up", v.Unprotected))
	case v.Failed > 0:
		coverage = sBad.Render(fmt.Sprintf("%d failing", v.Failed))
	case v.Stale > 0:
		coverage = sWarn.Render(fmt.Sprintf("%d stale", v.Stale))
	}
	destNote := "none yet"
	if n := len(v.Destinations); n > 0 {
		names := make([]string, 0, n)
		for _, d := range v.Destinations {
			names = append(names, d.Name)
		}
		destNote = strings.Join(names, ", ")
	}
	// Four cards across, the last taking the remainder so the row is
	// as wide as the panel.
	each := width / 4
	last := width - 3*each
	b.WriteString(cards(width,
		stat(each, "accounts", fmt.Sprint(len(v.Accounts)), coverage, false),
		stat(each, "destinations", fmt.Sprint(len(v.Destinations)), destNote, len(v.Destinations) == 0),
		stat(each, "next run", next, nextNote, false),
		stat(last, "staging space", staging, stagingNote, v.SpaceTight),
	) + "\n")

	if run := v.LastRun; run != nil {
		took := ""
		if !run.StartedAt.IsZero() && !run.FinishedAt.IsZero() {
			took = ", took " + since(run.FinishedAt.Sub(run.StartedAt))
		}
		outcome := sOK.Render(fmt.Sprintf("%d of %d succeeded", run.Succeeded, run.Accounts))
		if run.Failed > 0 || run.Partial > 0 {
			outcome = sWarn.Render(fmt.Sprintf("%d of %d succeeded, %d failed, %d partial",
				run.Succeeded, run.Accounts, run.Failed, run.Partial))
		}
		b.WriteString("\n" + row("Last run", fmt.Sprintf("%s · %s · %s added%s", run.Policy, outcome, human.Bytes(run.BytesAdded), took)) + "\n")
	}
	if v.Held.Copies > 0 {
		b.WriteString(row("Destinations hold", fmt.Sprintf("%d backups, %s after deduplication", v.Held.Copies, human.Bytes(v.Held.Stored))) + "\n")
	}
	if len(v.Attention) > 0 {
		b.WriteString("\n" + sTitle.Render("Needs attention") + "\n")
		for _, a := range v.Attention {
			mark := sWarn.Render("•")
			if a.Condition == "never" || a.Condition == "failed" {
				mark = sBad.Render("•")
			}
			b.WriteString(fmt.Sprintf("  %s %s  %s\n", mark, sTitle.Render(pad(a.User, 20)), sMuted.Render(a.Because)))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// --- destinations ---

func (m Model) destinationsView() string {
	v := decode[destinationsPage](m, screenDestinations)
	width := m.innerWidth()
	if len(v.Destinations) == 0 {
		return empty(width, "No destination yet. Nothing is backed up until there is one.", "a", "add one")
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
	out := grid(width, m.cursor[screenDestinations],
		[]string{"Name", "Type", "Recovery key", "Space", "Where", "Status"}, rows)
	if len(v.Unnoted) > 0 {
		out += "\n\n" + banner("bad", fmt.Sprintf(
			"The recovery key for %s exists nowhere but this server. Press K to read it, then n once it is written down.",
			strings.Join(v.Unnoted, ", ")), width)
	}
	return out
}

// --- schedules ---

func (m Model) schedulesView() string {
	v := decode[schedulesPage](m, screenSchedules)
	width := m.innerWidth()
	if len(v.Policies) == 0 {
		return empty(width, "No schedule yet. A schedule is what makes backups happen on their own.", "a", "add one")
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
	return grid(width, m.cursor[screenSchedules],
		[]string{"Name", "On", "When (cron)", "Next", "Covers", "Keep d/w/m", "Writes to"}, rows)
}

// --- accounts ---

func (m Model) accountsView() string {
	v := decode[accountsPage](m, screenAccounts)
	width := m.innerWidth()
	if len(v.Accounts) == 0 {
		return empty(width, "No accounts were found.", "", "") + "\n\n" +
			sMuted.Render(clipWrap("On a server with no panel, an account is a directory under one of the roots in /etc/gniza/plain.env.", width))
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
	summary := fmt.Sprintf("%s · %s protected", plural(len(v.Accounts), "account"), fmt.Sprint(v.Protected))
	if v.Unprotected > 0 {
		summary += " · " + sBad.Render(fmt.Sprintf("%d never backed up", v.Unprotected))
	}
	out := sMuted.Render(summary) + "\n\n" + grid(width, m.cursor[screenAccounts],
		[]string{"Account", "State", "Last backup", "Size", "DBs", "Record", "Why"}, rows)
	for _, warning := range v.Warnings {
		out += "\n" + banner("warn", warning, width)
	}
	return out
}

// --- logs ---

func (m Model) logsView() string {
	v := decode[logsPage](m, screenLogs)
	width := m.innerWidth()
	var tabs []string
	for _, tab := range logTabs {
		label := tab
		if n, counted := v.Counts[tab]; counted && n > 0 {
			label = fmt.Sprintf("%s %d", tab, n)
		}
		if tab == m.logTab {
			tabs = append(tabs, sPill.Render(label))
		} else {
			tabs = append(tabs, sPillOff.Render(label))
		}
	}
	out := strings.Join(tabs, " ") + "\n\n"
	switch m.logTab {
	case "backups", "system":
		jobs := v.Jobs
		if m.logTab == "system" {
			jobs = v.System
		}
		if len(jobs) == 0 {
			return out + empty(width, "Nothing has run yet.", "", "")
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
		return out + grid(width, m.cursor[screenLogs],
			[]string{"Started", "Account", "Status", "Added", "Took", "To", "Problem"}, rows)
	case "restores":
		return out + restoreTable(width, m.cursor[screenLogs], v.Restores)
	case "lifecycle":
		if len(v.Lifecycle) == 0 {
			return out + empty(width, "No account has been created, changed or removed since Gniza was installed.", "", "")
		}
		rows := make([][]string, 0, len(v.Lifecycle))
		for _, e := range v.Lifecycle {
			ok := "ok"
			if !e.OK {
				ok = "failed"
			}
			rows = append(rows, []string{when(&e.At), e.Event, e.Account, ok, e.Detail})
		}
		return out + grid(width, m.cursor[screenLogs], []string{"At", "Event", "Account", "Result", "Detail"}, rows)
	case "service":
		if v.Service.Error != "" {
			return out + banner("warn", v.Service.Error, width)
		}
		lines := strings.Split(strings.TrimRight(v.Service.Text, "\n"), "\n")
		keep := max(m.height-12, 5)
		if len(lines) > keep {
			lines = lines[len(lines)-keep:]
		}
		for i, line := range lines {
			line = clip(line, width)
			switch {
			case strings.Contains(line, "level=ERROR"):
				line = sBad.Render(line)
			case strings.Contains(line, "level=WARN"):
				line = sWarn.Render(line)
			}
			lines[i] = line
		}
		return out + strings.Join(lines, "\n")
	}
	return out
}

func restoreTable(width, cursor int, restores []restoreRow) string {
	if len(restores) == 0 {
		return empty(width, "No restore has been asked for yet.", "", "")
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
	return grid(width, cursor, []string{"Asked", "Account", "What", "Status", "Size", "Where / problem"}, rows)
}

// --- restore ---

func (m Model) restoreView() string {
	v := decode[restorePage](m, screenRestore)
	width := m.innerWidth()
	out := restoreTable(width, m.cursor[screenRestore], v.Restores)
	return out + "\n\n" + sMuted.Render(clipWrap(
		"Asking for a restore from the terminal is not written yet. Until it is, the restore "+
			"forms are posted with curl as docs/guide/plain-server.md shows; what they do shows here.", width))
}

// --- settings ---

func (m Model) settingsView() string {
	v := decode[settingsPage](m, screenSettings)
	width := m.innerWidth()
	var b strings.Builder
	b.WriteString(sTitle.Render("This build") + "\n")
	b.WriteString(row("Version", v.Version) + "\n")
	switch {
	case v.Update.Installing:
		b.WriteString(row("Update", sWarn.Render("installing "+v.Update.Latest+"…")) + "\n")
	case v.Update.Error != "":
		b.WriteString(row("Update", sBad.Render(v.Update.Error)) + "\n")
	case v.Update.Newer:
		b.WriteString(row("Update", sWarn.Render(v.Update.Latest+" is published")+sMuted.Render("  press ")+sKey.Render("i")+sMuted.Render(" to install it")) + "\n")
	case v.Update.Latest != "":
		b.WriteString(row("Update", "none: "+v.Update.Latest+" is the newest") + "\n")
	}
	if v.LastChecked != "" {
		checked := v.LastChecked
		if v.CheckError != "" {
			checked += " · " + sWarn.Render(v.CheckError)
		}
		b.WriteString(row("Last checked", checked) + "\n")
	}
	b.WriteString("\n" + sTitle.Render("This server") + "\n")
	b.WriteString(row("Hostname", v.Settings.Hostname) + "\n")
	b.WriteString(row("Staging directory", fmt.Sprintf("%s  %s", v.Settings.StagingRoot, sMuted.Render(human.Bytes(v.StagingFree)+" free"))) + "\n")
	b.WriteString(row("Accounts at once", fmt.Sprint(v.Settings.MaxConcurrent)) + "\n")
	b.WriteString(row("restic", v.Settings.ResticBinary) + "\n")
	b.WriteString(row("restic cache", v.Settings.ResticCache) + "\n")
	b.WriteString(row("Configuration", v.Settings.ConfigDir) + "\n")
	if v.Settings.LogLevel != "" {
		b.WriteString(row("Log level", v.Settings.LogLevel) + "\n")
	}
	if v.Settings.UpdateChannel != "" {
		b.WriteString(row("Update channel", v.Settings.UpdateChannel) + "\n")
	}
	b.WriteString("\n" + sMuted.Render(clipWrap("Changing these from the terminal is not written yet; the settings form is posted with curl as the guide shows.", width)))
	return strings.TrimRight(b.String(), "\n")
}

// --- forms ---

func (f *form) view(width int) string {
	var b strings.Builder
	b.WriteString(sTitle.Render(f.title) + "\n\n")
	if f.err != "" {
		b.WriteString(banner("error", f.err, width) + "\n\n")
	}
	labelWidth := 20
	for i := range f.fields {
		if !f.shown(i) {
			continue
		}
		fld := &f.fields[i]
		focused := i == f.focus
		marker := "  "
		if focused {
			marker = sKey.Render("▸ ")
		}
		label := pad(fld.label, labelWidth)
		if focused {
			label = sTitle.Render(label)
		} else {
			label = sMuted.Render(label)
		}
		var control string
		switch fld.kind {
		case fieldChoice:
			parts := make([]string, len(fld.choices))
			for j, c := range fld.choices {
				if j == fld.chosen {
					parts[j] = sPill.Render(c.label)
				} else {
					parts[j] = sPillOff.Render(c.label)
				}
			}
			control = strings.Join(parts, " ")
		case fieldToggle:
			box := sMuted.Render("[ ]")
			if fld.on {
				box = sKey.Render("[x]")
			}
			control = box + " " + fld.label
			label = ""
			if focused {
				control = sTitle.Render("") + control
			}
		default:
			// One line each: a bar at the left of the field, lit when it
			// has the focus, and the text after it.
			edge := sBorder.Render("│ ")
			if focused {
				edge = sKey.Render("│ ")
			}
			control = edge + fld.input.View()
		}
		if fld.kind == fieldToggle {
			b.WriteString(marker + strings.Repeat(" ", labelWidth) + control + "\n")
		} else {
			b.WriteString(marker + label + control + "\n")
		}
		if fld.help != "" && focused {
			lead := "  " + strings.Repeat(" ", labelWidth)
			help := clipWrap(fld.help, max(width-labelWidth-4, 20))
			for _, l := range strings.Split(help, "\n") {
				b.WriteString(lead + sMuted.Render(l) + "\n")
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
