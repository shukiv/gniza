package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// release is a published release as a test can serve one: the three files,
// signed with a key the test holds.
type release struct {
	files map[string][]byte
	key   *ecdsa.PrivateKey
}

func newRelease(t *testing.T, tarball []byte) *release {
	t.Helper()
	return newReleaseFor(t, tarball, "v9.9.9")
}

// newReleaseFor publishes a release whose checksums say which release they
// belong to, as the build writes them.
func newReleaseFor(t *testing.T, tarball []byte, version string) *release {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(tarball)
	sums := []byte("# cprest " + version + "\n" +
		hex.EncodeToString(sum[:]) + "  " + TarballName + "\nabc  get.sh\n")
	digest := sha256.Sum256(sums)
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return &release{
		key: key,
		files: map[string][]byte{
			TarballName: tarball,
			SumsName:    sums,
			SigName:     signature,
		},
	}
}

// publish adds another asset to a release and signs the checksums again,
// the way a build that writes one line per package does.
func (r *release) publish(t *testing.T, name string, body []byte) {
	t.Helper()
	sum := sha256.Sum256(body)
	sums := append([]byte{}, r.files[SumsName]...)
	sums = append(sums, []byte(hex.EncodeToString(sum[:])+"  "+name+"\n")...)
	digest := sha256.Sum256(sums)
	signature, err := ecdsa.SignASN1(rand.Reader, r.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	r.files[name] = body
	r.files[SumsName] = sums
	r.files[SigName] = signature
}

func (r *release) serve(t *testing.T) Source {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, found := r.files[filepath.Base(req.URL.Path)]
		if !found {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return Source{Client: server.Client(), Base: server.URL, Key: &r.key.PublicKey}
}

// plugin is a tarball shaped like a release: one top directory with the
// installer in it.
func plugin(t *testing.T, entries ...*tar.Header) []byte {
	t.Helper()
	var buffer bytes.Buffer
	zipped := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(zipped)
	write := func(name, body string) {
		if err := archive.WriteHeader(&tar.Header{
			Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write("cprest-plugin/install.sh", "#!/bin/sh\nexit 0\n")
	write("cprest-plugin/gniza-agent", "not really a binary")
	for _, header := range entries {
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestFetchTakesASignedRelease(t *testing.T) {
	published := newRelease(t, plugin(t))
	source := published.serve(t)
	dir := t.TempDir()

	tarball, err := source.Fetch(context.Background(), "v9.9.9", dir, WHMPackage)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	body, err := os.ReadFile(tarball)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, published.files[TarballName]) {
		t.Error("what was written is not what was served")
	}

	into := filepath.Join(dir, "unpacked")
	if err := Unpack(tarball, into, WHMPackage); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if _, err := os.Stat(filepath.Join(into, "cprest-plugin", "install.sh")); err != nil {
		t.Errorf("the installer is not there: %v", err)
	}
}

// TestFetchRefusesWhatTheKeyDidNotSign is the test this whole package
// exists for: everything it downloads is run as root.
func TestFetchRefusesWhatTheKeyDidNotSign(t *testing.T) {
	broken := map[string]func(*release){
		"a signature from another key": func(r *release) {
			other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(r.files[SumsName])
			signature, err := ecdsa.SignASN1(rand.Reader, other, digest[:])
			if err != nil {
				t.Fatal(err)
			}
			r.files[SigName] = signature
		},
		"checksums changed after signing": func(r *release) {
			r.files[SumsName] = append([]byte("# added after signing\n"), r.files[SumsName]...)
		},
		"no signature published": func(r *release) { delete(r.files, SigName) },
		// Making a tag is not holding the key. Publishing an old release
		// again under a new number is the one attack a signature alone
		// does not stop.
		"an older release published again under this number": func(r *release) {
			old := newReleaseFor(t, plugin(t), "v0.0.1")
			old.key = r.key
			resigned := newReleaseFor(t, old.files[TarballName], "v0.0.1")
			resigned.key = r.key
			digest := sha256.Sum256(resigned.files[SumsName])
			signature, err := ecdsa.SignASN1(rand.Reader, r.key, digest[:])
			if err != nil {
				t.Fatal(err)
			}
			r.files[SumsName] = resigned.files[SumsName]
			r.files[SigName] = signature
			r.files[TarballName] = resigned.files[TarballName]
		},
		"checksums that say no release at all": func(r *release) {
			sums := []byte(strings.SplitN(string(r.files[SumsName]), "\n", 2)[1])
			digest := sha256.Sum256(sums)
			signature, err := ecdsa.SignASN1(rand.Reader, r.key, digest[:])
			if err != nil {
				t.Fatal(err)
			}
			r.files[SumsName], r.files[SigName] = sums, signature
		},
		"a tarball that is not the one signed for": func(r *release) {
			r.files[TarballName] = plugin(t, &tar.Header{
				Name: "cprest-plugin/extra", Mode: 0o644, Typeflag: tar.TypeReg,
			})
		},
		"nothing about the tarball in the checksums": func(r *release) {
			lines := strings.SplitN(string(r.files[SumsName]), "\n", 3)
			sums := []byte(lines[0] + "\n" + lines[2])
			digest := sha256.Sum256(sums)
			signature, err := ecdsa.SignASN1(rand.Reader, r.key, digest[:])
			if err != nil {
				t.Fatal(err)
			}
			r.files[SumsName], r.files[SigName] = sums, signature
		},
	}
	for what, breakIt := range broken {
		t.Run(what, func(t *testing.T) {
			published := newRelease(t, plugin(t))
			breakIt(published)
			source := published.serve(t)
			dir := t.TempDir()

			if _, err := source.Fetch(context.Background(), "v9.9.9", dir, WHMPackage); err == nil {
				t.Fatal("a release that should not have been believed was accepted")
			}
			// Nothing that failed a check is left where an installer
			// could be pointed at it.
			if _, err := os.Stat(filepath.Join(dir, TarballName)); err == nil {
				t.Error("the tarball was kept")
			}
		})
	}
}

func TestFetchOnlyTakesReleaseVersions(t *testing.T) {
	published := newRelease(t, plugin(t))
	source := published.serve(t)
	for _, version := range []string{
		"", "dev", "v0.1", "v0.1.0-3-gabc1234-dirty", "../../etc", "v1.0.0/x", "v9.9.9\n", " v9.9.9",
	} {
		if _, err := source.Fetch(context.Background(), version, t.TempDir(), WHMPackage); err == nil {
			t.Errorf("%q was fetched", version)
		}
	}
}

func TestUnpackRefusesWhatWritesElsewhere(t *testing.T) {
	refused := map[string]*tar.Header{
		"a path climbing out": {Name: "cprest-plugin/../../evil", Mode: 0o644, Typeflag: tar.TypeReg},
		"an absolute path":    {Name: "/etc/cron.d/evil", Mode: 0o644, Typeflag: tar.TypeReg},
		"a file of its own":   {Name: "elsewhere/evil", Mode: 0o644, Typeflag: tar.TypeReg},
		"a symlink":           {Name: "cprest-plugin/link", Linkname: "/etc/shadow", Typeflag: tar.TypeSymlink},
		"a hard link":         {Name: "cprest-plugin/hard", Linkname: "/etc/shadow", Typeflag: tar.TypeLink},
		"a device":            {Name: "cprest-plugin/disk", Typeflag: tar.TypeChar},
	}
	for what, entry := range refused {
		t.Run(what, func(t *testing.T) {
			dir := t.TempDir()
			tarball := filepath.Join(dir, TarballName)
			if err := os.WriteFile(tarball, plugin(t, entry), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := Unpack(tarball, filepath.Join(dir, "unpacked"), WHMPackage); err == nil {
				t.Fatal("the archive was unpacked")
			}
			if _, err := os.Stat(filepath.Join(dir, "evil")); err == nil {
				t.Error("a file was written outside the directory")
			}
		})
	}
}

func TestUnpackNeedsAnInstaller(t *testing.T) {
	var buffer bytes.Buffer
	zipped := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(zipped)
	if err := archive.WriteHeader(&tar.Header{
		Name: "cprest-plugin/readme", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	_ = archive.Close()
	_ = zipped.Close()

	dir := t.TempDir()
	tarball := filepath.Join(dir, TarballName)
	if err := os.WriteFile(tarball, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Unpack(tarball, filepath.Join(dir, "unpacked"), WHMPackage); err == nil {
		t.Fatal("an archive with no installer in it was accepted")
	}
}

// TestTheKeyIsTheSameEverywhere guards the one drift that would be found
// only by a server failing to update: get.sh carries the same key written
// out, because a shell script has no binary to embed it in.
func TestTheKeyIsTheSameEverywhere(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "packaging", "whm", "get.sh"))
	if err != nil {
		t.Fatal(err)
	}
	const begin, end = "-----BEGIN PUBLIC KEY-----", "-----END PUBLIC KEY-----"
	from := strings.Index(string(script), begin)
	to := strings.Index(string(script), end)
	if from < 0 || to < 0 {
		t.Fatal("get.sh has no public key in it")
	}
	inScript := strings.TrimSpace(string(script)[from : to+len(end)])
	if inScript != strings.TrimSpace(string(releaseKeyPEM)) {
		t.Errorf("get.sh carries a different release key:\n%s\n---\n%s",
			inScript, strings.TrimSpace(string(releaseKeyPEM)))
	}
	if _, err := ReleaseKey(); err != nil {
		t.Errorf("the embedded release key does not parse: %v", err)
	}
}

// TestFetchFollowsARedirectButNotOffHTTPS covers what a release download
// actually does -- GitHub sends it on to the storage holding the file --
// and where it stops.
func TestFetchFollowsARedirectButNotOffHTTPS(t *testing.T) {
	published := newRelease(t, plugin(t))
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, found := published.files[filepath.Base(req.URL.Path)]
		if !found {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(files.Close)

	t.Run("one hop is followed", func(t *testing.T) {
		front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, files.URL+req.URL.Path, http.StatusFound)
		}))
		t.Cleanup(front.Close)

		source := Source{Client: front.Client(), Base: front.URL, Key: &published.key.PublicKey}
		if _, err := source.Fetch(context.Background(), "v9.9.9", t.TempDir(), WHMPackage); err != nil {
			t.Fatalf("Fetch through a redirect: %v", err)
		}
	})

	t.Run("a plain address is not", func(t *testing.T) {
		secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, files.URL+req.URL.Path, http.StatusFound)
		}))
		t.Cleanup(secure.Close)

		source := Source{Client: secure.Client(), Base: secure.URL, Key: &published.key.PublicKey}
		_, err := source.Fetch(context.Background(), "v9.9.9", t.TempDir(), WHMPackage)
		if err == nil {
			t.Fatal("a download was followed off https onto a plain connection")
		}
		if !strings.Contains(err.Error(), "not https") {
			t.Errorf("the reason is not the downgrade: %v", err)
		}
	})

	t.Run("a server that only redirects", func(t *testing.T) {
		var loop *httptest.Server
		loop = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, loop.URL+req.URL.Path, http.StatusFound)
		}))
		t.Cleanup(loop.Close)

		source := Source{Client: loop.Client(), Base: loop.URL, Key: &published.key.PublicKey}
		if _, err := source.Fetch(context.Background(), "v9.9.9", t.TempDir(), WHMPackage); err == nil {
			t.Fatal("a redirect loop was followed to the end")
		}
	})
}

