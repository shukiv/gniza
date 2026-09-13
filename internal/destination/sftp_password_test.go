package destination

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSFTPLogsInWithAPasswordThroughAskpass: a destination whose login is
// the account's password hands ssh no key, tells it to ask for a password
// once and to ask the askpass program rather than a terminal, and puts the
// password in the environment rather than in an argument.
func TestSFTPLogsInWithAPasswordThroughAskpass(t *testing.T) {
	dir := t.TempDir()
	known := filepath.Join(dir, "known_hosts")
	askpass := filepath.Join(dir, "askpass")
	if err := os.WriteFile(known, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\nprintf '%s\\n' \"$GNIZA_SSH_PASSWORD\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	dest := &SFTP{
		Host: "127.0.0.1", User: "shuki", Root: "/home/shuki/backups",
		KnownHostsFile: known, Password: "hunter2", AskPass: askpass,
	}

	options, err := dest.Options()
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	args := options["sftp.args"]
	for _, want := range []string{
		"UserKnownHostsFile=" + known, "StrictHostKeyChecking=yes",
		"PubkeyAuthentication=no", "PreferredAuthentications=password,keyboard-interactive",
		"NumberOfPasswordPrompts=1",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("sftp.args = %q, missing %q", args, want)
		}
	}
	for _, forbidden := range []string{"-i ", "BatchMode", "IdentitiesOnly", "hunter2"} {
		if strings.Contains(args, forbidden) {
			t.Errorf("sftp.args = %q, carries %q", args, forbidden)
		}
	}

	env, err := dest.Env()
	if err != nil {
		t.Fatalf("Env: %v", err)
	}
	for key, want := range map[string]string{
		"SSH_ASKPASS": askpass, "SSH_ASKPASS_REQUIRE": "force", "DISPLAY": "gniza",
		"GNIZA_SSH_PASSWORD": "hunter2",
	} {
		if env[key] != want {
			t.Errorf("env %s = %q, want %q", key, env[key], want)
		}
	}

	// A key destination's environment is empty, as it was.
	byKey := &SFTP{Host: "h", User: "u", Root: "/b", IdentityFile: "/k", KnownHostsFile: "/kh"}
	if env, err := byKey.Env(); err != nil || len(env) != 0 {
		t.Errorf("a key destination's env = %v, %v", env, err)
	}

	// Preflight runs ssh with that environment and no terminal, and
	// explains a refused password as one, not as a refused key.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	dest.Port = listener.Addr().(*net.TCPAddr).Port
	fake := func(script string) string {
		path := filepath.Join(dir, "ssh-"+strconv.Itoa(len(script)))
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	seen := filepath.Join(dir, "seen")
	dest.SSHPath = fake("echo \"$@\" > " + seen + "; env >> " + seen + "; exit 0\n")
	if err := dest.Preflight(context.Background()); err != nil {
		t.Fatalf("a server that accepts the password: %v", err)
	}
	got, _ := os.ReadFile(seen)
	for _, want := range []string{"-l shuki", "PubkeyAuthentication=no", "127.0.0.1 -s sftp",
		"SSH_ASKPASS=" + askpass, "GNIZA_SSH_PASSWORD=hunter2", "SSH_ASKPASS_REQUIRE=force"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("ssh ran without %q:\n%s", want, got)
		}
	}
	if strings.Contains(string(got), "-i ") {
		t.Errorf("ssh was handed a key:\n%s", got)
	}

	dest.SSHPath = fake("echo 'Permission denied, please try again.' >&2; echo 'shuki@127.0.0.1: Permission denied (publickey,password).' >&2; exit 255\n")
	err = dest.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not accept the password for shuki") || strings.Contains(err.Error(), "authorized_keys") {
		t.Errorf("a refused password: %v", err)
	}

	// An askpass program others could replace would hand them the
	// password.
	if err := os.Chmod(askpass, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := dest.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "askpass program") {
		t.Errorf("a world-readable askpass program: %v", err)
	}
}

// TestAnSFTPSpecSaysHowItLogsIn: the configuration key auth=password
// builds the password login from the sealed password and the askpass
// path, needs no key, and a spec without it builds the key login as
// before.
func TestAnSFTPSpecSaysHowItLogsIn(t *testing.T) {
	built, err := Build(Spec{Type: TypeSFTP,
		Config:  map[string]string{"host": "h", "user": "u", "root": "/b", "known_hosts_file": "/kh", "auth": "password", "askpass_file": "/etc/gniza/askpass"},
		Secrets: map[string]string{"password": "pw"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	sftp := built.(*SFTP)
	if sftp.Password != "pw" || sftp.AskPass != "/etc/gniza/askpass" || sftp.IdentityFile != "" {
		t.Errorf("built %+v", sftp)
	}

	_, err = Build(Spec{Type: TypeSFTP,
		Config: map[string]string{"host": "h", "user": "u", "root": "/b", "known_hosts_file": "/kh", "auth": "password", "askpass_file": "/a"}})
	if err == nil || !strings.Contains(err.Error(), `secret "password" is required`) {
		t.Errorf("a password login with no password: %v", err)
	}

	_, err = Build(Spec{Type: TypeSFTP,
		Config: map[string]string{"host": "h", "user": "u", "root": "/b", "known_hosts_file": "/kh"}})
	if err == nil || !strings.Contains(err.Error(), `config "identity_file" is required`) {
		t.Errorf("a key login with no key: %v", err)
	}
}
