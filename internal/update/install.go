package update

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// releaseKeyPEM is the public half of the key every release is signed
// with. It is compiled in rather than read from disk because a key a
// program reads from a file it could also be handed a different one for,
// and this key is the whole reason downloading a release and running it as
// root is not simply a way of handing this server to whoever can answer
// for github.com.
//
// The same key is written out in packaging/whm/get.sh, which has no
// binary to embed it in. A test here checks the two have not drifted.
//
//go:embed release.pub
var releaseKeyPEM []byte

// The three files a release publishes. The tarball is the plugin; the
// sums are what says which tarball; the signature is what says the sums
// came from whoever holds the release key.
//
// The tarball still carries the name this program had before it was called
// Gniza, and so does the word this file's manifest is read by and the
// directory inside the tarball. Every server already running an older
// release has those three spellings compiled into it and asks for exactly
// them: a release that renamed the asset could be downloaded by nobody who
// has not already upgraded, which is the one set of servers an upgrade has
// to reach. They are names on a published artefact rather than anything a
// person reads, and they cost nothing to leave alone.
const (
	TarballName = "cprest-plugin-amd64.tar.gz"
	SumsName    = "SHA256SUMS"
	SigName     = "SHA256SUMS.sig"
)

// Package is what one panel takes out of a release: the asset published
// for it, and the single directory that asset unpacks to.
//
// A release publishes one per panel and signs both in the same file. A
// server installs the one for the panel it runs: the other one is a
// plugin for a panel that is not there, and installing it would leave the
// panel that is there without one.
type Package struct {
	Asset  string
	TopDir string
}

// The published packages. cPanel's keeps the pre-Gniza spelling for the
// reason above; DirectAdmin's was named after the rename and says what it
// is.
var (
	WHMPackage         = Package{Asset: TarballName, TopDir: TopDir}
	DirectAdminPackage = Package{Asset: "gniza-directadmin-amd64.tar.gz", TopDir: "gniza-directadmin"}
	// PlainPackage is for a server with no panel at all: a LAMP or
	// container host, run from the terminal (ADR 0022).
	PlainPackage = Package{Asset: "gniza-plain-amd64.tar.gz", TopDir: "gniza-plain"}
)

// PackageFor is the release package for a panel, by the name the panel's
// provider goes by.
//
// A panel with no package is not one this server can install from here.
// That is a refusal rather than a guess: the two packages install
// different files in different places, and the wrong one run as root
// would put a plugin on a machine with nothing to show it.
func PackageFor(panelName string) (Package, error) {
	switch panelName {
	case "cPanel":
		return WHMPackage, nil
	case "DirectAdmin":
		return DirectAdminPackage, nil
	case "Plain server":
		return PlainPackage, nil
	}
	return Package{}, fmt.Errorf("update: a release publishes no package for %s", panelName)
}

// What is accepted off the network. A release tarball is around 16 MB and
// the other two are a few hundred bytes; these are room to grow, not
// estimates. Nothing here is streamed into anything that would run before
// the whole of it has been checked.
const (
	maxTarball = 128 << 20
	maxText    = 64 << 10
)

// Channel is where a server takes its updates from.
//
// Releases are the default and the safer one: a version number, notes, and
// a tag somebody decided to make. The dist branch is for a server that
// follows the work rather than the releases -- what is published there is
// signed with the same key, so it is not less checked, only less
// deliberate.
type Channel string

const (
	ChannelReleases Channel = "releases"
	ChannelDist     Channel = "dist"
)

// DistBranch is the branch a dist build is published on.
const DistBranch = "dist"

// Manifest is what a published build says about itself, out of the file
// the release key signed.
type Manifest struct {
	// Version is a release like v1.2.3 on the releases channel, and
	// whatever git describe said on the dist branch.
	Version string
	// BuiltAt is the commit the build was made from. Empty on releases
	// published before this existed.
	BuiltAt time.Time
}

// Source is where releases are fetched from.
type Source struct {
	// Client is left nil in production, where a client with a timeout
	// long enough for a 16 MB download is made here.
	Client *http.Client
	// Base is the directory releases live under, without the tag. It is
	// GitHub unless GNIZA_RELEASE_BASE says otherwise, which exists so
	// this can be exercised against a server on the machine itself: the
	// signature is checked against the compiled-in key either way, so an
	// address that points somewhere else can still only deliver what the
	// release key signed.
	Base string
	// Key is the public key signatures are checked against. Nil means the
	// release key, which is what production uses.
	Key *ecdsa.PublicKey
	// Flat says the files sit directly under Base rather than under a
	// directory named after the version. A git branch has one copy of
	// each file and no version in its path; a releases page has one
	// directory per tag.
	Flat bool
}