// TestFetchReadsOnlyOverHTTP is the other end of the same rule: an
// operator's own GNIZA_RELEASE_BASE moves where a release is read from,
// not how.
func TestFetchReadsOnlyOverHTTP(t *testing.T) {
	published := newRelease(t, plugin(t))
	for _, base := range []string{"file:///tmp", "ftp://example.com/releases", "/tmp/releases"} {
		source := Source{Base: base, Key: &published.key.PublicKey}
		_, err := source.Fetch(context.Background(), "v9.9.9", t.TempDir(), WHMPackage)
		if err == nil {
			t.Errorf("%q was read from", base)
		}
	}
}

// TestDistChannelReadsTheBranch: a branch has one copy of each file and no
// version in its path, so what says which build it is is the manifest --
// and the manifest is only worth reading because the release key signed it.
func TestDistChannelReadsTheBranch(t *testing.T) {
	body := plugin(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	built := time.Now().UTC().Truncate(time.Second)
	sign := func(version string, at time.Time, tarball []byte) map[string][]byte {
		sum := sha256.Sum256(tarball)
		sums := []byte("# cprest " + version + " " + at.Format(time.RFC3339) + "\n" +
			hex.EncodeToString(sum[:]) + "  " + TarballName + "\n")
		digest := sha256.Sum256(sums)
		signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		return map[string][]byte{TarballName: tarball, SumsName: sums, SigName: signature}
	}

	files := sign("v0.1.0-18-gabc1234", built, body)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// A branch is flat: the request must not carry a version.
		if strings.Count(strings.Trim(req.URL.Path, "/"), "/") != 0 {
			t.Errorf("the dist channel asked for %s", req.URL.Path)
		}
		found, ok := files[filepath.Base(req.URL.Path)]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(found)
	}))
	t.Cleanup(server.Close)
	source := Source{Client: server.Client(), Base: server.URL, Key: &key.PublicKey, Flat: true}

	published, err := source.Published(context.Background(), "")
	if err != nil {
		t.Fatalf("Published: %v", err)
	}
	if published.Version != "v0.1.0-18-gabc1234" {
		t.Errorf("version = %q", published.Version)
	}
	if !published.BuiltAt.Equal(built) {
		t.Errorf("built at %s, want %s", published.BuiltAt, built)
	}

	if _, err := source.Fetch(context.Background(), published.Version, t.TempDir(), WHMPackage); err != nil {
		t.Fatalf("Fetch from the branch: %v", err)
	}

	// What the operator agreed to is what gets installed. A branch moves:
	// if a different build is published between the page and the button,
	// the install stops rather than fetching something else.
	files = sign("v0.1.0-19-gdef5678", built.Add(time.Minute), plugin(t))
	if _, err := source.Fetch(context.Background(), "v0.1.0-18-gabc1234", t.TempDir(), WHMPackage); err == nil {
		t.Error("a build nobody agreed to was installed")
	}

	// An unsigned manifest is refused here as everywhere.
	files[SigName] = []byte("not a signature")
	if _, err := source.Published(context.Background(), ""); err == nil {
		t.Error("an unsigned branch build was believed")
	}
}

