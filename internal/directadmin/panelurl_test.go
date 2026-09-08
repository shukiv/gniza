package directadmin

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConf(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "directadmin.conf")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The panel's own address is the name its certificate carries, which is
// the name DirectAdmin was told to call itself. An address that reaches
// the same machine is not the same thing: the certificate is checked.
func TestThePanelIsReachedAtTheNameItCallsItself(t *testing.T) {
	for _, test := range []struct {
		name string
		conf string
		want string
	}{
		{
			// What a real 1.709 host has: a servername, ssl on, and no
			// port line at all.
			name: "a real host",
			conf: "servername=uscp.linux-hosting.network\nssl=1\n",
			want: "https://uscp.linux-hosting.network:2222",
		},
		{
			name: "a port of its own",
			conf: "servername=panel.example.invalid\nssl=1\nport=2223\n",
			want: "https://panel.example.invalid:2223",
		},
		{
			// The panel itself is unencrypted, so the session already
			// travels in clear to reach it. Speaking https to a panel
			// that is not listening for it reaches nothing at all.
			name: "no ssl",
			conf: "servername=panel.example.invalid\nssl=0\n",
			want: "http://panel.example.invalid:2222",
		},
		{
			name: "spaces and comments",
			conf: "# the panel\n servername = panel.example.invalid \n\nssl=1\n",
			want: "https://panel.example.invalid:2222",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := PanelURL(writeConf(t, test.conf))
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if got != test.want {
				t.Errorf("read %q, wanted %q", got, test.want)
			}
		})
	}
}

// A configuration that does not say where the panel is cannot be guessed
// at. Guessing would mean sending a live session credential somewhere
// DirectAdmin never said it was.
func TestAConfigurationThatNamesNoPanelIsRefused(t *testing.T) {
	for _, test := range []struct{ name, conf string }{
		{"nothing", ""},
		{"no servername", "ssl=1\nport=2222\n"},
		{"an empty servername", "servername=\nssl=1\n"},
		{"a servername that is not a name", "servername=panel example/../etc\nssl=1\n"},
		{"a servername with a port stuck to it", "servername=panel.example.invalid:2222\n"},
		{"a port that is not one", "servername=panel.example.invalid\nport=two\n"},
		{"a port out of range", "servername=panel.example.invalid\nport=70000\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, err := PanelURL(writeConf(t, test.conf)); err == nil {
				t.Errorf("accepted, as %q", got)
			}
		})
	}
}

func TestAMissingConfigurationIsRefused(t *testing.T) {
	if _, err := PanelURL(filepath.Join(t.TempDir(), "not-there.conf")); err == nil {
		t.Error("a configuration that is not there was read")
	}
}
