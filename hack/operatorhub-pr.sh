#!/usr/bin/env bash
# Create or update PRs to OperatorHub repos for a new Attune release.
# Submits to both k8s-operatorhub/community-operators (operatorhub.io)
# and redhat-openshift-ecosystem/community-operators-prod (OpenShift catalog).
#
# Usage: VERSION=0.1.7 GH_TOKEN=<token> hack/operatorhub-pr.sh
#
# Required env vars:
#   VERSION         - Release version without 'v' prefix (e.g., 0.1.7)
#   GH_TOKEN        - GitHub App token for pushing to the fork repos
#
# Optional env vars:
#   UPSTREAM_GH_TOKEN - PAT with public_repo scope for creating PRs on
#                       upstream repos. Required because GitHub App tokens
#                       can only act on repos where the app is installed.
#                       Falls back to GH_TOKEN if not set.
#   FORK_OWNER      - GitHub org owning the forks (default: attune-io)
#   GIT_USER_NAME   - Git commit author name (default: github-actions[bot])
#   GIT_USER_EMAIL  - Git commit author email (default: 41898282+github-actions[bot]@users.noreply.github.com)
#   BUNDLE_DIR      - Pre-generated bundle path (default: generates via make)

set -Eeuo pipefail

: "${VERSION:?VERSION is required (e.g., 0.1.7)}"
: "${GH_TOKEN:?GH_TOKEN is required}"
: "${IMAGE_DIGEST:?IMAGE_DIGEST is required (e.g., sha256:abc...)}"

FORK_OWNER="${FORK_OWNER:-attune-io}"
GIT_USER_NAME="${GIT_USER_NAME:-github-actions[bot]}"
GIT_USER_EMAIL="${GIT_USER_EMAIL:-41898282+github-actions[bot]@users.noreply.github.com}"
BRANCH="attune-v${VERSION}"
OPERATOR_DIR="operators/attune"

# Save checkout root before any cd operations.
CHECKOUT_DIR="$(pwd)"

# Collect temp dirs for cleanup.
CLEANUP_DIRS=()
cleanup() { for d in "${CLEANUP_DIRS[@]}"; do rm -rf "$d"; done; }
trap cleanup EXIT

# Generate the OLM bundle if not pre-generated
BUNDLE_DIR="${BUNDLE_DIR:-dist/olm-bundle/${VERSION}}"
if [ ! -d "${BUNDLE_DIR}/manifests" ]; then
    echo "Generating OLM bundle..."
    make generate-olm-bundle VERSION="${VERSION}" IMAGE_DIGEST="${IMAGE_DIGEST}"
fi

# Verify bundle contents
for f in \
    "${BUNDLE_DIR}/manifests/attune.clusterserviceversion.yaml" \
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

# ── Submit bundle to one upstream repo ──────────────────────────────
# Args: upstream_repo [openshift_versions]
#   upstream_repo      - e.g. k8s-operatorhub/community-operators
#   openshift_versions - e.g. v4.19 (only for community-operators-prod)
submit_bundle() {
    local upstream_repo="$1"
    local openshift_versions="${2:-}"
    local repo_name="${upstream_repo##*/}"
    local fork_repo="${FORK_OWNER}/${repo_name}"

    echo ""
    echo "=== Submitting to ${upstream_repo} ==="

    local work_dir
    work_dir=$(mktemp -d)
    CLEANUP_DIRS+=("${work_dir}")

    echo "Cloning fork ${fork_repo}..."
    git clone --depth=1 \
        "https://x-access-token:${GH_TOKEN}@github.com/${fork_repo}.git" \
        "${work_dir}/${repo_name}"
    cd "${work_dir}/${repo_name}"

    git config user.name "${GIT_USER_NAME}"
    git config user.email "${GIT_USER_EMAIL}"

    # Reset to upstream main for a clean base
    git remote add upstream "https://github.com/${upstream_repo}.git"
    git fetch upstream main --depth=1
    git checkout -B "${BRANCH}" upstream/main

    # Copy the bundle
    mkdir -p "${OPERATOR_DIR}/${VERSION}/manifests" "${OPERATOR_DIR}/${VERSION}/metadata"
    cp "${CHECKOUT_DIR}/${BUNDLE_DIR}/manifests/"* "${OPERATOR_DIR}/${VERSION}/manifests/"
    cp "${CHECKOUT_DIR}/${BUNDLE_DIR}/metadata/"* "${OPERATOR_DIR}/${VERSION}/metadata/"

    # Inject OpenShift version annotation if specified
    if [ -n "${openshift_versions}" ]; then
        sed -i '/^annotations:/a\  com.redhat.openshift.versions: "'"${openshift_versions}"'"' \
            "${OPERATOR_DIR}/${VERSION}/metadata/annotations.yaml"
    fi

    # Ensure ci.yaml exists at the operator root with reviewers
    cat > "${OPERATOR_DIR}/ci.yaml" <<'CIEOF'
updateGraph: semver-mode
reviewers:
  - SebTardif
  - attune-release-bot[bot]
CIEOF

    # Commit with DCO sign-off
    git add "${OPERATOR_DIR}/"
    git commit -s -m "operator attune (${VERSION})"

    # Force-push (creates or updates the branch)
    git push --force origin "${BRANCH}"
    echo "Pushed branch ${BRANCH} to ${fork_repo}"

    # Detect new vs update: check if operator dir exists on upstream/main
    local pr_tag="[U]"
    if ! git ls-tree --name-only upstream/main -- "${OPERATOR_DIR}" | grep -q .; then
        pr_tag="[N]"
    fi

    local pr_title="operator ${pr_tag} [CI] attune (${VERSION})"
    local pr_body
    pr_body="### New Submission

**Operator:** attune
**Version:** ${VERSION}

Update Attune operator to version ${VERSION}.

See [release notes](https://github.com/attune-io/attune/releases/tag/v${VERSION}) for changes.

---
*This PR was automatically created by the Attune release workflow.*"

    # Switch to a token that can create PRs on the upstream public repo.
    local pr_token="${UPSTREAM_GH_TOKEN:-${GH_TOKEN}}"

    # Check if a PR already exists for this branch
    local existing_pr
    existing_pr=$(GH_TOKEN="${pr_token}" gh pr list \
        --repo "${upstream_repo}" \
        --head "${FORK_OWNER}:${BRANCH}" \
        --state open \
        --json number \
        --jq '.[0].number // empty' 2>/dev/null || true)

    if [ -n "${existing_pr}" ]; then
        echo "Updating existing PR #${existing_pr}"
        GH_TOKEN="${pr_token}" gh pr edit "${existing_pr}" \
            --repo "${upstream_repo}" \
            --title "${pr_title}" \
            --body "${pr_body}"
        echo "PR updated: https://github.com/${upstream_repo}/pull/${existing_pr}"
    else
        echo "Creating new PR..."
        local pr_url
        pr_url=$(GH_TOKEN="${pr_token}" gh pr create \
            --repo "${upstream_repo}" \
            --head "${FORK_OWNER}:${BRANCH}" \
            --base main \
            --title "${pr_title}" \
            --body "${pr_body}")
        echo "PR created: ${pr_url}"
    fi

    cd "${CHECKOUT_DIR}"
}

# ── Submit to both OperatorHub repos ────────────────────────────────
submit_bundle "k8s-operatorhub/community-operators"
submit_bundle "redhat-openshift-ecosystem/community-operators-prod" "v4.19"
