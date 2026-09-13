package node

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shukiv/gniza/internal/destination"
	"github.com/shukiv/gniza/internal/nodestore"
	"github.com/shukiv/gniza/internal/sshkeys"
)

// SFTPRequest is what an operator fills in to back up to another Linux
// server.
//
// There is no field for a key: Gniza generates its own, one per
// destination, so revoking access to one destination does not lock it out
// of the others.
type SFTPRequest struct {
	Name           string
	Host           string
	Port           int
	User           string
	RemoteDir      string
	RepositoryPath string
	// Auth is how the backups log in: "password" keeps the account's
	// password, sealed in the vault, and every backup logs in with it;
	// anything else is a key Gniza makes, the default.
	Auth string
	// Password is the account's password on the far side. With Auth
	// "password" it is the login and is kept. Otherwise, when given, it
	// is used once to install the generated public key on the far side
	// and is then discarded, never stored.
	Password string
	// ExistingKeyPath uses a key the operator already has instead of
	// generating one.
	ExistingKeyPath string
	// AdminUser and AdminPassword, when given, are an account on the
	// remote server that can create the backup account: usually root.
	// Gniza makes the user, its home, the backup directory and the
	// authorized_keys entry, then locks the account's password so the key
	// is the only way in. The password is used for that one connection
	// and is never stored.
	AdminUser     string
	AdminPassword string
	// ConfirmedFingerprint is the remote server's host key fingerprint as
	// the operator has agreed to it. Empty means they have not been shown
	// one yet, and nothing is sent to that server until they have: the
	// whole point of a fingerprint is that somebody looked at it.
	ConfirmedFingerprint string
}

// UnconfirmedHostError is what AddSFTPDestination returns when it has
// learnt a server's identity and nobody has agreed to it yet.
//
// Trust on first use is what an operator does when they type "yes" at
// ssh's prompt, and it is a real decision: the fingerprint is the only
// thing standing between a backup destination and whoever answered on
// that address. Storing it silently and then sending a password made
// that decision for them, invisibly, every time.
type UnconfirmedHostError struct {
	Host        string
	Fingerprint string
	KeyType     string
}

func (e *UnconfirmedHostError) Error() string {
	return fmt.Sprintf("%s identifies itself with a %s key, fingerprint %s. "+
		"Check it against that server before going on: on it, run "+
		"ssh-keygen -lf /etc/ssh/ssh_host_%s_key.pub",
		e.Host, e.KeyType, e.Fingerprint, strings.TrimPrefix(e.KeyType, "ssh-"))
}

// SFTPResult reports what was set up, including the public key an operator
// must install if Gniza could not do it for them.
type SFTPResult struct {
	Destination     nodestore.Destination
	Repository      nodestore.Repository
	AuthorizedKey   string
	KeyFingerprint  string
	HostFingerprint string
	HostKeyType     string
	// Installed is true when the public key was placed on the remote
	// server, Verified when logging in with it then worked, and Created
	// when Gniza made the account it logs in to.
	Installed bool
	Verified  bool
	Created   bool
	// Warning explains what still needs doing by hand.
	Warning string
}

const sshTimeout = 20 * time.Second

// detailOf appends a remote command's output to an error, when it said
// anything worth reading.
func detailOf(output string) string {
	if output == "" {
		return ""
	}
	return ": " + strings.ReplaceAll(output, "\n", " | ")
}

// PreparedKey is a key Gniza made before any destination exists, so its
// public half can be installed on the far side first.
//
// A backup server is often not one this cPanel server has a password for:
// somebody else administers it, or the password is on a card in a drawer.
// The way that works is to hand them a public key. Until now the key was
// made while the destination was being created, so there was nothing to
// hand over until after the thing that needed it had been set up.
type PreparedKey struct {
	// Path is where the private half lives on this server. It goes into
	// the form as the key to use, so saving the destination uses the same
	// key whose public half was copied out of the page.
	Path          string
	AuthorizedKey string
	Fingerprint   string
}

