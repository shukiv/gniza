package tui

import (
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Run opens the interface on a socket and holds the terminal until the
// operator leaves it.
func Run(socket string) error {
	client, err := Dial(socket)
	if err != nil {
		return err
	}
	// Asked before the program owns the terminal: lipgloss queries the
	// background colour once, and a query while bubbletea reads the
	// keys is answered to the wrong reader.
	_ = lipgloss.HasDarkBackground()
	program := tea.NewProgram(New(client), tea.WithAltScreen())
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("terminal interface: %w", err)
	}
	return nil
}

// RunWithIO is Run for a test or a script: keys read from in, nothing
// drawn. It returns the last screen when the input ends or the operator
// quits.
func RunWithIO(socket string, in io.Reader) (string, error) {
	client, err := Dial(socket)
	if err != nil {
		return "", err
	}
	program := tea.NewProgram(New(client), tea.WithInput(in), tea.WithoutRenderer())
	final, err := program.Run()
	if err != nil {
		return "", err
	}
	return final.View(), nil
}
