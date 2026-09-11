package tui

import (
	"net/url"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

type fieldKind int

const (
	fieldText fieldKind = iota
	fieldSecret
	fieldChoice
	fieldToggle
)

// field is one input on a form. A toggle with a value is one of several
// that share a name, the way checkboxes do: every one that is on is sent.
type field struct {
	name  string
	label string
	help  string
	kind  fieldKind
	input textinput.Model
	// choices and chosen are for a choice field.
	choices []choice
	chosen  int
	// on and value are for a toggle.
	on    bool
	value string
	// when hides the field unless the form is in a state it belongs to:
	// the sftp fields when the type is sftp.
	when func(f *form) bool
}

type choice struct{ value, label string }

// form is a page of fields that posts to one path.
type form struct {
	title  string
	path   string
	fields []field
	focus  int
	err    string
	// fixed is what is sent with every submission and never shown.
	fixed url.Values
}

func textField(name, label, value, help string) field {
	input := textinput.New()
	input.Prompt = ""
	input.CharLimit = 1024
	input.Width = 48
	input.SetValue(value)
	return field{name: name, label: label, help: help, kind: fieldText, input: input}
}

func secretField(name, label, help string) field {
	f := textField(name, label, "", help)
	f.kind = fieldSecret
	f.input.EchoMode = textinput.EchoPassword
	return f
}

func choiceField(name, label string, choices []choice, chosen string) field {
	f := field{name: name, label: label, kind: fieldChoice, choices: choices}
	for i, c := range choices {
		if c.value == chosen {
			f.chosen = i
		}
	}
	return f
}

func toggleField(name, label, value string, on bool) field {
	return field{name: name, label: label, kind: fieldToggle, value: value, on: on}
}

func whenType(kind string) func(f *form) bool {
	return func(f *form) bool { return f.value("type") == kind }
}

func newForm(title, path string, fields ...field) *form {
	f := &form{title: title, path: path, fields: fields, fixed: url.Values{}}
	f.focus = -1
	f.next()
	return f
}

// value is what a field would send now, for one that sends one thing.
func (f *form) value(name string) string {
	for i := range f.fields {
		fld := &f.fields[i]
		if fld.name != name {
			continue
		}
		switch fld.kind {
		case fieldChoice:
			return fld.choices[fld.chosen].value
		case fieldToggle:
			if fld.on {
				return fld.value
			}
			return ""
		default:
			return fld.input.Value()
		}
	}
	return ""
}

func (f *form) shown(i int) bool {
	fld := &f.fields[i]
	return fld.when == nil || fld.when(f)
}

// values is the form as it would be posted.
func (f *form) values() url.Values {
	out := url.Values{}
	for name, values := range f.fixed {
		out[name] = values
	}
	for i := range f.fields {
		if !f.shown(i) {
			continue
		}
		fld := &f.fields[i]
		switch fld.kind {
		case fieldChoice:
			out.Set(fld.name, fld.choices[fld.chosen].value)
		case fieldToggle:
			if fld.on {
				out.Add(fld.name, fld.value)
			}
		default:
			out.Set(fld.name, strings.TrimSpace(fld.input.Value()))
		}
	}
	return out
}

func (f *form) next() { f.move(1) }
func (f *form) prev() { f.move(-1) }

func (f *form) move(step int) {
	if f.focus >= 0 && f.focus < len(f.fields) && f.fields[f.focus].typed() {
		f.fields[f.focus].input.Blur()
	}
	for tries := 0; tries < len(f.fields); tries++ {
		f.focus = (f.focus + step + len(f.fields)) % len(f.fields)
		if f.shown(f.focus) {
			break
		}
	}
	if f.fields[f.focus].typed() {
		// The cursor's blink command is dropped: a steady cursor is
		// fine, and a form that schedules nothing is one a test can
		// drive to the end.
		_ = f.fields[f.focus].input.Focus()
	}
}

// typed says the field is one that is typed into, and so has an input.
func (fld *field) typed() bool { return fld.kind == fieldText || fld.kind == fieldSecret }

// last says whether the focus is on the last shown field, where enter
// submits rather than moving on.
func (f *form) last() bool {
	for i := len(f.fields) - 1; i >= 0; i-- {
		if f.shown(i) {
			return i == f.focus
		}
	}
	return true
}

// update handles a key on the form. It reports whether the form asks to
// be submitted.
func (f *form) update(msg tea.KeyMsg) (submit bool) {
	fld := &f.fields[f.focus]
	switch msg.String() {
	case "ctrl+s":
		return true
	case "tab", "down":
		f.next()
		return false
	case "shift+tab", "up":
		f.prev()
		return false
	case "enter":
		if f.last() {
			return true
		}
		f.next()
		return false
	}
	switch fld.kind {
	case fieldChoice:
		switch msg.String() {
		case " ", "right":
			fld.chosen = (fld.chosen + 1) % len(fld.choices)
		case "left":
			fld.chosen = (fld.chosen + len(fld.choices) - 1) % len(fld.choices)
		}
		return false
	case fieldToggle:
		if msg.String() == " " {
			fld.on = !fld.on
		}
		return false
	}
	fld.input, _ = fld.input.Update(msg)
	return false
}
