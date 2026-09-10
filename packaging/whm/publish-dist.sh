#!/bin/sh
# Put a signed build on the dist branch, which is what a server following
# the work reads its updates from.
#
# The branch holds four files and nothing else: a plugin tarball for each
# panel, the checksums, and the signature over them. It is written with git's plumbing
# rather than by checking the branch out, so the working tree this is run
# from is not touched and no branch is switched.
#
# Usage: sh packaging/whm/publish-dist.sh <dir with the built files> <version>
set -eu

BIN=${1:?usage: publish-dist.sh <bin dir> <version>}
VERSION=${2:?usage: publish-dist.sh <bin dir> <version>}
BRANCH=${GNIZA_DIST_BRANCH:-dist}
REMOTE=${GNIZA_DIST_REMOTE:-origin}

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

FILES="cprest-plugin-amd64.tar.gz gniza-directadmin-amd64.tar.gz SHA256SUMS SHA256SUMS.sig"

for name in $FILES; do
    [ -f "$BIN/$name" ] || die "$BIN/$name is missing; run make release, not this script"
done

# The signature is checked here as well as in the Makefile, because this is
# the last thing that runs before the files leave the machine.
openssl dgst -sha256 -verify internal/update/release.pub \
    -signature "$BIN/SHA256SUMS.sig" "$BIN/SHA256SUMS" >/dev/null \
    || die "the checksums are not signed by the release key; nothing was published"

# One tree of those blobs, made without touching the working tree.
tree=$(
    for name in $FILES; do
        blob=$(git hash-object -w "$BIN/$name")
        printf '100644 blob %s\t%s\n' "$blob" "$name"
    done | git mktree
)

parent=$(git rev-parse --verify --quiet "refs/heads/$BRANCH" || true)
if [ -z "$parent" ]; then
    parent=$(git rev-parse --verify --quiet "refs/remotes/$REMOTE/$BRANCH" || true)
fi
if [ -n "$parent" ]; then
    # Nothing to say if the same build is already there.
    if [ "$(git rev-parse "$parent^{tree}")" = "$tree" ]; then
        say "the dist branch already has this build ($VERSION)"
        exit 0
    fi
    commit=$(git commit-tree "$tree" -p "$parent" -m "gniza $VERSION")
else
    say "starting the $BRANCH branch"
    commit=$(git commit-tree "$tree" -m "gniza $VERSION")
fi

git update-ref "refs/heads/$BRANCH" "$commit"
say "committed $VERSION to $BRANCH as $(git rev-parse --short "$commit")"

if [ "${GNIZA_DIST_PUSH:-1}" = "0" ]; then
    say "not pushed (GNIZA_DIST_PUSH=0); push it with: git push $REMOTE $BRANCH"
    exit 0
fi
git push "$REMOTE" "$BRANCH"
say "pushed to $REMOTE/$BRANCH; servers on that channel will offer $VERSION"
