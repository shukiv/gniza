package webui_test

import (
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/node"
	"github.com/shukiv/gniza/internal/nodestore"
)

// TestAnSFTPDestinationIsEditedToLogInWithAPassword: a destination added
// with a key is switched to the account's password on its edit form. The
// password is sealed, the askpass program is written for root alone, the
// card stops showing a key nothing uses, and the switch back is allowed
// only while the key is there.
func TestAnSFTPDestinationIsEditedToLogInWithAPassword(t *testing.T) {
	client, _, engine := newUI(t)
	dir := t.TempDir()
	identity := filepath.Join(dir, "id")
	known := filepath.Join(dir, "known_hosts")
	for path, content := range map[string]string{
		identity: "-----BEGIN OPENSSH PRIVATE KEY-----\nnot really\n-----END OPENSSH PRIVATE KEY-----\n",
		known:    "[127.0.0.1]:1 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFmWXiKjAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dest, err := engine.Store().PutDestination(nodestore.Destination{
		Name: "backup", Type: "sftp",
		Config: map[string]string{
			"host": "127.0.0.1", "port": "1", "user": "shuki", "root": "/home/shuki/backups",
			"identity_file": identity, "known_hosts_file": known,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, page := get(t, client, "/destinations")
	form := func(auth, password string) url.Values {
		return url.Values{
			"csrf": {csrfToken(t, page)}, "id": {dest.ID}, "name": {"backup"}, "type": {"sftp"},
			"host": {"127.0.0.1"}, "port": {"1"}, "user": {"shuki"}, "root": {"/home/shuki/backups"},
			"auth": {auth}, "password": {password},
		}
	}

	// Nothing listens on port 1, so the save is followed by "could not be
	// reached" rather than "updated and reachable", and that is a save.
	resp, err := client.PostForm("http://ui/destinations/edit", form("password", "hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if location := resp.Header.Get("Location"); !strings.Contains(location, "could+not+be+reached") {
		t.Fatalf("the edit ended at %q", location)
	}
	edited, err := engine.Store().Destination(dest.ID)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := engine.Store().Settings()
	if err != nil {
		t.Fatal(err)
	}
	askpass := filepath.Join(settings.ConfigDir, "askpass")
	if edited.Config["auth"] != "password" || edited.Config["askpass_file"] != askpass {
		t.Errorf("edited config = %v", edited.Config)
	}
	if edited.CredentialsSecretID == "" {
		t.Error("the password was not sealed")
	}
	if info, err := os.Stat(askpass); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("askpass program: %v, %v", info, err)
	}
	_, page = get(t, client, "/destinations")
	if strings.Contains(page, "Public key for backup") {
		t.Error("the page still shows a key for a destination that logs in with a password")
	}
	if strings.Contains(page, "hunter2") {
		t.Error("the password is on the page")
	}

	// The recovery file for it pins the host key and says ssh asks for
	// the account's password; it does not hand over the key nothing uses.
	passwordID, err := node.SealRepositoryPassword(engine.Store(), engine.Vault(), "repo-secret")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := engine.Store().PutRepository(nodestore.Repository{
		DestinationID: dest.ID, Path: "cp01", PasswordSecretID: passwordID})
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.PostForm("http://ui/destinations/recovery/card",
		url.Values{"csrf": {csrfToken(t, page)}, "repository": {repo.ID}})
	if err != nil {
		t.Fatal(err)
	}
	card, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		"with that user's password", "[127.0.0.1]:1 ssh-ed25519", "PubkeyAuthentication=no",
		"shuki@127.0.0.1", "repo-secret",
	} {
		if !strings.Contains(string(card), want) {
			t.Errorf("the recovery file lacks %q", want)
		}
	}
	for _, forbidden := range []string{"not really", "hunter2", "-i "} {
		if strings.Contains(string(card), forbidden) {
			t.Errorf("the recovery file carries %q", forbidden)
		}
	}

	// Back to the key, which is still there.
	resp, err = client.PostForm("http://ui/destinations/edit", form("key", ""))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if edited, _ = engine.Store().Destination(dest.ID); edited.Config["auth"] != "" || edited.CredentialsSecretID != "" {
		t.Errorf("after the switch back: config %v, credentials %q", edited.Config, edited.CredentialsSecretID)
	}

	// A destination that was added with a password has no key to go
	// back to.
	byPassword, err := engine.Store().PutDestination(nodestore.Destination{
		Name: "vault", Type: "sftp",
		Config: map[string]string{
			"host": "127.0.0.1", "port": "1", "user": "shuki", "root": "/home/shuki/vault",
			"known_hosts_file": known, "auth": "password", "askpass_file": askpass,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	values := form("key", "")
	values.Set("id", byPassword.ID)
	values.Set("name", "vault")
	resp, err = client.PostForm("http://ui/destinations/edit", values)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if location := resp.Header.Get("Location"); !strings.Contains(location, "remove+it+and+add+it+again") {
		t.Errorf("a password destination switched to a key ended at %q", location)
	}
}
