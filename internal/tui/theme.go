package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The look of the terminal interface.
//
// One palette, adaptive: every colour has a light-terminal and a
// dark-terminal value, because an operator's terminal is whichever they
// have, and a green that reads on black is invisible on white. Slate
// neutrals, the status green as the one accent, and three semantic
// colours that mean the same thing everywhere: green is fine, amber
// wants a look, red wants an act. Nothing else is coloured.
var (
	cAccent   = lipgloss.AdaptiveColor{Light: "#15803D", Dark: "#4ADE80"}
	cOnAccent = lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#052E16"}
	cMuted    = lipgloss.AdaptiveColor{Light: "#64748B", Dark: "#94A3B8"}
	cBorder   = lipgloss.AdaptiveColor{Light: "#CBD5E1", Dark: "#475569"}
	cOK       = cAccent
	cWarn     = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FBBF24"}
	cBad      = lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#F87171"}
	cSelected = lipgloss.AdaptiveColor{Light: "#DCFCE7", Dark: "#14532D"}
)

var (
	sBrand    = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sTitle    = lipgloss.NewStyle().Bold(true)
	sMuted    = lipgloss.NewStyle().Foreground(cMuted)
	sOK       = lipgloss.NewStyle().Foreground(cOK)
	sWarn     = lipgloss.NewStyle().Foreground(cWarn)
	sBad      = lipgloss.NewStyle().Foreground(cBad)
	sBadBold  = lipgloss.NewStyle().Foreground(cBad).Bold(true)
	sKey      = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sTabOn    = lipgloss.NewStyle().Bold(true).Foreground(cOnAccent).Background(cAccent).Padding(0, 1)
	sTabOff   = lipgloss.NewStyle().Foreground(cMuted).Padding(0, 1)
	sPill     = lipgloss.NewStyle().Bold(true).Foreground(cOnAccent).Background(cAccent).Padding(0, 1)
	sPillOff  = lipgloss.NewStyle().Foreground(cMuted).Padding(0, 1)
	sSelected = lipgloss.NewStyle().Bold(true).Background(cSelected)
	sHeader   = lipgloss.NewStyle().Bold(true).Foreground(cMuted)
	sBorder   = lipgloss.NewStyle().Foreground(cBorder)
	sPanel    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cBorder).Padding(0, 1)
	sCard     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cBorder).Padding(0, 1)
	sCardOn   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cAccent).Padding(0, 1)
	sAlert    = lipgloss.NewStyle().Border(lipgloss.ThickBorder()).BorderForeground(cWarn).Padding(0, 1)
	sBig      = lipgloss.NewStyle().Bold(true)
)

// tone is the style a status word is drawn in, so the same word is the
// same colour on every screen, and a name that happens to be a word is
// left alone by matching whole cells only.
func tone(cell string) (lipgloss.Style, bool) {
	switch strings.TrimSpace(cell) {
	case "ok", "success", "saved", "protected", "on", "yes":
		return sOK, true
	case "failed", "NOT SAVED", "never", "error", "off", "cancelled", "bad":
		return sBad, true
	case "pending", "running", "stale", "out-of-date", "partial", "copy-gap", "unscheduled",
		"working", "warn", "partial_success", "waiting":
		return sWarn, true
	}
	return lipgloss.Style{}, false
}

func toneFor(kind string) lipgloss.Style {
	switch kind {
	case "ok":
		return sOK
	case "warn":
		return sWarn
	case "error", "bad":
		return sBad
	}
	return sMuted
}

// chip is a key and what it does: "a add".
func chip(key, label string) string {
	return sKey.Render(key) + " " + sMuted.Render(label)
}

// chips joins several, spaced so they read as a row of buttons.
func chips(pairs ...string) string {
	out := make([]string, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, chip(pairs[i], pairs[i+1]))
	}
	return strings.Join(out, "   ")
}

// banner is one line that must be noticed: a stripe in the severity
// colour, then the words, wrapped inside the width with the stripe on
// every line.
func banner(kind, text string, width int) string {
	style := toneFor(kind)
	body := lipgloss.NewStyle().Width(max(width-2, 10)).Render(text)
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = style.Render("▌ ") + line
	}
	return strings.Join(lines, "\n")
}

// stat is one number with its name under it, in a card.
func stat(width int, label, value, note string, highlight bool) string {
	style := sCard
	if highlight {
		style = sCardOn
	}
	// Width is the whole card, borders included: the box is width-2
	// wide and its text, inside the padding, width-4.
	box := max(width-2, 10)
	text := box - 2
	body := sBig.Render(clip(value, text)) + "\n" + sMuted.Render(clip(label, text))
	if note != "" {
		body += "\n" + clip(note, text)
	} else {
		body += "\n"
	}
	return style.Width(box).Render(body)
}

// cards lays stat cards side by side when there is room, else one under
// another.
func cards(width int, items ...string) string {
	if len(items) == 0 {
		return ""
	}
	each := width / len(items)
	if each < 18 {
		return lipgloss.JoinVertical(lipgloss.Left, items...)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, items...)
}

// bar draws progress in the width given.
func bar(percent float64, width int) string {
	if width < 4 {
		return ""
	}
	filled := int(percent / 100 * float64(width))
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	return sOK.Render(strings.Repeat("█", filled)) + sBorder.Render(strings.Repeat("░", width-filled))
}

// clip cuts a string to a width with an ellipsis, by display cells.
func clip(text string, width int) string {
	text = strings.ReplaceAll(text, "\n", " ")
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(text) <= width {
		return text
	}
	runes := []rune(text)
	for len(runes) > 0 && lipgloss.Width(string(runes))+1 > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

// pad cuts or fills a cell to a width.
func pad(text string, width int) string {
	text = clip(text, width)
	return text + strings.Repeat(" ", max(0, width-lipgloss.Width(text)))
}

// row is a label and its value, the label in the muted column every
// screen shares.
func row(label, value string) string {
	return fmt.Sprintf("%s %s", sMuted.Render(pad(label, 18)), value)
}

// empty is what a screen with nothing on it says, with what to do.
func empty(width int, headline, key, action string) string {
	out := sTitle.Render(headline)
	if key != "" {
		out += "\n\n" + sMuted.Render("Press ") + sKey.Render(key) + sMuted.Render(" to "+action+".")
	}
	return lipgloss.NewStyle().Width(max(width, 20)).Render(out)
}
