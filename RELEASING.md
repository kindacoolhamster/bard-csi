# Releasing Bard CSI

A release is cut by pushing a version tag; everything else is automated. The
tag is the single source of truth for the version — `Chart.yaml`'s
`version`/`appVersion` are dev placeholders that the workflow overrides at
package time, so no bump commit is needed.

```sh
git tag v0.1.0
git push origin v0.1.0
```

That fires two workflows:

- **images** — multi-arch (amd64/arm64) core + plugin images pushed to
  `ghcr.io/kindacoolhamster/<image>:{0.1.0, 0.1}`, each with a BuildKit **SPDX
  SBOM** and **SLSA provenance** attestation attached, and a **keyless cosign
  signature** over the digest.
- **release** — static binaries (+ `SHA256SUMS`) attached to a GitHub Release,
  the **`kubectl-bard` plugin** built for Linux/macOS/Windows (client-side, so
  more platforms than the server binaries — see docs/inspect.md for install),
  and the **Helm chart** packaged as `0.1.0`, pushed to
  `oci://ghcr.io/kindacoolhamster/charts/bard-csi`, cosign-signed, and attached
  to the Release as a `.tgz`.

The chart's empty image `tag` values default to `.Chart.AppVersion`, so the
`0.1.0` chart deploys the `0.1.0` images with no extra wiring.

## Verifying artifacts (what users run)

Signatures are keyless: the Fulcio certificate binds each signature to this
repo's workflow identity, so verification pins the repo — no key distribution.

```sh
# an image
cosign verify ghcr.io/kindacoolhamster/bard-csi:0.1.0 \
  --certificate-identity-regexp 'https://github.com/kindacoolhamster/bard-csi/\.github/workflows/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# the chart
cosign verify ghcr.io/kindacoolhamster/charts/bard-csi:0.1.0 \
  --certificate-identity-regexp 'https://github.com/kindacoolhamster/bard-csi/\.github/workflows/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# the SBOM / provenance attached to an image
docker buildx imagetools inspect ghcr.io/kindacoolhamster/bard-csi:0.1.0 \
  --format '{{ json .SBOM }}'
```

## Checklist per release

1. `main` green: `go build ./... && go vet ./... && go test ./...`, `gofmt -l .`
   empty, chart lints (CI runs all of it).
2. Skim `git log <last-tag>..` and write the Release notes (the workflow
   creates the Release; edit its body on GitHub afterwards — call out breaking
   changes and any plugin-contract version change).
3. Tag + push (above). Watch the two workflow runs. For a pre-release tag
   (`v0.1.0-rc.N`), confirm the resulting GitHub Release actually landed as a
   prerelease and isn't flagged the repo's "Latest" release --
   `gh api repos/kindacoolhamster/bard-csi/releases/latest` should 404 while
   the newest tag is still an RC. The workflow derives this from the tag's own
   semver prerelease identifier, but it's cheap enough to eyeball once per cut.
4. Sanity: `helm install` the pushed chart with the quickstart values on a
   fresh kind cluster (docs/quickstart.md flow, which pulls everything from
   the registry).

## One-time setup (before/at the first public release)

- **Make the GHCR packages public**: each package (`bard-csi`, the
  `bard-plugin-*` images, and `charts/bard-csi`) → Package settings → Change
  visibility → Public. Fresh packages default to private and the quickstart
  can't pull them.
- **Artifact Hub**: after the first chart push, register the repository at
  artifacthub.io → Control Panel → Add repository → kind **Helm charts (OCI)**,
  URL `oci://ghcr.io/kindacoolhamster/charts/bard-csi`. Optional: claim
  verified-publisher status by pushing an `artifacthub-repo.yml` (with the
  `repositoryID` Artifact Hub assigns) to the special
  `:artifacthub.io` tag of the chart package.
- **Versioning policy**: pre-1.0, minor (`v0.X.0`) may include breaking
  changes, called out in the notes; the plugin wire contract is versioned
  separately (`pkg/bardplugin.ContractVersion`) with its own compatibility
  promise — see docs/writing-a-plugin.md.
