# Multi-Namespace Example

Deploy right-sizing policies across dev, staging, and production namespaces
using Kustomize overlays. Each environment uses different aggressiveness
settings appropriate for its risk tolerance.

| Environment | Mode | Percentile | Max Change | Cooldown |
|-------------|------|------------|------------|----------|
| dev | Auto | P90 | 100% | 30m |
| staging | Canary (25%) | P95 | 50% | 1h |
| prod | Canary (10%) | P99 | 30% | 2h |

## Usage

```bash
# Deploy to dev
kubectl apply -k overlays/dev

# Deploy to staging
kubectl apply -k overlays/staging

# Deploy to production
kubectl apply -k overlays/prod
```

## Structure

- `base/` - Shared policy template and RightSizeDefaults
- `overlays/dev/` - Aggressive settings, Auto mode, fast cooldown
- `overlays/staging/` - Moderate settings, 25% canary
- `overlays/prod/` - Conservative settings, 10% canary, long cooldown