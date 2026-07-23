#!/usr/bin/env bash
# Asserts the chart version the DOCS tell a reader to install is self-consistent
# -- and, when given an expected version, that it matches the release being cut.
#
#   bash hack/check-doc-versions.sh              # docs agree with each other
#   bash hack/check-doc-versions.sh 0.1.0-rc.4   # ...and equal this version
#
# Why this exists: both docs pin `--version` on purpose (helm's unversioned OCI
# resolution does not select pre-releases, so while every published chart
# version is a pre-release an unpinned install resolves nothing and the
# quickstart is dead on arrival). A pin is only correct if it is kept current --
# a stale one can successfully install an older release, which is quieter and
# worse. An unpinned quickstart shipped through three RCs because the only thing
# guarding it was a checklist line, so this is the mechanical version of it: CI
# runs the no-argument form, and the release workflow runs it with the tag.
set -uo pipefail
cd "$(dirname "$0")/.."

QS=docs/quickstart.md
CR=charts/bard-csi/README.md
rc=0
fail() { echo "FAIL: $*" >&2; rc=1; }

# docs/quickstart.md: BARD_VERSION=<ver>  (the whole flow -- chart + the raw
# manifest URLs -- is pinned off this one assignment)
qs_ver=$(sed -n 's/^BARD_VERSION=\([^ ]*\).*/\1/p' "$QS" | head -1)
# charts/bard-csi/README.md: helm install ... --version <ver>
cr_ver=$(sed -n 's/.*--version \([0-9][0-9A-Za-z.+-]*\).*/\1/p' "$CR" | head -1)

semver='^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'

[ -n "$qs_ver" ] || fail "$QS: no 'BARD_VERSION=<version>' assignment found"
[ -n "$cr_ver" ] || fail "$CR: no 'helm install ... --version <version>' found"
[[ "$qs_ver" =~ $semver ]] || fail "$QS: BARD_VERSION='$qs_ver' is not a semver"
[[ "$cr_ver" =~ $semver ]] || fail "$CR: --version '$cr_ver' is not a semver"

if [ -n "$qs_ver" ] && [ -n "$cr_ver" ] && [ "$qs_ver" != "$cr_ver" ]; then
  fail "docs disagree: $QS says '$qs_ver', $CR says '$cr_ver'"
fi

# The quickstart's manifest URLs must ride the same pin, not a mutable branch --
# otherwise a pinned chart gets paired with whatever main happens to hold.
if grep -qE 'raw\.githubusercontent\.com/kindacoolhamster/bard-csi/(main|master)/' "$QS"; then
  fail "$QS: fetches from mutable main; use .../v\$BARD_VERSION/... instead"
fi

if [ "$#" -ge 1 ]; then
  want=${1#v}
  [ "$qs_ver" = "$want" ] || fail "docs pin '$qs_ver' but the release being cut is '$want' (bump the docs BEFORE tagging -- see RELEASING.md)"
fi

if [ "$rc" -eq 0 ]; then
  echo "OK: docs install version $qs_ver${1:+ (matches ${1#v})}, pinned consistently"
fi
exit "$rc"
