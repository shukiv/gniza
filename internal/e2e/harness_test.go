//go:build e2e

package e2e_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/agent"
	"github.com/shukiv/gniza/internal/certs"
	"github.com/shukiv/gniza/internal/controller"
	"github.com/shukiv/gniza/internal/cpanel"
	"github.com/shukiv/gniza/internal/layout/cpmove"
	"github.com/shukiv/gniza/internal/maintenance"
	"github.com/shukiv/gniza/internal/repobuild"
	"github.com/shukiv/gniza/internal/resticrun"
	"github.com/shukiv/gniza/internal/staging"
	"github.com/shukiv/gniza/internal/store"
	"github.com/shukiv/gniza/internal/testsupport"
	"github.com/shukiv/gniza/internal/vault"
)

// harness is a complete Gniza deployment inside one test: a real
// PostgreSQL, a real controller over mutual TLS, a real restic, a real
// rest-server in append-only mode, and an agent driving a synthetic cPanel
// account.
type harness struct {
	ctx  context.Context
	db   *store.Store
	v    *vault.Vault
	auth *certs.Authority

	restic       string
	resticRunner *resticrun.Runner
	maintenance  *maintenance.Runner

	agentClient           *agent.Client
	unauthenticatedClient *agent.Client
	worker                *agent.Agent
	provider              *cpanel.Fake

	serverID  string
	accountID string
	policyID  string

	localRepoID string
	restRepoID  string
	localRoot   string
	restRoot    string
	caPath      string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	resticPath := testsupport.RequireBinary(t, "restic")
	restServerPath := testsupport.RequireBinary(t, "rest-server")

	ctx := context.Background()
	workDir := t.TempDir()

	db, err := store.Open(ctx, testsupport.PostgresDSN(t))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	keyHex, err := vault.GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	masterKey, err := decodeHex(keyHex)
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.New(masterKey)
	if err != nil {
		t.Fatalf("open vault: %v", err)
	}

	authority, err := certs.NewAuthority("gniza test CA", time.Hour)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	caPath := filepath.Join(workDir, "ca.pem")
	if err := os.WriteFile(caPath, authority.Pair.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	h := &harness{
		ctx: ctx, db: db, v: v, auth: authority,
		restic: resticPath, caPath: caPath,
		localRoot: filepath.Join(workDir, "local-destination"),
		restRoot:  filepath.Join(workDir, "rest-destination"),
	}
	for _, dir := range []string{h.localRoot, h.restRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// Two endpoints over one data directory, which is how an append-only
	// destination has to be deployed: rest-server's --append-only is a
	// property of the process, not of a credential, so a second
	// delete-capable endpoint is the only way retention can ever run.
	agentURL := h.startRestServer(t, restServerPath, true)
	maintenanceURL := h.startRestServer(t, restServerPath, false)
	h.startController(t, workDir)
	h.buildAgent(t, workDir)
	h.seed(t, agentURL, maintenanceURL)
	return h
}

// startRestServer runs rest-server over TLS signed by the test CA, which is
// what a destination we control looks like.
//
// appendOnly selects the endpoint agents reach; the delete-capable one is
// what the maintenance runner reaches over a management network.
func (h *harness) startRestServer(t *testing.T, binary string, appendOnly bool) string {
	t.Helper()

	port := freePort(t)
	pair, err := h.auth.IssueServer("rest-server", []string{"127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatalf("issue rest-server certificate: %v", err)
	}
	certPath := filepath.Join(t.TempDir(), "rest.pem")
	keyPath := filepath.Join(filepath.Dir(certPath), "rest-key.pem")
	if err := pair.WriteFiles(certPath, keyPath); err != nil {
		t.Fatal(err)
	}

	args := []string{
		"--path", h.restRoot,
		"--listen", fmt.Sprintf("127.0.0.1:%d", port),
		"--tls", "--tls-cert", certPath, "--tls-key", keyPath,
		// Authentication is exercised by the controller's mTLS; here it
		// would only add a bcrypt dependency to the test.
		"--no-auth",
	}
	if appendOnly {
		args = append(args, "--append-only")
	}
	cmd := exec.Command(binary, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start rest-server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	address := fmt.Sprintf("127.0.0.1:%d", port)
	if err := testsupport.WaitFor(h.ctx, 10*time.Second, func() error {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			return err
		}
		return conn.Close()
	}); err != nil {
		t.Fatalf("rest-server did not start: %v", err)
	}
	return "https://" + address
}

// startController serves the agent API over mutual TLS and points an agent
// client at it.
func (h *harness) startController(t *testing.T, workDir string) {
	t.Helper()

	service := controller.New(h.db, h.v, testLogger(t))
	service.LeaseDuration = time.Minute
	api := controller.NewAPI(service, testLogger(t))
	// The suite drives jobs explicitly; a long hold would just add
	// wall-clock time.
	api.LongPollWait = 2 * time.Second
	api.PollBackoff = 100 * time.Millisecond

	serverPair, err := h.auth.IssueServer("controller", []string{"127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatalf("issue controller certificate: %v", err)
	}
	serverCert, err := tls.X509KeyPair(serverPair.CertPEM, serverPair.KeyPEM)
	if err != nil {
		t.Fatalf("load controller certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(h.auth.Pair.CertPEM)

	httpServer := httptest.NewUnstartedServer(api.Handler())
	httpServer.TLS = controller.ServerTLSConfig(serverCert, pool)
	httpServer.StartTLS()
	t.Cleanup(httpServer.Close)

	agentPair, err := h.auth.IssueClient("cp01.example.com", time.Hour)
	if err != nil {
		t.Fatalf("issue agent certificate: %v", err)
	}
	agentCert, err := tls.X509KeyPair(agentPair.CertPEM, agentPair.KeyPEM)
	if err != nil {
		t.Fatalf("load agent certificate: %v", err)
	}
	fingerprint, err := certs.FingerprintPEM(agentPair.CertPEM)
	if err != nil {
		t.Fatal(err)
	}

	serverID, err := h.db.CreateServer(h.ctx, "cp01.example.com", fingerprint)
	if err != nil {
		t.Fatalf("register server: %v", err)
	}
	h.serverID = serverID

	h.agentClient = agent.NewClientWithHTTP(httpServer.URL, &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{agentCert},
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS13,
		}},
	})
	h.unauthenticatedClient = agent.NewClientWithHTTP(httpServer.URL, &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: pool, MinVersion: tls.VersionTLS13,
		}},
	})
	_ = workDir
}

