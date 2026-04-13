#!/bin/bash
# Deploys Synod services to Cloud Run.
# Prerequisites: 00_setup.gcp.sh completed, .env populated (see .env.example).
#
# Usage:
#   ./02_deploy.sh              # Deploy both services (source build)
#   ./02_deploy.sh sandbox      # Deploy sandbox only
#   ./02_deploy.sh orchestrator # Deploy orchestrator only
#
# If you ran 01_build.sh first, images from Artifact Registry are used
# automatically via --image. Otherwise, --source triggers Cloud Build.

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# --- Load .env ---
ENV_FILE="${SCRIPT_DIR}/.env"
if [ ! -f "$ENV_FILE" ]; then
    echo "❌ $ENV_FILE not found. Copy .env.example → .env and fill in values."
    exit 1
fi
set -a; source "$ENV_FILE"; set +a

# --- Variables (must match 00_setup.gcp.sh) ---
PROJECT_ID="synod-493123"
REGION="us-central1"
QUEUE_NAME="synod-tasks"
SA_EMAIL="synod-orchestrator@${PROJECT_ID}.iam.gserviceaccount.com"
SANDBOX_SA_EMAIL="synod-sandbox@${PROJECT_ID}.iam.gserviceaccount.com"
DOMAIN="synod.ai.church"
REGISTRY="${REGION}-docker.pkg.dev/${PROJECT_ID}/synod"

# What to deploy
TARGET="${1:-all}"

# --- Helpers ---
get_service_url() {
    gcloud run services describe "$1" \
        --region="$REGION" --project="$PROJECT_ID" \
        --format='value(status.url)' 2>/dev/null || echo ""
}

# Use pre-built image if it exists in Artifact Registry, else fall back to --source.
image_or_source() {
    local svc="$1"
    if gcloud artifacts docker images describe "${REGISTRY}/${svc}:latest" \
        --project="$PROJECT_ID" &>/dev/null; then
        echo "--image=${REGISTRY}/${svc}:latest"
    else
        echo "--source=$REPO_ROOT/${svc}/"
    fi
}

# ========== SANDBOX ==========
deploy_sandbox() {
    local SRC
    SRC=$(image_or_source sandbox)
    echo "=== Deploying sandbox to Cloud Run ($SRC) ==="
    gcloud run deploy sandbox \
        $SRC \
        --region="$REGION" \
        --project="$PROJECT_ID" \
        --service-account="$SANDBOX_SA_EMAIL" \
        --no-allow-unauthenticated \
        --memory=1Gi \
        --cpu=1 \
        --timeout=60 \
        --max-instances=5 \
        --set-env-vars="GCP_PROJECT_ID=$PROJECT_ID"

    echo "✅ Sandbox deployed."
}

# ========== ORCHESTRATOR ==========
deploy_orchestrator() {
    # Auto-detect sandbox URL
    SANDBOX_URL=$(get_service_url sandbox)
    if [ -z "$SANDBOX_URL" ]; then
        echo "❌ Sandbox service not found. Deploy sandbox first."
        exit 1
    fi

    # Use the Cloud Run service URL for internal callbacks (Cloud Tasks).
    # The custom domain is for external clients only and may not be active yet.
    ORCHESTRATOR_URL=$(get_service_url orchestrator)
    if [ -z "$ORCHESTRATOR_URL" ]; then
        # First deploy — will update after service is created.
        ORCHESTRATOR_URL="https://$DOMAIN"
    fi

    local SRC
    SRC=$(image_or_source orchestrator)
    echo "=== Deploying orchestrator to Cloud Run ($SRC) ==="

    # Secrets are passed as plain env vars from .env.
    # TODO: migrate to GCP Secret Manager (--set-secrets) for production.
    gcloud run deploy orchestrator \
        $SRC \
        --region="$REGION" \
        --project="$PROJECT_ID" \
        --service-account="$SA_EMAIL" \
        --allow-unauthenticated \
        --memory=512Mi \
        --cpu=1 \
        --timeout=300 \
        --max-instances=10 \
        --set-env-vars="\
GCP_PROJECT_ID=$PROJECT_ID,\
CLOUD_TASKS_LOCATION=$REGION,\
CLOUD_TASKS_QUEUE=$QUEUE_NAME,\
SERVICE_ACCOUNT_EMAIL=$SA_EMAIL,\
ORCHESTRATOR_BASE_URL=$ORCHESTRATOR_URL,\
SANDBOX_URL=$SANDBOX_URL,\
LLM_MODEL=gemini-2.0-flash,\
GEMINI_API_KEY=$GEMINI_API_KEY,\
GOOGLE_CSE_API_KEY=${GOOGLE_CSE_API_KEY:-},\
GOOGLE_CSE_CX=${GOOGLE_CSE_CX:-},\
SEC_EDGAR_USER_AGENT=${SEC_EDGAR_USER_AGENT:-}"

    echo "✅ Orchestrator deployed."
}

# ========== DOMAIN MAPPING ==========
setup_domain() {
    echo "=== Mapping custom domain: $DOMAIN ==="
    gcloud run domain-mappings create \
        --service=orchestrator \
        --domain="$DOMAIN" \
        --region="$REGION" \
        --project="$PROJECT_ID" 2>/dev/null || true

    echo ""
    echo "=== DNS Configuration Required ==="
    echo "Add the following DNS records for $DOMAIN:"
    gcloud run domain-mappings describe \
        --domain="$DOMAIN" \
        --region="$REGION" \
        --project="$PROJECT_ID" \
        --format='table(resourceRecords.type, resourceRecords.name, resourceRecords.rrdata)' 2>/dev/null || \
        echo "  (Run this script again after deployment to see DNS records)"
    echo ""
}

# ========== IAM ==========
setup_iam() {
    echo "=== Configuring IAM for internal invocation ==="
    # Orchestrator SA can invoke itself (Cloud Tasks callbacks) and sandbox
    for SVC in orchestrator sandbox; do
        if gcloud run services describe "$SVC" \
            --region="$REGION" \
            --project="$PROJECT_ID" >/dev/null 2>&1; then
            gcloud run services add-iam-policy-binding "$SVC" \
                --region="$REGION" \
                --member="serviceAccount:${SA_EMAIL}" \
                --role="roles/run.invoker" \
                --project="$PROJECT_ID" --quiet
        else
            echo "ℹ️  Skipping IAM binding for $SVC: service not yet deployed."
        fi
    done
    echo "✅ IAM bindings configured."
}

# ========== MAIN ==========
case "$TARGET" in
    sandbox)
        deploy_sandbox
        setup_iam
        ;;
    orchestrator)
        deploy_orchestrator
        setup_iam
        setup_domain
        ;;
    all)
        deploy_sandbox
        deploy_orchestrator
        setup_iam
        setup_domain
        ;;
    *)
        echo "Usage: $0 [sandbox|orchestrator|all]"
        exit 1
        ;;
esac

echo ""
echo "========================================="
echo " Synod deployment complete!"
echo " Public API: https://$DOMAIN"
echo "========================================="
echo ""
echo "Quick test:"
echo "  curl -s https://$DOMAIN/health"
echo ""
echo "Submit a task:"
echo "  curl -X POST https://$DOMAIN/api/v1/tasks \\"
echo "    -H 'Content-Type: application/json' \\"
echo "    -d '{\"prompt\": \"Compare Apple and Microsoft revenue growth over the last 3 years\"}'"
