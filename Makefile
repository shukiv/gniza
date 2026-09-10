BIN  := bin
PKGS := ./...
# End-to-end tests spin up PostgreSQL clusters and restic repositories, which
# are too large for a small /tmp tmpfs. Keep their scratch space next to the
# repository instead.
E2E_TMPDIR := $(CURDIR)/.tmp

# Most cPanel servers are x86-64; override for an ARM host.
PLUGIN_ARCH         := amd64
RESTIC_VERSION      := v0.19.1
# What the agent reports as its own version, and what an update check
# compares against the newest release. A working tree that is not exactly a
# tag says so: "v0.1.0-3-gabc1234-dirty" is not a release.
VERSION             := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# The commit this build was made from, which is the only thing that puts two
# builds of a branch in order: v0.1.0-18-gabc1234 is not later than
# v0.1.0-9-gdef5678 in any order a computer can see. The commit's own time,
# not the moment of compilation, so two builds of one commit agree.
BUILT_AT            := $(shell git log -1 --format=%cI 2>/dev/null || echo "")
REST_SERVER_VERSION := v0.14.0
# Pinned, like everything else a build downloads: a release gate that
# fetches whatever is newest is a build step nobody reviewed.
GOVULNCHECK         := golang.org/x/vuln/cmd/govulncheck@v1.7.0

.PHONY: all build plugin directadmin-package release provenance test cover e2e vet vuln fmt tools clean

all: fmt vet test build

build:
	go build -o $(BIN)/gniza-agent       ./cmd/agent
	go build -o $(BIN)/gniza-controller  ./cmd/controller
	go build -o $(BIN)/gniza-maintenance ./cmd/maintenance

