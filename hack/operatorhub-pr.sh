#!/usr/bin/env bash
# Create or update a PR to k8s-operatorhub/community-operators for a new Attune release.
#
# Usage: VERSION=0.1.7 GH_TOKEN=<pat> hack/operatorhub-pr.sh
#
# Required env vars:
#   VERSION         - Release version without 'v' prefix (e.g., 0.1.7)
#   GH_TOKEN        - GitHub token (App token from CI, or PAT for local use)
#
# Optional env vars:
#   FORK_OWNER      - GitHub user owning the fork (default: SebTardif)
#   GIT_USER_NAME   - Git commit author name (default: github-actions[bot])
#   GIT_USER_EMAIL  - Git commit author email (default: 41898282+github-actions[bot]@users.noreply.github.com)
#   BUNDLE_DIR      - Pre-generated bundle path (default: generates via make)

set -Eeuo pipefail

: "${VERSION:?VERSION is required (e.g., 0.1.7)}"
: "${GH_TOKEN:?GH_TOKEN is required}"

FORK_OWNER="${FORK_OWNER:-attune-io}"
GIT_USER_NAME="${GIT_USER_NAME:-github-actions[bot]}"
GIT_USER_EMAIL="${GIT_USER_EMAIL:-41898282+github-actions[bot]@users.noreply.github.com}"
UPSTREAM_REPO="k8s-operatorhub/community-operators"
FORK_REPO="${FORK_OWNER}/community-operators"
BRANCH="attune-v${VERSION}"
OPERATOR_DIR="operators/attune"

# Save checkout root before any cd operations (OLDPWD is unreliable
# after multiple cd commands).
CHECKOUT_DIR="$(pwd)"

# Generate the OLM bundle if not pre-generated
BUNDLE_DIR="${BUNDLE_DIR:-dist/olm-bundle/${VERSION}}"
if [ ! -d "${BUNDLE_DIR}/manifests" ]; then
    echo "Generating OLM bundle..."
    make generate-olm-bundle VERSION="${VERSION}"
fi

# Verify bundle contents
for f in \
    "${BUNDLE_DIR}/manifests/attune-operator.clusterserviceversion.yaml" \
    "${BUNDLE_DIR}/manifests/attune.io_attunepolicies.yaml" \
    "${BUNDLE_DIR}/manifests/attune.io_attunedefaults.yaml" \
    "${BUNDLE_DIR}/manifests/attune.io_attunenamespacedefaults.yaml" \
    "${BUNDLE_DIR}/metadata/annotations.yaml"; do
    if [ ! -f "$f" ]; then
        echo "ERROR: Missing bundle file: $f" >&2
        exit 1
    fi
done
echo "Bundle verified: ${BUNDLE_DIR}"

# Clone the fork (shallow is fine here since we reset to upstream/main)
WORK_DIR=$(mktemp -d)
trap 'rm -rf "${WORK_DIR}"' EXIT

echo "Cloning fork ${FORK_REPO}..."
cd "${WORK_DIR}"
git clone --depth=1 "https://x-access-token:${GH_TOKEN}@github.com/${FORK_REPO}.git" community-operators
cd community-operators

git config user.name "${GIT_USER_NAME}"
git config user.email "${GIT_USER_EMAIL}"

# Reset to upstream main for a clean base
git remote add upstream "https://github.com/${UPSTREAM_REPO}.git"
git fetch upstream main --depth=1
git checkout -B "${BRANCH}" upstream/main

# Copy the bundle
mkdir -p "${OPERATOR_DIR}/${VERSION}/manifests" "${OPERATOR_DIR}/${VERSION}/metadata"
cp "${CHECKOUT_DIR}/${BUNDLE_DIR}/manifests/"* "${OPERATOR_DIR}/${VERSION}/manifests/"
cp "${CHECKOUT_DIR}/${BUNDLE_DIR}/metadata/"* "${OPERATOR_DIR}/${VERSION}/metadata/"

# Ensure ci.yaml exists at the operator root with reviewers for auto-merge
cat > "${OPERATOR_DIR}/ci.yaml" <<'EOF'
updateGraph: semver-mode
reviewers:
  - SebTardif
  - attune-release-bot[bot]
EOF

# Commit with DCO sign-off
git add "${OPERATOR_DIR}/"
git commit -s -m "operator attune (${VERSION})"

# Force-push (creates or updates the branch)
git push --force origin "${BRANCH}"
echo "Pushed branch ${BRANCH} to ${FORK_REPO}"

# Create or update the PR
PR_TITLE="operator [U] [CI] attune (${VERSION})"
PR_BODY="### New Submission

**Operator:** attune
**Version:** ${VERSION}

Update Attune operator to version ${VERSION}.

See [release notes](https://github.com/attune-io/attune/releases/tag/v${VERSION}) for changes.

---
*This PR was automatically created by the Attune release workflow.*"

# Check if a PR already exists for this branch
EXISTING_PR=$(gh pr list \
    --repo "${UPSTREAM_REPO}" \
    --head "${FORK_OWNER}:${BRANCH}" \
    --state open \
    --json number \
    --jq '.[0].number // empty' 2>/dev/null || true)

if [ -n "${EXISTING_PR}" ]; then
    echo "Updating existing PR #${EXISTING_PR}"
    gh pr edit "${EXISTING_PR}" \
        --repo "${UPSTREAM_REPO}" \
        --title "${PR_TITLE}" \
        --body "${PR_BODY}"
    echo "PR updated: https://github.com/${UPSTREAM_REPO}/pull/${EXISTING_PR}"
else
    echo "Creating new PR..."
    PR_URL=$(gh pr create \
        --repo "${UPSTREAM_REPO}" \
        --head "${FORK_OWNER}:${BRANCH}" \
        --base main \
        --title "${PR_TITLE}" \
        --body "${PR_BODY}")
    echo "PR created: ${PR_URL}"
fi