// buildAgent assembles the worker with a synthetic cPanel provider, so the
// whole pipeline runs on a machine that has never seen cPanel.
func (h *harness) buildAgent(t *testing.T, workDir string) {
	t.Helper()

	h.provider = &cpanel.Fake{
		Root:      filepath.Join(workDir, "cpanel"),
		Databases: map[string][]string{"customer1": {"customer1_wp", "customer1_shop"}},
		FileCount: 8,
		FileSize:  8192,
	}
	stagingRoot := filepath.Join(workDir, "staging")
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	h.resticRunner = resticrun.New(resticrun.Config{
		Binary:     h.restic,
		RuntimeDir: workDir,
		CacheDir:   filepath.Join(workDir, "restic-cache"),
		CACertPath: h.caPath,
	}, nil)

	h.worker = agent.New(agent.Config{
		Client:   h.agentClient,
		Provider: h.provider,
		Staging: &staging.Manager{
			Root: stagingRoot, SafetyMarginRatio: 0.1, MaxConcurrent: 2,
		},
		Runner:        h.resticRunner,
		Log:           testLogger(t),
		Hostname:      "cp01.example.com",
		ResticVersion: "test",
		// restic retries an unreachable backend for minutes; the suite
		// only needs to see that the target fails and the others do not.
		TargetTimeout: 30 * time.Second,
	})
	h.maintenance = maintenance.New(h.db, h.v, h.resticRunner, testLogger(t), cpmove.Layout{})
}