// TestEachPanelTakesItsOwnPackage: a release publishes one package per
// panel, signed in the same checksums, and a server takes the one for the
// panel it runs. A DirectAdmin server that fetched cPanel's would install
// a plugin for a panel that is not there and leave its own without one.
func TestEachPanelTakesItsOwnPackage(t *testing.T) {
	forDirectAdmin := packageFor(t, DirectAdminPackage.TopDir)
	published := newRelease(t, plugin(t))
	published.publish(t, DirectAdminPackage.Asset, forDirectAdmin)
	source := published.serve(t)

	if _, err := PackageFor("Plesk"); err == nil {
		t.Error("a panel a release publishes nothing for was given a package")
	}
	if plain, err := PackageFor("Plain server"); err != nil || plain != PlainPackage {
		t.Errorf("PackageFor(Plain server) = %v, %v; want the plain package", plain, err)
	}
	want, err := PackageFor("DirectAdmin")
	if err != nil {
		t.Fatalf("PackageFor(DirectAdmin): %v", err)
	}

	dir := t.TempDir()
	tarball, err := source.Fetch(context.Background(), "v9.9.9", dir, want)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if filepath.Base(tarball) != DirectAdminPackage.Asset {
		t.Errorf("a DirectAdmin server downloaded %s", filepath.Base(tarball))
	}
	into := filepath.Join(dir, "unpacked")
	if err := Unpack(tarball, into, want); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if _, err := os.Stat(filepath.Join(into, DirectAdminPackage.TopDir, "install.sh")); err != nil {
		t.Errorf("the DirectAdmin installer is not there: %v", err)
	}

	// And the two do not stand in for one another: the cPanel package
	// unpacked as DirectAdmin's is a directory that is not the one being
	// asked for, whatever it holds.
	if err := Unpack(filepath.Join(dir, "wrong.tar.gz"), into, want); err == nil {
		t.Error("a tarball that is not there unpacked")
	}
	whm := filepath.Join(dir, WHMPackage.Asset)
	if err := os.WriteFile(whm, published.files[TarballName], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Unpack(whm, filepath.Join(dir, "crossed"), want); err == nil {
		t.Error("a DirectAdmin server unpacked the cPanel package")
	}
}

// packageFor is a tarball shaped like a release for one panel: one top
// directory, named for that package, with the installer in it.
func packageFor(t *testing.T, top string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	zipped := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(zipped)
	for name, body := range map[string]string{
		top + "/install.sh":  "#!/bin/sh\nexit 0\n",
		top + "/gniza-agent": "not really a binary",
	} {
		if err := archive.WriteHeader(&tar.Header{
			Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
