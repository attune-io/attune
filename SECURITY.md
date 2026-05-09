# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in kube-rightsize, please report it
responsibly.

**DO NOT** create a public GitHub issue for security vulnerabilities.

Instead, please report vulnerabilities via GitHub's private security advisory
feature: [Report a vulnerability](https://github.com/SebTardif/kube-rightsize/security/advisories/new).

## Supported Versions

| Version | Supported |
|---------|-----------|
| Latest release | Yes |
| Previous release | Security fixes only |
| Older | No |

## Security Practices

- Container images are signed with [cosign](https://github.com/sigstore/cosign)
  using keyless signing (Sigstore OIDC)
- SBOMs are generated for every release (SPDX format)
- Dependencies are scanned weekly with Trivy and govulncheck
- CodeQL analysis runs on every PR and weekly
- The operator runs as non-root with a read-only root filesystem
- RBAC permissions follow least-privilege principles