// seed creates the destinations, repositories, policy and account.
func (h *harness) seed(t *testing.T, agentURL, maintenanceURL string) {
	t.Helper()

	localDest := h.createDestination(t, "Local NAS", "local",
		map[string]any{"root": h.localRoot}, nil, false)
	restDest := h.createDestination(t, "Append-only rest-server", "rest",
		map[string]any{
			"base_url":             agentURL,
			"maintenance_base_url": maintenanceURL,
			"append_only":          true,
			"ca_bundle":            h.caPath,
		},
		map[string]string{"username": "cp01", "password": "unused-no-auth"}, true)

	h.localRepoID = h.createRepository(t, localDest, "cp01")
	h.restRepoID = h.createRepository(t, restDest, "cp01")

	policyID, err := h.db.CreatePolicy(h.ctx, store.Policy{
		Name: "nightly", ScheduleCron: "0 2 * * *", PayloadMode: "split",
		Compression: "auto",
		Retention:   store.Retention{KeepLast: 1},
	})
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	h.policyID = policyID
	for _, repoID := range []string{h.localRepoID, h.restRepoID} {
		if err := h.db.AttachRepositoryToPolicy(h.ctx, policyID, repoID); err != nil {
			t.Fatalf("attach repository: %v", err)
		}
	}

	accountID, err := h.db.CreateAccount(h.ctx, store.Account{
		ServerID: h.serverID, CPanelUser: "customer1",
		PrimaryDomain: "customer1.example", SizeEstimate: 1 << 20,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	h.accountID = accountID
	if err := h.db.AttachPolicyToAccount(h.ctx, accountID, policyID); err != nil {
		t.Fatalf("attach policy: %v", err)
	}
}

func (h *harness) createDestination(t *testing.T, name, destType string,
	config map[string]any, secrets map[string]string, appendOnly bool) string {
	t.Helper()

	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var secretID string
	if len(secrets) > 0 {
		sealed, err := repobuild.SealCredentials(h.v, secrets)
		if err != nil {
			t.Fatal(err)
		}
		if secretID, err = h.db.CreateSecret(h.ctx, store.SecretBackendCredentials,
			sealed, h.v.KeyID()); err != nil {
			t.Fatal(err)
		}
	}
	id, err := h.db.CreateDestination(h.ctx, store.Destination{
		Name: name, Type: destType, Config: encoded,
		CredentialsSecretID: secretID, AppendOnly: appendOnly,
	})
	if err != nil {
		t.Fatalf("create destination %s: %v", name, err)
	}
	return id
}

func (h *harness) createRepository(t *testing.T, destinationID, path string) string {
	t.Helper()

	password, err := vault.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := h.v.SealString(password)
	if err != nil {
		t.Fatal(err)
	}
	secretID, err := h.db.CreateSecret(h.ctx, store.SecretRepositoryPassword, sealed, h.v.KeyID())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := h.db.CreateRepository(h.ctx, store.Repository{
		DestinationID: destinationID, ServerID: h.serverID,
		Path: path, PasswordSecretID: secretID,
	})
	if err != nil {
		t.Fatalf("create repository: %v", err)
	}
	return repo.ID
}

// resticIn runs restic directly against a repository, for assertions the
// product code does not expose.
func (h *harness) resticIn(t *testing.T, repositoryID string, args ...string) []byte {
	t.Helper()

	sealed, err := h.db.SealedRepository(h.ctx, repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := repobuild.Open(h.v, repobuild.Sealed{
		DestinationType:    sealed.DestinationType,
		DestinationConfig:  sealed.DestinationConfig,
		CredentialsSealed:  sealed.CredentialsSealed,
		RepoPath:           sealed.RepositoryPath,
		RepoPasswordSealed: sealed.RepoPasswordSealed,
	})
	if err != nil {
		t.Fatal(err)
	}
	dest, err := buildDestination(opened)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := dest.URI(opened.Path)
	if err != nil {
		t.Fatal(err)
	}
	env, err := dest.Env()
	if err != nil {
		t.Fatal(err)
	}

	full := append([]string{"--cacert", h.caPath}, args...)
	cmd := exec.CommandContext(h.ctx, h.restic, full...)
	cmd.Env = append(os.Environ(),
		"RESTIC_REPOSITORY="+uri,
		"RESTIC_PASSWORD="+opened.Password,
	)
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("restic %v failed: %v\n%s", args, err, output)
	}
	return output
}

// forgetAsAgent runs retention through the endpoint an agent reaches, which
// is how a compromised cPanel server would try to destroy history.
func (h *harness) forgetAsAgent(t *testing.T, repositoryID string, spec resticrun.ForgetSpec) error {
	t.Helper()

	sealed, err := h.db.SealedRepository(h.ctx, repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	// repobuild.Open, not OpenForMaintenance: no maintenance endpoint.
	opened, err := repobuild.Open(h.v, repobuild.Sealed{
		DestinationType:    sealed.DestinationType,
		DestinationConfig:  sealed.DestinationConfig,
		CredentialsSealed:  sealed.CredentialsSealed,
		RepoPath:           sealed.RepositoryPath,
		RepoPasswordSealed: sealed.RepoPasswordSealed,
	})
	if err != nil {
		t.Fatal(err)
	}
	dest, err := buildDestination(opened)
	if err != nil {
		t.Fatal(err)
	}
	return h.resticRunner.Forget(h.ctx, resticrun.Repository{
		Dest: dest, Path: opened.Path, Password: opened.Password,
	}, spec)
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func decodeHex(s string) ([]byte, error) {
	out := make([]byte, len(s)/2)
	for i := range out {
		var value int
		if _, err := fmt.Sscanf(s[2*i:2*i+2], "%02x", &value); err != nil {
			return nil, fmt.Errorf("decode hex: %w", err)
		}
		out[i] = byte(value)
	}
	return out, nil
}