// DistSource reads whatever build is currently published on the dist
// branch. It is the raw file service rather than the API: three files, no
// token, and the same signature check as a release.
func DistSource(repo string) Source {
	if repo == "" {
		repo = Repo
	}
	base := strings.TrimSpace(os.Getenv("GNIZA_DIST_BASE"))
	if base == "" {
		base = "https://raw.githubusercontent.com/" + repo + "/" + DistBranch
	}
	return Source{Base: base, Flat: true}
}

// SourceFor is where a channel reads from.
func SourceFor(channel Channel, repo string) Source {
	if channel == ChannelDist {
		return DistSource(repo)
	}
	return DefaultSource(repo)
}

// DefaultSource reads releases from GitHub, or from wherever
// GNIZA_RELEASE_BASE says.
func DefaultSource(repo string) Source {
	if repo == "" {
		repo = Repo
	}
	base := strings.TrimSpace(os.Getenv("GNIZA_RELEASE_BASE"))
	if base == "" {
		base = "https://github.com/" + repo + "/releases/download"
	}
	return Source{Base: base}
}

// ReleaseKey is the key releases are signed with.
func ReleaseKey() (*ecdsa.PublicKey, error) { return parseKey(releaseKeyPEM) }

func parseKey(body []byte) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, errors.New("update: the release key is not PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("update: read the release key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("update: the release key is a %T, not an ECDSA key", parsed)
	}
	return key, nil
}

// Fetch downloads a release into dir and returns the path of the tarball,
// having checked that the release key signed the checksums and that the
// checksums describe what arrived.
//
// Nothing is unpacked here and nothing is run. What comes back is a file
// this server has decided it can believe.
func (s Source) Fetch(ctx context.Context, version, dir string, want Package) (string, error) {
	if !s.Flat && !IsRelease(version) {
		return "", fmt.Errorf("update: %q is not a release version", version)
	}
	sums, manifest, err := s.signedSums(ctx, version)
	if err != nil {
		return "", err
	}
	// What was agreed to is what gets installed. On a branch the files
	// move under the operator's feet -- a build published between the
	// page being read and the button being pressed is a different program
	// from the one they were shown.
	if manifest.Version != version {
		return "", fmt.Errorf("update: what is published is %s, not the %s that was asked for",
			describe(manifest.Version), version)
	}
	sum, err := sumFor(sums, want.Asset)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	tarball := filepath.Join(dir, want.Asset)
	got, err := s.download(ctx, version, want.Asset, tarball)
	if err != nil {
		return "", err
	}
	if got != sum {
		_ = os.Remove(tarball)
		return "", fmt.Errorf("update: %s does not match the signed checksum", want.Asset)
	}
	return tarball, nil
}

// Published is what this source is offering now: the version, and the
// commit it was built from.
//
// Nothing is downloaded but the manifest, and the manifest is not believed
// until the release key has signed it.
func (s Source) Published(ctx context.Context, version string) (Manifest, error) {
	_, manifest, err := s.signedSums(ctx, version)
	return manifest, err
}

// signedSums reads the checksums and proves they were published by
// whoever holds the release key.
//
// version names the directory to read from on a releases page, and is
// what the manifest is checked against there. A branch has one copy of
// each file and no version in its path, so it is read with an empty
// version and the manifest says what it is.
func (s Source) signedSums(ctx context.Context, version string) ([]byte, Manifest, error) {
	key := s.Key
	if key == nil {
		release, err := ReleaseKey()
		if err != nil {
			return nil, Manifest{}, err
		}
		key = release
	}
	sums, err := s.get(ctx, version, SumsName, maxText)
	if err != nil {
		return nil, Manifest{}, err
	}
	signature, err := s.get(ctx, version, SigName, maxText)
	if err != nil {
		return nil, Manifest{}, err
	}
	digest := sha256.Sum256(sums)
	if !ecdsa.VerifyASN1(key, digest[:], signature) {
		// Everything after this point is a file this server runs as root,
		// so this is the end of the road rather than a warning.
		return nil, Manifest{}, fmt.Errorf(
			"update: what is published %s is not signed by the Gniza release key", at(version))
	}
	manifest := manifestIn(sums)
	if manifest.Version == "" {
		return nil, Manifest{}, fmt.Errorf(
			"update: the signed checksums %s do not say which build they are for", at(version))
	}
	// The signature says these checksums were published by whoever holds
	// the release key. The version inside them says which build they were
	// published for. Without that second check, anybody who can make a
	// tag or push a branch -- which is not the same as holding the key --
	// could publish an old signed build again under a new name, and this
	// server would install it believing it had gone forward.
	if !s.Flat && manifest.Version != version {
		return nil, Manifest{}, fmt.Errorf(
			"update: those checksums are signed for %s, not %s",
			describe(manifest.Version), version)
	}
	return sums, manifest, nil
}

