# Security Policy

Bard moves data and handles storage credentials (cephx keys, KMS material,
encryption passphrases), so security reports get priority handling.

## Reporting a vulnerability

**Please do not open a public issue for a suspected vulnerability.**

Preferred: GitHub private vulnerability reporting — the **Security** tab →
**Report a vulnerability** on this repository.

Alternatively, email <kindacoolhamster@gmail.com> with `[bard-csi security]` in the
subject line.

You will get an acknowledgment within 48 hours. Expect a fix or a concrete
remediation plan — and credit, if you want it — before any public
disclosure; please allow a coordinated-disclosure window (up to 90 days for
complex issues).

## Supported versions

Pre-1.0, only the most recent release receives security fixes.

## Areas of particular interest

- Credential handling: per-instance key resolution, the KMS providers, the
  LUKS and fscrypt encryption paths.
- The plugin socket contract (privilege boundaries between core and plugins).
- Anything that lets one volume, namespace, or tenant read another's data.
