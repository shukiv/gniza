#!/bin/sh
# Put Gniza on this server, from a published release.
#
#   curl -fsSL https://github.com/shukiv/gniza/releases/latest/download/get.sh | sh
#
# It downloads one tarball, checks it against the checksums published beside
# it, unpacks it into a temporary directory and hands over to the installer
# inside. Nothing is left behind but what that installer puts in place.
#
# The checksums are signed, and the key that signs them is in this script
# rather than fetched beside them: a checksum file from the same page as the
# tarball proves only that a download arrived whole, while a signature over
# it says the release came from whoever holds the release key. Both are
# checked before anything is unpacked, and a failure of either stops the
# install.
#
# This script is short so that reading it before running it as root is a
# minute's work rather than an act of faith.
#
# GNIZA_VERSION=v1.2.3   install that release instead of the newest
# GNIZA_TARBALL=/path    install a tarball already on this machine, which is
#                         yours to trust: nothing is downloaded, so nothing is
#                         verified
set -eu

REPO=shukiv/gniza
RELEASES=https://github.com/$REPO/releases

# The public half of the release signing key. Its private half signs
# SHA256SUMS in the release workflow and exists nowhere on any server.
release_key() {
    cat <<'KEY'
-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE2MDPJ3fp3foGtRq84rWhpvKtWvOz
bQfXxTAfSVvgmMJXAqovklf7eUc3C/nCXsvTEyN1x7uXWbQm6Fnh/Udn3A==
-----END PUBLIC KEY-----
KEY
}

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# Everything is inside main, called on the last line. Read from a pipe, a
# download that stops half way through then runs nothing at all, rather than
# running the half that arrived.
main() {
    [ "$(id -u)" = 0 ] || die "run this as root"

    case "$(uname -m)" in
        x86_64) arch=amd64 ;;
        *)      die "the published builds are x86-64 only; on $(uname -m), build from source with 'make plugin'" ;;
    esac

    # Which package this server takes. A release publishes one per panel,
    # and installing the other one puts a plugin on a machine that has no
    # panel to show it. cPanel first, so a server that somehow has both
    # directories gets the package it has always had.
    #
    # The cPanel asset keeps the name from before the rename to Gniza,
    # because servers running an older release ask for it by that name.
    # See internal/update/install.go.
    if [ -d /usr/local/cpanel ]; then
        panel=cPanel
        tarball=cprest-plugin-$arch.tar.gz
        tree=cprest-plugin
    elif [ -d /usr/local/directadmin ]; then
        panel=DirectAdmin
        tarball=gniza-directadmin-$arch.tar.gz
        tree=gniza-directadmin
    else
        die "this does not look like a cPanel or a DirectAdmin server (neither /usr/local/cpanel nor /usr/local/directadmin is here)"
    fi
    say "installing the $panel package"

    if [ -n "${GNIZA_VERSION:-}" ]; then
        base=$RELEASES/download/$GNIZA_VERSION
    else
        base=$RELEASES/latest/download
    fi

    for tool in curl tar sha256sum openssl; do
        command -v "$tool" >/dev/null 2>&1 || die "$tool is needed; install it and run this again"
    done

    work=$(mktemp -d /var/tmp/gniza-install.XXXXXX)
    trap 'rm -rf -- "$work"' 0 1 2 15

    if [ -n "${GNIZA_TARBALL:-}" ]; then
        say "installing from $GNIZA_TARBALL"
        cp "$GNIZA_TARBALL" "$work/$tarball"
    else
        say "downloading $base/$tarball"
        curl -fsSL -o "$work/$tarball" "$base/$tarball" \
            || die "could not download $base/$tarball"
        curl -fsSL -o "$work/SHA256SUMS" "$base/SHA256SUMS" \
            || die "could not download $base/SHA256SUMS"
        curl -fsSL -o "$work/SHA256SUMS.sig" "$base/SHA256SUMS.sig" \
            || die "could not download $base/SHA256SUMS.sig; releases before v0.2.0 are not signed"

        # The signature first: an unsigned or wrongly signed checksum file is
        # not worth checking a download against.
        release_key > "$work/release.pub"
        openssl dgst -sha256 -verify "$work/release.pub" \
            -signature "$work/SHA256SUMS.sig" "$work/SHA256SUMS" >/dev/null 2>&1 \
            || die "the checksums are not signed by the Gniza release key; nothing was installed"

        # Which release those checksums were published for. The build
        # writes it into the file that gets signed, so a signature made
        # for one release cannot be published again under another tag by
        # somebody who can make a tag but does not hold the key.
        # Two patterns rather than one loose one: a release on its own,
        # and a release followed by the commit time the build stamps in
        # so that two builds of a branch can be put in order. Neither
        # matches a line with anything else in the version field.
        signed_for=$(sed -n \
            -e 's/^# cprest \(v[0-9][0-9.]*\)$/\1/p' \
            -e 's/^# cprest \(v[0-9][0-9.]*\) [^ ]*$/\1/p' \
            "$work/SHA256SUMS" | head -1)
        [ -n "$signed_for" ] \
            || die "the published checksums do not say which release they are for; nothing was installed"
        if [ -n "${GNIZA_VERSION:-}" ] && [ "$signed_for" != "$GNIZA_VERSION" ]; then
            die "those checksums are signed for $signed_for, not $GNIZA_VERSION; nothing was installed"
        fi
        say "signature ok, for $signed_for"

        # Only the line for the file actually downloaded: the rest of that
        # file names things this machine did not fetch.
        grep " $tarball\$" "$work/SHA256SUMS" > "$work/expected" \
            || die "the published checksums do not mention $tarball"
        ( cd "$work" && sha256sum -c expected ) >/dev/null \
            || die "the download does not match its published checksum; nothing was installed"
        say "checksum ok"
    fi

    tar -C "$work" -xzf "$work/$tarball"
    [ -f "$work/$tree/install.sh" ] || die "that tarball has no $tree/install.sh in it"

    # Run it through sh rather than as a program. cPanel mounts /tmp and
    # /var/tmp noexec, so a script unpacked there cannot be executed even by
    # root, whatever its mode says. Nothing in the package is executed from
    # here: the installer copies its files into place with install(1).
    say "running the installer"
    sh "$work/$tree/install.sh"
}

main "$@"