// PrepareSFTPKey makes a key for a destination that does not exist yet.
//
// The private half never leaves this server. What comes back is the line
// for the far side's authorized_keys, and where the private half was put,
// so the destination that is saved next uses it rather than a second key
// nobody has installed.
func (e *Engine) PrepareSFTPKey() (PreparedKey, error) {
	pair, err := sshkeys.Generate("gniza@" + e.settings.Hostname)
	if err != nil {
		return PreparedKey{}, err
	}
	// "prepared-" so a key nobody went on to use can be told apart from
	// one a destination depends on, and swept.
	path, err := sshkeys.WritePrivateKey(
		filepath.Join(e.settings.ConfigDir, "keys"), "prepared-"+nodestore.NewID(), pair)
	if err != nil {
		return PreparedKey{}, err
	}
	e.log.Info("prepared an SFTP key", "path", path, "fingerprint", pair.Fingerprint)
	return PreparedKey{
		Path: path, AuthorizedKey: pair.AuthorizedKey, Fingerprint: pair.Fingerprint,
	}, nil
}

// PreparedKeyAt describes a key an earlier click made, so a form that
// comes back -- refused, or waiting for a host key to be agreed to --
// still shows the public half somebody may already be installing on the
// far side. Without it the page would offer to make a second key and
// quietly stop pointing at the first.
//
// The path arrives from a form, so only a prepared key in Gniza's own key
// directory is described: nothing else on this server is a file this reads.
// The private half is not returned either way.
func (e *Engine) PreparedKeyAt(path string) (PreparedKey, bool) {
	if path == "" {
		return PreparedKey{}, false
	}
	clean := filepath.Clean(path)
	if filepath.Dir(clean) != filepath.Join(e.settings.ConfigDir, "keys") ||
		!strings.HasPrefix(filepath.Base(clean), "prepared-") {
		return PreparedKey{}, false
	}
	pair, err := sshkeys.PublicHalf(clean)
	if err != nil {
		return PreparedKey{}, false
	}
	return PreparedKey{
		Path: clean, AuthorizedKey: pair.AuthorizedKey, Fingerprint: pair.Fingerprint,
	}, true
}

// preparedKeyLife is how long a key nobody used stays. Long enough to add
// it to a server somebody else administers and come back tomorrow.
const preparedKeyLife = 7 * 24 * time.Hour

// sweepPreparedKeys removes prepared keys no destination went on to use.
//
// A key made and abandoned is a private key on disk that opens an account
// somewhere, kept for no reason. One still named by a destination is left
// alone however old it is.
func (e *Engine) sweepPreparedKeys(now time.Time) {
	if !e.lastKeySweep.IsZero() && now.Sub(e.lastKeySweep) < time.Hour {
		return
	}
	e.lastKeySweep = now

	dir := filepath.Join(e.settings.ConfigDir, "keys")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	destinations, err := e.store.Destinations()
	if err != nil {
		e.log.Error("read destinations before sweeping prepared keys", "error", err)
		return
	}
	inUse := map[string]bool{}
	for _, dest := range destinations {
		if path := dest.Config["identity_file"]; path != "" {
			inUse[path] = true
		}
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "prepared-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if inUse[path] {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < preparedKeyLife {
			continue
		}
		if err := os.Remove(path); err != nil {
			e.log.Error("remove an unused prepared key", "path", path, "error", err)
			continue
		}
		e.log.Info("removed a prepared key nobody used", "path", path)
	}
}

