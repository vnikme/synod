#!/bin/bash
# GCP Initialization Script for Project Synod
# IMPORTANT: Run this ONLY ONCE per project.

set -e # Exit immediately if a command exits with a non-zero status.

# --- Variables ---
PROJECT_ID="synod-493123"
REGION="us-central1"
QUEUE_NAME="synod-tasks"
SA_NAME="synod-orchestrator"
SA_EMAIL="${SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"

echo "=== 1. Setting GCP Project to: ${PROJECT_ID} ==="
gcloud config set project $PROJECT_ID

echo "=== 2. Enabling necessary GCP APIs ==="
gcloud services enable \
    run.googleapis.com \
    cloudtasks.googleapis.com \
    firestore.googleapis.com \
    secretmanager.googleapis.com \
    --project=$PROJECT_ID

echo "=== 3. Initializing Firestore Database (Native Mode) ==="
# Note: If Firestore is already initialized, this might throw an error. We use || true to proceed.
gcloud firestore databases create \
    --location=$REGION \
    --type=firestore-native \
    --project=$PROJECT_ID || true

echo "=== 4. Creating Cloud Tasks Queue ==="
gcloud tasks queues create $QUEUE_NAME \
    --location=$REGION \
    --project=$PROJECT_ID || true

echo "=== 5. Creating Dedicated Service Account ==="
gcloud iam service-accounts create $SA_NAME \
    --display-name="Synod Orchestrator SA" \
    --project=$PROJECT_ID || true

echo "=== 6. Binding IAM Roles to Service Account ==="
# Role 1: Firestore access
gcloud projects add-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:${SA_EMAIL}" \
    --role="roles/datastore.user"

# Role 2: Cloud Tasks enqueuer
gcloud projects add-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:${SA_EMAIL}" \
    --role="roles/cloudtasks.enqueuer"

# Role 3: Cloud Run invoker (internal webhooks)
gcloud projects add-iam-policy-binding $PROJECT_ID \
    --member="serviceAccount:${SA_EMAIL}" \
    --role="roles/run.invoker"

echo "✅ GCP Initialization Complete for ${PROJECT_ID}!"
