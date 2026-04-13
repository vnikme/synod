#!/bin/bash
# Builds and pushes container images to Artifact Registry.
# Run BEFORE 02_deploy.sh if you prefer pre-built images.
#
# Usage:
#   ./01_build.sh              # Build both images
#   ./01_build.sh sandbox      # Build sandbox only
#   ./01_build.sh orchestrator # Build orchestrator only

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

PROJECT_ID="synod-493123"
REGION="us-central1"
REPO_NAME="synod"
REGISTRY="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPO_NAME}"

TARGET="${1:-all}"

# --- Ensure Artifact Registry repo exists ---
ensure_registry() {
    echo "=== Ensuring Artifact Registry repo: ${REPO_NAME} ==="
    gcloud artifacts repositories create "$REPO_NAME" \
        --repository-format=docker \
        --location="$REGION" \
        --project="$PROJECT_ID" 2>/dev/null || true
    gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet
}

# --- Build & push ---
build_sandbox() {
    echo "=== Building sandbox image ==="
    docker build -t "${REGISTRY}/sandbox:latest" "$REPO_ROOT/sandbox/"
    docker push "${REGISTRY}/sandbox:latest"
    echo "✅ sandbox pushed to ${REGISTRY}/sandbox:latest"
}

build_orchestrator() {
    echo "=== Building orchestrator image ==="
    docker build -t "${REGISTRY}/orchestrator:latest" "$REPO_ROOT/orchestrator/"
    docker push "${REGISTRY}/orchestrator:latest"
    echo "✅ orchestrator pushed to ${REGISTRY}/orchestrator:latest"
}

# --- Main ---
ensure_registry

case "$TARGET" in
    sandbox)       build_sandbox ;;
    orchestrator)  build_orchestrator ;;
    all)           build_sandbox; build_orchestrator ;;
    *)
        echo "Usage: $0 [sandbox|orchestrator|all]"
        exit 1
        ;;
esac

echo ""
echo "Images ready in ${REGISTRY}/"
echo "Deploy with: ./02_deploy.sh"
