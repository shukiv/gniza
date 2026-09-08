package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func daConf(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "directadmin.conf")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A cPanel server has no DirectAdmin plugin to serve, and opening a
// socket for one would be a second way into the operator's interface
// that nothing on that server guards.
func TestNoDirectAdminSocketOnACPanelServer(t *testing.T) {
	options, socket, err := directAdminUI(config{
		panelName:    "cpanel",
		daSocketPath: "/var/run/gniza/directadmin/plugin.sock",
		daPluginUser: "gniza-plugin",
	})
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if socket != "" || len(options) != 0 {
		t.Errorf("a cPanel server got socket %q and %d options", socket, len(options))
	}
}

// On DirectAdmin the panel's own address comes from DirectAdmin's
// configuration, because the certificate carries the name written there.
func TestTheDirectAdminSocketIsBuiltFromThePanelsOwnConfiguration(t *testing.T) {
	options, socket, err := directAdminUI(config{
		panelName:    "directadmin",
		daSocketPath: "/var/run/gniza/directadmin/plugin.sock",
		daPluginUser: "gniza-plugin",
		daConfPath:   daConf(t, "servername=panel.example.invalid\nssl=1\n"),
	})
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if socket != "/var/run/gniza/directadmin/plugin.sock" {
		t.Errorf("socket %q", socket)
	}
	if len(options) != 1 {
		t.Fatalf("%d options", len(options))
	}
}

// A server that cannot say where its own panel is cannot check a session
// against it. Starting anyway would open a socket that refuses
// everything, and the operator would read that as Gniza being broken
// rather than as this.
func TestADirectAdminServerThatCannotSayWhereItsPanelIsDoesNotStart(t *testing.T) {
	_, _, err := directAdminUI(config{
		panelName:    "directadmin",
		daSocketPath: "/var/run/gniza/directadmin/plugin.sock",
		daPluginUser: "gniza-plugin",
		daConfPath:   filepath.Join(t.TempDir(), "not-there.conf"),
	})
	if err == nil {
		t.Fatal("it started with nowhere to check a session")
	}
	if !strings.Contains(err.Error(), "not-there.conf") {
		t.Errorf("the refusal does not say what it could not read: %v", err)
	}
}

// An operator who turns the socket off gets no socket, not a socket with
// no account to belong to.
func TestTheDirectAdminSocketCanBeTurnedOff(t *testing.T) {
	for _, cfg := range []config{
		{panelName: "directadmin", daSocketPath: "", daPluginUser: "gniza-plugin"},
		{panelName: "directadmin", daSocketPath: "/var/run/gniza/directadmin/plugin.sock", daPluginUser: ""},
	} {
		options, socket, err := directAdminUI(cfg)
		if err != nil {
			t.Fatalf("refused: %v", err)
		}
		if socket != "" || len(options) != 0 {
			t.Errorf("socket %q with %d options", socket, len(options))
		}
	}
}