// at names where something was published, for an error that has to say so
// whether or not there was a version in the path.
func at(version string) string {
	if version == "" {
		return "on the dist branch"
	}
	return "for " + version
}

// IsRelease says whether a version is a published release rather than a
// build of somebody's own. Only these are ever fetched, so a version
// number can never put anything of its own in a URL, and nothing with
// space around it is one: this is a whole answer on its own, not a check
// that happens to be followed by another.
func IsRelease(version string) bool {
	_, ok := parse(version)
	return ok && strings.HasPrefix(version, "v") && version == strings.TrimSpace(version)
}

// manifestIn reads what a set of checksums says about itself, which the
// build writes as its first line. The word is "cprest" and not "gniza" for
// the reason given at TarballName: an older agent reads this line and
// accepts no other spelling.
//
// The line is "# cprest v1.2.3" for a release, and
// "# cprest v1.2.3-18-gabc1234 2026-09-06T10:11:12Z" for a branch build,
// where the commit time is the only thing that puts two of them in order.
func manifestIn(sums []byte) Manifest {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "#" || fields[1] != "cprest" {
			continue
		}
		manifest := Manifest{Version: fields[2]}
		if len(fields) >= 4 {
			if at, err := time.Parse(time.RFC3339, fields[3]); err == nil {
				manifest.BuiltAt = at.UTC()
			}
		}
		return manifest
	}
	return Manifest{}
}

// describe names what a set of checksums says it is, for the one error
// that has to distinguish "signed for another release" from "signed for
// nothing in particular".
func describe(version string) string {
	if version == "" {
		return "no particular release"
	}
	return version
}

// get reads one small file of a release into memory.
func (s Source) get(ctx context.Context, version, name string, limit int64) ([]byte, error) {
	response, err := s.open(ctx, version, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	return io.ReadAll(io.LimitReader(response.Body, limit))
}

// download writes one file of a release to disk and returns its checksum,
// which is taken as it is written rather than by reading the file back:
// what is checked is then what was received.
func (s Source) download(ctx context.Context, version, name, path string) (string, error) {
	response, err := s.open(ctx, version, name)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, sum), io.LimitReader(response.Body, maxTarball+1))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if written > maxTarball {
		_ = os.Remove(path)
		return "", fmt.Errorf("update: %s is larger than %d bytes", name, int64(maxTarball))
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func (s Source) open(ctx context.Context, version, name string) (*http.Response, error) {
	base := s.Base
	if base == "" {
		base = DefaultSource("").Base
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("update: %s is not an address releases can be read from: %w", base, err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, fmt.Errorf("update: releases are read over http or https, not %q", parsed.Scheme)
	}
	segments := []string{version, name}
	if s.Flat || version == "" {
		// A branch holds one copy of each file, and its path says
		// nothing about which build that is. The manifest does.
		segments = []string{name}
	}
	address, err := url.JoinPath(base, segments...)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "gniza")

	response, err := s.client().Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, fmt.Errorf("update: %s of %s: %s", name, version, response.Status)
	}
	return response, nil
}

// maxHops is how many redirects a release may be behind. GitHub answers a
// release download with one, to wherever it keeps the file.
const maxHops = 5

// client is the client to fetch with, with this program's own rule about
// where a redirect may lead.
//
// A release download is redirected -- GitHub sends the request on to the
// storage that holds the file -- so redirects are followed. What is not
// followed is a redirect off https onto a plain connection: nothing
// unsigned can be installed either way, but a download that quietly
// stopped being private or tamper-evident halfway is not a download to
// keep going with. The hop count is capped so a server answering every
// request with a redirect is an error rather than a loop.
func (s Source) client() *http.Client {
	client := &http.Client{Timeout: 10 * time.Minute}
	if s.Client != nil {
		// The caller's transport, this program's redirect rule: a client
		// passed in for a test still travels the way a real one does.
		copied := *s.Client
		client = &copied
	}
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= maxHops {
			return fmt.Errorf("update: %s is behind more than %d redirects", via[0].URL, maxHops)
		}
		if via[0].URL.Scheme == "https" && request.URL.Scheme != "https" {
			return fmt.Errorf("update: %s redirected to %s, which is not https",
				via[0].URL, request.URL.Scheme)
		}
		return nil
	}
	return client
}

// sumFor reads one entry out of a sha256sum file.
func sumFor(sums []byte, name string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// sha256sum writes " " or " *" before the name.
		if strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		if len(fields[0]) != sha256.Size*2 {
			return "", fmt.Errorf("update: the checksum of %s is not a sha256", name)
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return "", fmt.Errorf("update: the checksum of %s is not hexadecimal", name)
		}
		return strings.ToLower(fields[0]), nil
	}
	return "", fmt.Errorf("update: the signed checksums do not mention %s", name)
}
