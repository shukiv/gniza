package destination

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLocalPreflight(t *testing.T) {
	root := t.TempDir()
	if err := (&Local{Root: root}).Preflight(context.Background()); err != nil {
		t.Errorf("Preflight on a real directory: %v", err)
	}
	if err := (&Local{Root: filepath.Join(root, "missing")}).Preflight(context.Background()); err == nil {
		t.Error("missing root should be rejected")
	}

	file := filepath.Join(root, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&Local{Root: file}).Preflight(context.Background()); err == nil {
		t.Error("root that is a file should be rejected")
	}
	if err := (&Local{Root: "relative"}).Preflight(context.Background()); err == nil {
		t.Error("relative root should be rejected")
	}
}

func TestRESTPreflight(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		switch user {
		case "good":
			// rest-server answers an unknown path with 404 once
			// authenticated; that is a healthy endpoint, not a failure.
			w.WriteHeader(http.StatusNotFound)
		case "broken":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer server.Close()

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test server
	}}

	base := server.URL
	cases := []struct {
		name    string
		dest    *REST
		wantErr string
	}{
		{
			name: "authenticated endpoint passes",
			dest: &REST{BaseURL: base, Username: "good", Password: "p", HTTPClient: client},
		},
		{
			name:    "rejected credentials fail",
			dest:    &REST{BaseURL: base, Username: "bad", Password: "p", HTTPClient: client},
			wantErr: "authentication rejected",
		},
		{
			name:    "server error fails",
			dest:    &REST{BaseURL: base, Username: "broken", Password: "p", HTTPClient: client},
			wantErr: "server error",
		},
		{
			// Basic auth over plaintext would hand the repository
			// credentials to anyone on the path.
			name:    "plaintext http is refused",
			dest:    &REST{BaseURL: "http://backup.example.com", Username: "good", HTTPClient: client},
			wantErr: "must use https",
		},
		{
			name:    "missing username is refused",
			dest:    &REST{BaseURL: base, HTTPClient: client},
			wantErr: "username is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.dest.Preflight(context.Background())
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Preflight: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestSFTPPreflightRequiresPinnedHostKey(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(key, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}

	dest := &SFTP{Host: "127.0.0.1", User: "u", Root: "/b", IdentityFile: key}
	err := dest.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "known-hosts") {
		t.Fatalf("err = %v, want a known-hosts complaint", err)
	}
}

func TestSFTPPreflightRejectsLooseKeyPermissions(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "id_ed25519")
	known := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(key, []byte("key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(known, []byte("host key"), 0o644); err != nil {
		t.Fatal(err)
	}

	dest := &SFTP{
		Host: "127.0.0.1", User: "u", Root: "/b",
		IdentityFile: key, KnownHostsFile: known,
	}
	err := dest.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "world-accessible") {
		t.Fatalf("err = %v, want a permissions complaint", err)
	}
}

// The port answering is not proof the key is accepted: Preflight logs
// in the way restic will, and says in words what ssh complained of.
func TestSFTPPreflightLogsInAndExplainsARefusedKey(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "id_ed25519")
	known := filepath.Join(dir, "known_hosts")
	for _, file := range []string{key, known} {
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	fake := func(script string) string {
		path := filepath.Join(dir, "ssh-"+strconv.Itoa(len(script)))
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	dest := &SFTP{
		Host: "127.0.0.1", Port: port, User: "backup", Root: "/b",
		IdentityFile: key, KnownHostsFile: known,
	}

	dest.SSHPath = fake("echo \"$@\" > " + filepath.Join(dir, "args") + "; exit 0\n")
	if err := dest.Preflight(context.Background()); err != nil {
		t.Fatalf("a server that accepts the key: %v", err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	for _, want := range []string{"-p " + strconv.Itoa(port), "-l backup", "-i " + key, "BatchMode=yes", "127.0.0.1 -s sftp"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("ssh was run as %q, without %q", strings.TrimSpace(string(args)), want)
		}
	}

	dest.SSHPath = fake("echo 'backup@127.0.0.1: Permission denied (publickey,password).' >&2; exit 255\n")
	err = dest.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not accept Gniza's key for backup") || !strings.Contains(err.Error(), "authorized_keys") {
		t.Errorf("a refused key: %v", err)
	}

	dest.SSHPath = fake("echo 'Host key verification failed.' >&2; exit 255\n")
	err = dest.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "host key is not the one pinned") {
		t.Errorf("a changed host key: %v", err)
	}

	dest.SSHPath = fake("echo 'ssh: connect to host 127.0.0.1 port 22: Connection refused' >&2; exit 255\n")
	err = dest.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ssh to backup@127.0.0.1: ssh: connect") {
		t.Errorf("another complaint: %v", err)
	}
}

func TestS3PreflightRejectsPlaintextEndpoint(t *testing.T) {
	dest := &S3{
		Endpoint: "http://minio.internal:9000", Bucket: "b",
		AccessKeyID: "AK", SecretAccessKey: "SK",
	}
	err := dest.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("err = %v, want an https complaint", err)
	}
}