# The WHM plugin tarball: statically linked so it runs on any cPanel server
# without matching libc versions, and stripped because it ships over ssh.
#
# The tarball, the directory inside it and the word at the top of SHA256SUMS
# keep the name this program had before it was called Gniza. Servers running
# an older release have those spellings compiled in and ask for exactly them;
# see internal/update/install.go. Everything inside the tarball is named
# gniza.
plugin: directadmin-package
	# From scratch every time. This directory is assembled by copying into
	# it, so a file that was in the package yesterday and is not in it today
	# stays there and ships -- which is how a build after the rename to
	# Gniza put both cprest-agent and gniza-agent in the tarball.
	rm -rf $(BIN)/cprest-plugin
	mkdir -p $(BIN)/cprest-plugin
	CGO_ENABLED=0 GOOS=linux GOARCH=$(PLUGIN_ARCH) go build -trimpath \
		-ldflags="-s -w -X github.com/shukiv/gniza/internal/agent.Version=$(VERSION) \
			-X github.com/shukiv/gniza/internal/agent.BuiltAt=$(BUILT_AT)" \
		-o $(BIN)/cprest-plugin/gniza-agent ./cmd/agent
	cp packaging/whm/gniza.cgi packaging/whm/install.sh packaging/whm/uninstall.sh $(BIN)/cprest-plugin/
	mkdir -p $(BIN)/cprest-plugin/cpanel/uapi \
		$(BIN)/cprest-plugin/cpanel/admin/Gniza $(BIN)/cprest-plugin/branding
	cp packaging/cpanel/*.php packaging/cpanel/install.json $(BIN)/cprest-plugin/cpanel/
	cp packaging/cpanel/uapi/Gniza.pm $(BIN)/cprest-plugin/cpanel/uapi/
	cp packaging/cpanel/admin/Gniza/Session.pm $(BIN)/cprest-plugin/cpanel/admin/Gniza/
	cp packaging/branding/badge.svg packaging/branding/gniza-logo.svg \
		$(BIN)/cprest-plugin/branding/
	cp packaging/branding/png/badge-48.png $(BIN)/cprest-plugin/branding/
	chmod +x $(BIN)/cprest-plugin/install.sh $(BIN)/cprest-plugin/uninstall.sh $(BIN)/cprest-plugin/gniza.cgi
	tar -C $(BIN) --owner=0 --group=0 --numeric-owner --mode='u+rwX,go+rX,go-w' \
		-czf $(BIN)/cprest-plugin-$(PLUGIN_ARCH).tar.gz cprest-plugin
	cp packaging/whm/get.sh $(BIN)/get.sh
	@# The version goes inside the file that is signed, so a signature made
	@# for one release cannot be published again under another tag. sha256sum
	@# ignores a line beginning with #, and so does everything that reads
	@# this.
	@# Both packages are signed by the one file: get.sh downloads the one
	@# for the panel it finds and checks it against these lines, so a
	@# release that signs only cPanel's leaves a DirectAdmin server with
	@# nothing it can verify.
	cd $(BIN) && { printf '# cprest %s %s\n' '$(VERSION)' '$(BUILT_AT)'; \
		sha256sum cprest-plugin-$(PLUGIN_ARCH).tar.gz \
			gniza-directadmin-$(PLUGIN_ARCH).tar.gz get.sh; } > SHA256SUMS
	@echo
	@echo "built $(BIN)/cprest-plugin-$(PLUGIN_ARCH).tar.gz, $(BIN)/gniza-directadmin-$(PLUGIN_ARCH).tar.gz,"
	@echo "$(BIN)/get.sh and $(BIN)/SHA256SUMS"
	@echo "copy the one for the panel to the server:"
	@echo "  scp $(BIN)/cprest-plugin-$(PLUGIN_ARCH).tar.gz root@your-cpanel-server:/root/"
	@echo "  scp $(BIN)/gniza-directadmin-$(PLUGIN_ARCH).tar.gz root@your-directadmin-server:/root/"
	@echo "then there, as root:"
	@echo "  tar xzf cprest-plugin-$(PLUGIN_ARCH).tar.gz && cprest-plugin/install.sh"
	@echo "  tar xzf gniza-directadmin-$(PLUGIN_ARCH).tar.gz && sh gniza-directadmin/install.sh"

# Publish this build to the dist branch, signed.
#
# It is the whole update path without a release: build, sign the checksums
# with the key an operator keeps off this machine, and push the three files
# to a branch a server reads over https. What lands there is checked on the
# server exactly as a release is -- same key, same signature, same refusal
# if either is wrong.
#
# GNIZA_SIGNING_KEY_FILE says where the private key is. It is never read
# from the repository and never written into one.
# The DirectAdmin package.
#
# It is published beside the cPanel one, and get.sh installs whichever the
# server it runs on is: a DirectAdmin server told to install by hand what
# a release does not carry is a server that does not get installed. What
# the DirectAdmin provider still refuses to do, it refuses on the server
# and says so there (ADR 0019); a package nobody can fetch does not make
# that any safer.
directadmin-package:
	rm -rf $(BIN)/gniza-directadmin
	mkdir -p $(BIN)/gniza-directadmin/directadmin
	CGO_ENABLED=0 GOOS=linux GOARCH=$(PLUGIN_ARCH) go build -trimpath \
		-ldflags="-s -w -X github.com/shukiv/gniza/internal/agent.Version=$(VERSION) \
			-X github.com/shukiv/gniza/internal/agent.BuiltAt=$(BUILT_AT)" \
		-o $(BIN)/gniza-directadmin/gniza-agent ./cmd/agent
	cp packaging/directadmin/install.sh packaging/directadmin/uninstall.sh \
		$(BIN)/gniza-directadmin/
	cp -R packaging/directadmin/hooks \
		packaging/directadmin/admin packaging/directadmin/user \
		packaging/directadmin/images packaging/directadmin/scripts \
		$(BIN)/gniza-directadmin/directadmin/
	@# The card in DirectAdmin's plugin manager shows this version. A
	@# version that never moves reads as a plugin nobody maintains.
	sed 's|^version=.*|version=$(VERSION)|' packaging/directadmin/plugin.conf \
		> $(BIN)/gniza-directadmin/directadmin/plugin.conf
	@# The typefaces. DirectAdmin wraps everything a plugin prints in its
	@# own skin, so a font asked for through the plugin script arrives as
	@# HTML with a woff2 inside it; DirectAdmin serves the images
	@# directory as files, and that is where they have to be. One copy in
	@# the repository, in internal/webui/fonts, embedded in the agent for
	@# WHM and copied here for DirectAdmin.
	mkdir -p $(BIN)/gniza-directadmin/directadmin/images/fonts
	cp internal/webui/fonts/*.woff2 internal/webui/fonts/OFL.txt \
		$(BIN)/gniza-directadmin/directadmin/images/fonts/
	chmod +x $(BIN)/gniza-directadmin/install.sh $(BIN)/gniza-directadmin/uninstall.sh \
		$(BIN)/gniza-directadmin/directadmin/scripts/*.sh
	@# The archive must not carry the set-group-ID bit this checkout's
	@# own directories have; five digits, because coreutils 8.30 -- what
	@# these servers run -- keeps that bit through "chmod 0755".
	find $(BIN)/gniza-directadmin -type d -exec chmod 00755 {} +
	tar -C $(BIN) --owner=0 --group=0 --numeric-owner --mode='u+rwX,go+rX,go-w' \
		-czf $(BIN)/gniza-directadmin-$(PLUGIN_ARCH).tar.gz gniza-directadmin
	@echo "built $(BIN)/gniza-directadmin-$(PLUGIN_ARCH).tar.gz"

# The same plugin in the shape DirectAdmin's own plugin manager takes: a
# tar.gz of the plugin's files with nothing wrapped around them. The
# manager makes the directory itself, from the id in plugin.conf, and
# unpacks the archive into it -- which is why DirectAdmin's own example,
# hello_world.tar.gz, begins "admin/" and carries plugin.conf at its root
# rather than a directory named after the plugin. Then it runs
# scripts/install.sh as root, which runs the installer beside it: the
# same installer somebody unpacking the release tarball runs by hand.
directadmin-plugin:
	rm -rf $(BIN)/gniza-plugin
	mkdir -p $(BIN)/gniza-plugin
	CGO_ENABLED=0 GOOS=linux GOARCH=$(PLUGIN_ARCH) go build -trimpath \
		-ldflags="-s -w -X github.com/shukiv/gniza/internal/agent.Version=$(VERSION) \
			-X github.com/shukiv/gniza/internal/agent.BuiltAt=$(BUILT_AT)" \
		-o $(BIN)/gniza-plugin/gniza-agent ./cmd/agent
	cp -R packaging/directadmin/hooks packaging/directadmin/admin \
		packaging/directadmin/user packaging/directadmin/images \
		packaging/directadmin/scripts $(BIN)/gniza-plugin/
	cp packaging/directadmin/install.sh packaging/directadmin/uninstall.sh \
		$(BIN)/gniza-plugin/
	sed 's|^version=.*|version=$(VERSION)|' packaging/directadmin/plugin.conf \
		> $(BIN)/gniza-plugin/plugin.conf
	mkdir -p $(BIN)/gniza-plugin/images/fonts
	cp internal/webui/fonts/*.woff2 internal/webui/fonts/OFL.txt \
		$(BIN)/gniza-plugin/images/fonts/
	chmod +x $(BIN)/gniza-plugin/install.sh \
		$(BIN)/gniza-plugin/uninstall.sh \
		$(BIN)/gniza-plugin/scripts/*.sh
	@# Named one by one rather than as ".", so that the archive holds the
	@# same entries DirectAdmin's own example does and nothing else.
	@# The archive must not carry the set-group-ID bit this checkout's
	@# own directories have; five digits, because coreutils 8.30 -- what
	@# these servers run -- keeps that bit through "chmod 0755".
	find $(BIN)/gniza-plugin -type d -exec chmod 00755 {} +
	tar -C $(BIN)/gniza-plugin --owner=0 --group=0 --numeric-owner \
		--mode='u+rwX,go+rX,go-w' \
		-czf $(BIN)/gniza-plugin-$(PLUGIN_ARCH).tar.gz \
		plugin.conf admin user hooks images scripts \
		install.sh uninstall.sh gniza-agent
	@echo "built $(BIN)/gniza-plugin-$(PLUGIN_ARCH).tar.gz -- install it from DirectAdmin's plugin manager"

# What the artifact was really built from.
#
# govulncheck reads the source; this reads the binary. A build made by an
# older toolchain carries that toolchain's standard library, and editing
# go.mod does not repair an executable that already exists. So the version
# stamped in the file has to be the one this checkout selects, or the
# release is of something other than what was reviewed.
provenance:
	@built=$$(go version -m $(BIN)/cprest-plugin/gniza-agent | awk 'NR==1 {print $$2}'); \
	want=$$(go env GOVERSION); \
	if [ "$$built" != "$$want" ]; then \
		echo "the plugin binary was built by $$built, but this tree builds with $$want" >&2; \
		echo "run 'make plugin' again with the toolchain go.mod asks for" >&2; \
		exit 1; \
	fi; \
	echo "$(BIN)/cprest-plugin/gniza-agent: built by $$built"

release: plugin
	$(MAKE) provenance
	@[ -n "$(GNIZA_SIGNING_KEY_FILE)" ] || { \
		echo "set GNIZA_SIGNING_KEY_FILE to the release key, e.g."; \
		echo "  make release GNIZA_SIGNING_KEY_FILE=~/.gniza/gniza-release.pem"; \
		exit 1; }
	openssl dgst -sha256 -sign "$(GNIZA_SIGNING_KEY_FILE)" \
		-out $(BIN)/SHA256SUMS.sig $(BIN)/SHA256SUMS
	@# Verified here with the key that is compiled into the agent, so a
	@# mismatched private key fails on this machine rather than on a server.
	openssl dgst -sha256 -verify internal/update/release.pub \
		-signature $(BIN)/SHA256SUMS.sig $(BIN)/SHA256SUMS
	sh packaging/whm/publish-dist.sh $(BIN) $(VERSION)

# Unit and integration tests. The store suite starts a throwaway PostgreSQL
# and skips itself when none is installed, so this stays runnable anywhere.
# The end-to-end suite is behind a build tag; see the e2e target.
test:
	go test $(PKGS)

cover:
	go test -coverprofile=coverage.out $(PKGS)
	go tool cover -func=coverage.out | tail -1

# The full pipeline against real dependencies. Needs PostgreSQL, restic and
# rest-server; run "make tools" first.
#
# The installed tools come first on PATH, not last: RESTIC_VERSION is
# pinned because restic's behaviour differs between releases, and a
# distribution's own restic further down the path would quietly decide
# what the suite proves.
e2e:
	mkdir -p $(E2E_TMPDIR)
	TMPDIR=$(E2E_TMPDIR) PATH="$(shell go env GOPATH)/bin:$(PATH)" \
		go test -tags e2e ./internal/e2e/ -count=1 -timeout 30m

tools:
	go install github.com/restic/restic/cmd/restic@$(RESTIC_VERSION)
	go install github.com/restic/rest-server/cmd/rest-server@$(REST_SERVER_VERSION)

vet:
	go vet $(PKGS)
	go vet -tags e2e ./internal/e2e/

# Known vulnerabilities that this code actually reaches.
#
# A backup program is worth attacking, and the release is a static binary:
# whatever the standard library and the dependencies were on the day it was
# built is what every server runs until it is replaced. So this is a gate on
# the release rather than something to run when somebody remembers.
vuln:
	go run $(GOVULNCHECK) ./...

fmt:
	gofmt -l -w .

clean:
	trash $(BIN) coverage.out $(E2E_TMPDIR) 2>/dev/null || true