// AddSFTPDestination sets up a backup destination on another Linux server.
//
// It generates a key, learns the server's host key, and — given the remote
// password once — installs the key and proves it works. What an operator
// used to do with ssh-keygen, ssh-copy-id and a hand-written known_hosts.
func (e *Engine) AddSFTPDestination(req SFTPRequest) (SFTPResult, error) {
	if err := validateSFTP(&req); err != nil {
		return SFTPResult{}, err
	}

	address := net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	settings := e.settings
	destinationID := nodestore.NewID()

	// Anything written before the destination is stored is litter if this
	// fails, and an operator retrying a typo should not accumulate keys.
	var litter []string
	defer func() {
		for _, path := range litter {
			_ = os.Remove(path)
		}
	}()

	// 1. The server's identity, read before we send it anything. This
	//    needs no credentials: the host proves itself first.
	hostKey, err := sshkeys.FetchHostKey(address, sshTimeout)
	if err != nil {
		return SFTPResult{}, fmt.Errorf(
			"node: could not reach %s over SSH: %w", address, err)
	}
	// Nothing has been sent to that server yet, and nothing is until the
	// operator has seen who answered.
	switch {
	case req.ConfirmedFingerprint == "":
		return SFTPResult{}, &UnconfirmedHostError{
			Host: req.Host, Fingerprint: hostKey.Fingerprint, KeyType: hostKey.Type,
		}
	case req.ConfirmedFingerprint != hostKey.Fingerprint:
		return SFTPResult{}, fmt.Errorf(
			"node: %s answered with a different host key than the one you agreed to. "+
				"You agreed to %s and it presented %s. Either that server was rebuilt, or "+
				"something else is answering on its address -- do not go on until you know "+
				"which", req.Host, req.ConfirmedFingerprint, hostKey.Fingerprint)
	}

	knownHostsPath := filepath.Join(settings.ConfigDir, "known_hosts", destinationID)
	if err := sshkeys.WriteKnownHosts(knownHostsPath, hostKey); err != nil {
		return SFTPResult{}, err
	}
	litter = append(litter, knownHostsPath)

	result := SFTPResult{
		HostFingerprint: hostKey.Fingerprint,
		HostKeyType:     hostKey.Type,
	}

	if req.Auth == sftpAuthPassword {
		result, err := e.addSFTPByPassword(req, address, hostKey, knownHostsPath, destinationID, result)
		if err == nil {
			litter = nil
		}
		return result, err
	}

	// 2. Our key for this destination.
	identityPath := req.ExistingKeyPath
	var privatePEM []byte
	if identityPath == "" {
		pair, err := sshkeys.Generate("gniza@" + settings.Hostname)
		if err != nil {
			return SFTPResult{}, err
		}
		identityPath, err = sshkeys.WritePrivateKey(
			filepath.Join(settings.ConfigDir, "keys"), destinationID, pair)
		if err != nil {
			return SFTPResult{}, err
		}
		privatePEM = pair.PrivatePEM
		result.AuthorizedKey = pair.AuthorizedKey
		result.KeyFingerprint = pair.Fingerprint
		litter = append(litter, identityPath)
	}

	// 3. Make the account, if we were given something that can.
	if req.AdminPassword != "" && privatePEM != nil {
		output, err := sshkeys.RunAsAdmin(address, req.AdminUser, req.AdminPassword, hostKey,
			sshkeys.ProvisionScript(req.User, req.RemoteDir, result.AuthorizedKey), sshTimeout)
		if err != nil {
			return SFTPResult{}, fmt.Errorf(
				"node: could not set up %s on %s as %s: %w%s",
				req.User, req.Host, req.AdminUser, err, detailOf(output))
		}
		result.Created = true
		result.Installed = true

		if err := sshkeys.VerifyKeyLogin(address, req.User, privatePEM, hostKey, sshTimeout); err != nil {
			return SFTPResult{}, fmt.Errorf(
				"node: %s was created on %s but logging in as it with the new key failed: %w. "+
					"Check that sshd on that server allows this account to log in with a key",
				req.User, req.Host, err)
		}
		result.Verified = true
	}

	// 4. Install the key, if we were trusted with the account's password
	//    and did not just create the account ourselves.
	switch {
	case result.Verified:
		// Already done, above.
	case req.Password != "" && privatePEM != nil:
		if err := sshkeys.InstallAuthorizedKey(address, req.User, req.Password,
			hostKey, result.AuthorizedKey, sshTimeout); err != nil {
			return SFTPResult{}, err
		}
		result.Installed = true

		if err := sshkeys.VerifyKeyLogin(address, req.User, privatePEM, hostKey, sshTimeout); err != nil {
			// sshd refuses a key silently when ~/.ssh or authorized_keys
			// are writable by anyone but their owner, so say what is
			// actually there rather than leaving the operator guessing.
			detail := sshkeys.Diagnose(address, req.User, req.Password, hostKey, sshTimeout)
			if detail != "" {
				return SFTPResult{}, fmt.Errorf(
					"node: the key was added to %s@%s but logging in with it failed: %w. "+
						"sshd ignores authorized_keys when the home directory or ~/.ssh is "+
						"writable by others. On that server: %s",
					req.User, req.Host, err, strings.ReplaceAll(detail, "\n", " | "))
			}
			return SFTPResult{}, fmt.Errorf(
				"node: the key was installed but logging in with it failed: %w", err)
		}
		result.Verified = true

		if err := sshkeys.EnsureRemoteDir(address, req.User, privatePEM,
			hostKey, req.RemoteDir, sshTimeout); err != nil {
			result.Warning = fmt.Sprintf(
				"Logged in, but could not create %s: %v. Create it yourself and test again.",
				req.RemoteDir, err)
		}
	case privatePEM != nil:
		result.Warning = "Add the public key below to " + req.User +
			"@" + req.Host + ":~/.ssh/authorized_keys, then use Test."
	}

	// 5. Store it. The private key stays a file on disk with the
	//    permissions ssh insists on; only its path is recorded.
	dest := nodestore.Destination{
		ID:   destinationID,
		Name: req.Name,
		Type: string(destination.TypeSFTP),
		Config: map[string]string{
			"host":             req.Host,
			"user":             req.User,
			"root":             req.RemoteDir,
			"identity_file":    identityPath,
			"known_hosts_file": knownHostsPath,
		},
	}
	if req.Port != 22 {
		dest.Config["port"] = strconv.Itoa(req.Port)
	}

	spec := destination.Spec{
		Type: destination.TypeSFTP, Config: dest.Config, Secrets: map[string]string{},
	}
	if _, err := destination.Build(spec); err != nil {
		return SFTPResult{}, err
	}

	stored, err := e.store.PutDestination(dest)
	if err != nil {
		return SFTPResult{}, err
	}
	// The destination owns these files now.
	litter = nil
	result.Destination = stored

	repository, err := e.newRepository(stored.ID, req.RepositoryPath)
	if err != nil {
		return SFTPResult{}, err
	}
	result.Repository = repository
	return result, nil
}