func TestSFTPOptionsPinHostKeyAndIdentity(t *testing.T) {
	dest := &SFTP{
		Host: "backup.example.com", User: "cpbackup", Root: "/backup",
		IdentityFile:   "/etc/gniza/id_ed25519",
		KnownHostsFile: "/etc/gniza/known_hosts",
	}
	options, err := dest.Options()
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	args := options["sftp.args"]
	for _, want := range []string{
		"-i /etc/gniza/id_ed25519",
		"UserKnownHostsFile=/etc/gniza/known_hosts",
		"StrictHostKeyChecking=yes",
		// An unattended agent must never be able to sit at a prompt.
		"BatchMode=yes",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("sftp.args = %q, missing %q", args, want)
		}
	}

	// Without a pinned host key the agent would trust whatever answers.
	unpinned := &SFTP{Host: "h", User: "u", Root: "/b", IdentityFile: "/k"}
	if _, err := unpinned.Options(); err == nil {
		t.Error("missing known-hosts file should be rejected")
	}
}

func TestSFTPRejectsPathsShellSplittingWouldMangle(t *testing.T) {
	dest := &SFTP{
		Host: "h", User: "u", Root: "/b",
		IdentityFile:   "/etc/gniza/my key",
		KnownHostsFile: "/etc/gniza/known_hosts",
	}
	if _, err := dest.Options(); err == nil {
		t.Error("an identity path containing a space should be rejected")
	}
}

func TestOptionsAreEmptyForSimpleBackends(t *testing.T) {
	backends := map[string]Destination{
		"local": &Local{Root: "/srv/b"},
		"rest":  &REST{BaseURL: "https://b", Username: "u"},
		"s3":    &S3{Bucket: "b", Region: "r"},
	}
	for name, dest := range backends {
		options, err := dest.Options()
		if err != nil {
			t.Errorf("%s Options: %v", name, err)
			continue
		}
		if len(options) != 0 {
			t.Errorf("%s Options = %v, want none", name, options)
		}
	}
}

// TestPlaintextIsRefusedWhereTheURLIsBuilt covers the way the https rule
// was got around without meaning to: a destination is saved before it is
// tested, and can be edited afterwards. The check lived only in Preflight,
// so an endpoint edited to http was never tested again and was used —
// sending this server's credentials, and every account on it, in the
// clear.
func TestPlaintextIsRefusedWhereTheURLIsBuilt(t *testing.T) {
	rest := &REST{BaseURL: "http://backup.example.com"}
	if _, err := rest.URI("repo"); err == nil {
		t.Error("a REST destination built a plaintext URI")
	} else if !strings.Contains(err.Error(), "https") {
		t.Errorf("err = %v, want an https complaint", err)
	}

	bucket := &S3{Endpoint: "http://minio.internal", Bucket: "backups"}
	if _, err := bucket.URI("repo"); err == nil {
		t.Error("an S3 destination built a plaintext URI")
	} else if !strings.Contains(err.Error(), "https") {
		t.Errorf("err = %v, want an https complaint", err)
	}

	// And the ones that are fine still are.
	secure := &REST{BaseURL: "https://backup.example.com"}
	if _, err := secure.URI("repo"); err != nil {
		t.Errorf("an https destination was refused: %v", err)
	}
}

// TestAppendOnlyIsCheckedRatherThanBelieved covers a promise the page made
// that nothing had verified. "The server runs with --append-only" was an
// operator's checkbox, and the page told them ransomware on this machine
// could not destroy their history. Nobody had asked the server.
func TestAppendOnlyIsCheckedRatherThanBelieved(t *testing.T) {
	var deleted []string
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = append(deleted, r.URL.Path)
			// What rest-server --append-only answers.
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer refusing.Close()

	accepting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = append(deleted, r.URL.Path)
			// What a read-write rest-server answers for an object that
			// is not there.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer accepting.Close()

	// httptest is http, and https is enforced where the URL is built, so
	// the check is driven directly.
	for _, probe := range []struct {
		name   string
		server *httptest.Server
		wantOK bool
	}{
		{"a server that refuses deletes", refusing, true},
		{"a server that does not", accepting, false},
	} {
		dest := &REST{
			BaseURL: probe.server.URL, Username: "gniza", Password: "x",
			AppendOnly: true, HTTPClient: probe.server.Client(),
		}
		base, err := url.Parse(probe.server.URL)
		if err != nil {
			t.Fatal(err)
		}
		err = dest.checkAppendOnly(context.Background(), base, probe.server.Client())
		if probe.wantOK && err != nil {
			t.Errorf("%s was reported as not append-only: %v", probe.name, err)
		}
		if !probe.wantOK && err == nil {
			t.Errorf("%s passed an append-only check", probe.name)
		}
	}

	// The probe must never name an object that could exist.
	for _, path := range deleted {
		if !strings.HasSuffix(path, strings.Repeat("0", 64)) {
			t.Errorf("the check aimed a delete at %q, which could be a real object", path)
		}
	}
}
