# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in attune, please report it
responsibly.

**DO NOT** create a public GitHub issue for security vulnerabilities.

Instead, please report vulnerabilities via GitHub's private security advisory
feature: [Report a vulnerability](https://github.com/attune-io/attune/security/advisories/new).

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
- Static analysis via golangci-lint (50+ linters) runs on every push
- The operator runs as non-root with a read-only root filesystem
- RBAC permissions follow least-privilege principles