// sftpAuthPassword is the value of SFTPRequest.Auth and of a destination's
// "auth" configuration key when the backups log in with the account's
// password.
const sftpAuthPassword = "password"

// addSFTPByPassword finishes a destination whose login is the account's
// password: proves the password opens a login, makes the directory,
// seals the password in the vault and stores the destination with the
// program that hands ssh the password on every run.
func (e *Engine) addSFTPByPassword(req SFTPRequest, address string, hostKey sshkeys.HostKey,
	knownHostsPath, destinationID string, result SFTPResult) (SFTPResult, error) {
	if err := sshkeys.VerifyPasswordLogin(address, req.User, req.Password, hostKey, sshTimeout); err != nil {
		return SFTPResult{}, fmt.Errorf(
			"node: %s does not accept the password for %s: %w", req.Host, req.User, err)
	}
	result.Verified = true
	if err := sshkeys.EnsureRemoteDirWithPassword(address, req.User, req.Password,
		hostKey, req.RemoteDir, sshTimeout); err != nil {
		result.Warning = fmt.Sprintf(
			"Logged in, but could not create %s: %v. Create it yourself and test again.",
			req.RemoteDir, err)
	}
	askPass, err := e.EnsureAskPass()
	if err != nil {
		return SFTPResult{}, err
	}
	secrets := map[string]string{"password": req.Password}
	dest := nodestore.Destination{
		ID:   destinationID,
		Name: req.Name,
		Type: string(destination.TypeSFTP),
		Config: map[string]string{
			"host":             req.Host,
			"user":             req.User,
			"root":             req.RemoteDir,
			"known_hosts_file": knownHostsPath,
			"auth":             sftpAuthPassword,
			"askpass_file":     askPass,
		},
	}
	if req.Port != 22 {
		dest.Config["port"] = strconv.Itoa(req.Port)
	}
	if _, err := destination.Build(destination.Spec{
		Type: destination.TypeSFTP, Config: dest.Config, Secrets: secrets}); err != nil {
		return SFTPResult{}, err
	}
	secretID, err := SealCredentials(e.store, e.vault, secrets)
	if err != nil {
		return SFTPResult{}, err
	}
	dest.CredentialsSecretID = secretID
	stored, err := e.store.PutDestination(dest)
	if err != nil {
		return SFTPResult{}, err
	}
	result.Destination = stored
	repository, err := e.newRepository(stored.ID, req.RepositoryPath)
	if err != nil {
		return SFTPResult{}, err
	}
	result.Repository = repository
	return result, nil
}

// EnsureSFTPDirWithPassword creates a destination's directory on the far
// side with the account's password, for a destination edited to log in
// with one: adding a destination makes the directory, and an edit that
// changes the login should leave it in the same state. The host key is
// the one pinned when the destination was added.
func (e *Engine) EnsureSFTPDirWithPassword(dest nodestore.Destination, password string) error {
	hosts, err := os.ReadFile(dest.Config["known_hosts_file"])
	if err != nil {
		return fmt.Errorf("node: the host key pinned for %s: %w", dest.Name, err)
	}
	var hostKey sshkeys.HostKey
	for _, line := range strings.Split(string(hosts), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			hostKey.Line = line
			break
		}
	}
	if hostKey.Line == "" {
		return fmt.Errorf("node: no host key is pinned for %s", dest.Name)
	}
	port := dest.Config["port"]
	if port == "" {
		port = "22"
	}
	address := net.JoinHostPort(dest.Config["host"], port)
	return sshkeys.EnsureRemoteDirWithPassword(address, dest.Config["user"], password,
		hostKey, dest.Config["root"], sshTimeout)
}

