# Contributing to Bard CSI

Thanks for your interest in Bard. Bug reports, docs fixes, new backend
plugins, and core improvements are all welcome. For anything non-trivial,
open an issue first so we can agree on the approach before you invest time.

## The fastest way to contribute: a backend plugin

Bard backends are out-of-tree plugins — a small HTTP+JSON server on a unix
socket, written in **any language**. You don't need to understand Bard's core
(or know Go) to add support for your storage system. Start with
[docs/writing-a-plugin.md](docs/writing-a-plugin.md);
[plugins/localpath](plugins/localpath) is a complete, dependency-free Python
example. Open a **New backend plugin** issue to discuss a backend you want to
build or see built.

## Development setup

You need Go 1.26+. The core test suite is hermetic — storage commands run
against an in-memory fake runner, so **no Ceph cluster, root access, or
special hardware is required**:

```sh
go build ./... && go vet ./... && go test ./...
gofmt -l .        # must print nothing
```

`go test ./internal/driver -run TestSanity` runs the upstream csi-sanity
suite against the driver.

Live-fixture tests for specific backends (a real Ceph cluster, a real LVM
volume group, a real LIO iSCSI target) live under [hack/](hack/) and are
documented there. They are **not** required for most PRs — CI runs the
hermetic suite.

## Pull requests

- Match the existing style; `gofmt` is enforced. For behavior changes,
  prefer table-driven tests against the fake command runner.
- Commit messages follow `area: summary`, lower case, imperative — e.g.
  `lvm: reject reserved volume names`.
- **Sign off every commit** (`git commit -s`). This asserts the
  [Developer Certificate of Origin](https://developercertificate.org/):
  you certify you have the right to submit the code under the project
  license. No CLA.
- Keep PRs focused; separate refactors from behavior changes.

## Reporting bugs

Use the bug-report issue template — the version, backend, and log fields it
asks for are what's needed to reproduce a storage problem. For suspected
security issues, see [SECURITY.md](SECURITY.md) — please do **not** open a
public issue.

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).
