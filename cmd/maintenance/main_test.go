package main

import (
	"context"
	"strings"
	"testing"
)

// A drill reads a snapshot back and checks it against the layout of the
// panel that wrote it. Maintenance runs away from the servers it serves,
// so it cannot ask a provider which panel that was: it has to be told,
// and being told the wrong one means a drill that fails on a DirectAdmin
// snapshot for no reason an operator can act on.
func TestTheDrillLayoutFollowsThePanelItIsToldAbout(t *testing.T) {
	for _, tc := range []struct {
		panel string
		want  string
	}{
		{panel: "", want: "cpanel"},
		{panel: "cpanel", want: "cpanel"},
		{panel: "directadmin", want: "directadmin"},
	} {
		layout, err := layoutFor(tc.panel)
		if err != nil {
			t.Fatalf("layoutFor(%q): %v", tc.panel, err)
		}
		if got := layout.Panel(); got != tc.want {
			t.Errorf("layoutFor(%q) is the %s layout, want %s", tc.panel, got, tc.want)
		}
	}
}

// A panel nobody wrote a layout for is refused by name, rather than
// quietly checked against cPanel's.
func TestAPanelWithNoLayoutIsRefusedByName(t *testing.T) {
	if _, err := layoutFor("plesk"); err == nil {
		t.Fatal("an unknown panel was accepted")
	} else if !strings.Contains(err.Error(), "plesk") {
		t.Errorf("the error does not name the panel: %v", err)
	}
}

// And it is refused before anything is opened, so an operator who
// mistypes it is told so instead of being told the database is
// unreachable.
func TestAnUnknownPanelIsRefusedBeforeTheDatabase(t *testing.T) {
	err := run(context.Background(), runConfig{kind: "check", panelName: "plesk"})
	if err == nil {
		t.Fatal("an unknown panel was accepted")
	}
	if !strings.Contains(err.Error(), "plesk") {
		t.Errorf("the run failed for some other reason first: %v", err)
	}
}