// askPassProgram is what ssh runs for a password when a destination logs
// in with one. ssh hands it the prompt as an argument and reads the
// answer from its output; the password is in the environment restic was
// given, never on a command line.
const askPassProgram = `#!/bin/sh
# Written by Gniza. ssh runs this instead of asking a terminal for the
# password of a backup destination; the password is in the environment
# the backup was started with.
printf '%s\n' "$GNIZA_SSH_PASSWORD"
`

// EnsureAskPass writes the askpass program under the configuration
// directory, readable and runnable by root alone, and returns its path.
// One program serves every destination that logs in with a password.
func (e *Engine) EnsureAskPass() (string, error) {
	path := filepath.Join(e.settings.ConfigDir, "askpass")
	if err := os.MkdirAll(e.settings.ConfigDir, 0o700); err != nil {
		return "", fmt.Errorf("node: askpass program: %w", err)
	}
	if current, err := os.ReadFile(path); err == nil && string(current) == askPassProgram {
		if info, err := os.Stat(path); err == nil && info.Mode().Perm() == 0o700 {
			return path, nil
		}
	}
	if err := os.WriteFile(path, []byte(askPassProgram), 0o700); err != nil {
		return "", fmt.Errorf("node: askpass program: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return "", fmt.Errorf("node: askpass program: %w", err)
	}
	return path, nil
}

// PublicKeyFor returns the public key Gniza generated for a destination, so
// the interface can show it again later.
func (e *Engine) PublicKeyFor(dest nodestore.Destination) string {
	// A destination edited from a key to a password keeps the key file
	// but logs in without it; showing the key would send an operator to
	// install something nothing uses.
	if dest.Config["auth"] == sftpAuthPassword {
		return ""
	}
	identity := dest.Config["identity_file"]
	if identity == "" {
		return ""
	}
	pair, err := sshkeys.PublicKeyFromFile(identity)
	if err != nil {
		e.log.Error("read public key", "destination", dest.Name, "error", err)
		return ""
	}
	return pair
}

func validateSFTP(req *SFTPRequest) error {
	req.Name = strings.TrimSpace(req.Name)
	req.Host = strings.TrimSpace(req.Host)
	req.User = strings.TrimSpace(req.User)
	req.RemoteDir = strings.TrimSpace(req.RemoteDir)
	req.AdminUser = strings.TrimSpace(req.AdminUser)
	if req.AdminPassword != "" && req.AdminUser == "" {
		req.AdminUser = "root"
	}

	switch {
	case req.Name == "":
		return fmt.Errorf("node: give the destination a name")
	case req.Host == "":
		return fmt.Errorf("node: the remote host is required")
	case req.User == "":
		return fmt.Errorf("node: the SSH user is required")
	case !strings.HasPrefix(req.RemoteDir, "/"):
		return fmt.Errorf("node: the remote directory must be an absolute path")
	case req.AdminPassword != "" && req.AdminUser == req.User:
		// Creating an account requires an account that already exists.
		return fmt.Errorf(
			"node: %s is the account to create, so it cannot also be the one that creates it. "+
				"Give an account that can already log in, usually root", req.User)
	case req.AdminPassword != "" && req.Password != "":
		return fmt.Errorf(
			"node: give either the backup account's own password or an administrator's, " +
				"not both: the first installs a key on an account that exists, the second " +
				"creates the account")
	case req.Auth == sftpAuthPassword && req.Password == "":
		return fmt.Errorf("node: the password is required to log in with one")
	case req.Auth == sftpAuthPassword && req.AdminPassword != "":
		return fmt.Errorf(
			"node: an administrator's password creates an account that logs in with a key; " +
				"to log in with a password, give that account's own")
	case req.Auth == sftpAuthPassword && req.ExistingKeyPath != "":
		return fmt.Errorf("node: a destination that logs in with a password has no key")
	}
	if req.Port == 0 {
		req.Port = 22
	}
	if req.Port < 1 || req.Port > 65535 {
		return fmt.Errorf("node: port %d is out of range", req.Port)
	}
	return nil
}
