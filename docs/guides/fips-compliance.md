# FIPS 140-3 Compliance

Attune supports running in FIPS 140-3 compliant mode for environments that
require validated cryptography (US federal agencies, FedRAMP, financial
services, healthcare).

## How it works

The Attune container image is built with Go's native FIPS cryptographic module
embedded (`GOFIPS140=latest`). This module holds
[CMVP Certificate #5247](https://csrc.nist.gov/projects/cryptographic-module-validation-program/certificate/5247)
(FIPS 140-3 Level 1), issued by NIST in April 2026.

FIPS mode is activated at runtime via the `GODEBUG=fips140=<mode>` environment
variable. The binary always contains the validated module; the flag controls
whether it is active.

## Enabling FIPS mode

### Via Helm (recommended)

```yaml
# values.yaml
fips:
  enabled: true
  mode: "on"     # or "only" for strict mode
```

```bash
helm upgrade attune oci://ghcr.io/attune-io/charts/attune \
  --set fips.enabled=true
```

### Via kubectl (manual deployment)

Add the environment variable to the manager container:

```yaml
env:
  - name: GODEBUG
    value: "fips140=on"
```

## FIPS modes

| Mode | Behavior | Recommended for |
|------|----------|-----------------|
| `on` | Prefer FIPS-approved algorithms; allow non-FIPS fallback when needed | Most deployments |
| `only` | Strictly FIPS-only; reject any non-FIPS algorithm | Environments requiring strict compliance |

### Choosing between `on` and `only`

**Use `on` (default)** for most deployments. This mode uses FIPS-approved
algorithms for all cryptographic operations while allowing fallback to
non-FIPS algorithms when required by the environment.

**Use `only` with caution.** Strict mode rejects non-FIPS algorithms entirely.
This can break TLS connections to the Kubernetes API server if the server
negotiates X25519 key exchange (common in default configurations). X25519 is
not a FIPS-approved algorithm, so `fips140=only` refuses to use it, causing
`client-go` connection failures.

!!! warning "Strict mode and Kubernetes API server"
    If you use `fips.mode: "only"`, verify your API server's TLS configuration
    supports FIPS-approved key exchange algorithms (ECDHE with P-256 or P-384).
    The default Kubernetes TLS cipher suite includes X25519, which `fips140=only`
    rejects.

## Verifying FIPS mode

Check the operator logs after deployment:

```bash
kubectl logs -n attune-system deploy/attune-controller-manager | head -20
```

When FIPS mode is active, the Go runtime logs:
```
GODEBUG fips140=on
```

## Industry context

FIPS mode in Attune follows the same pattern used by other Kubernetes operators:

- **Default off, opt-in to enable** (same as MinIO AIStor, Oracle Coherence,
  Elastic ECK, Bitnami charts, Tyk)
- **Runtime toggle via environment variable** (no separate binary or image)
- **CMVP-certified module** (Go's native module, not a third-party fork)

This approach avoids maintaining separate FIPS and non-FIPS container images
while providing validated cryptography when required.

## FAQ

**Do I need microsoft/go or BoringCrypto?**
No. Go 1.24+ includes a native FIPS 140-3 module with its own CMVP
certificate. The microsoft/go fork (which routes crypto to OpenSSL/CNG) is a
corporate policy choice, not a certification requirement.

**Does FIPS mode affect performance?**
Minimally. The FIPS module runs self-tests on startup (adds ~50ms) and uses
FIPS-approved algorithms that are comparable in performance to their
non-FIPS counterparts.

**Is the FIPS module always present in the image?**
Yes. The image is built with `GOFIPS140=latest`, which embeds the validated
module snapshot. The `fips.enabled` Helm value only controls whether the
module is activated at runtime via `GODEBUG`.
