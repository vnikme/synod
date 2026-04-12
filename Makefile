# Project Synod — Makefile
# Usage: make deploy-all GCP_PROJECT_ID=your-project REGION=us-central1

GCP_PROJECT_ID ?= $(shell gcloud config get-value project)
REGION ?= us-central1
QUEUE_NAME ?= synod-queue
ORCHESTRATOR_SA ?= synod-invoker@$(GCP_PROJECT_ID).iam.gserviceaccount.com

.PHONY: all setup-infra deploy-orchestrator deploy-workers deploy-all

all: deploy-all

# --- Infrastructure ---

setup-infra:
	@echo "==> Creating Firestore database..."
	-gcloud firestore databases create --location=$(REGION) --project=$(GCP_PROJECT_ID)
	@echo "==> Creating Cloud Tasks queue..."
	-gcloud tasks queues create $(QUEUE_NAME) --location=$(REGION) --project=$(GCP_PROJECT_ID)
	@echo "==> Creating service account..."
	-gcloud iam service-accounts create synod-invoker \
		--display-name="Synod Cloud Tasks Invoker" \
		--project=$(GCP_PROJECT_ID)
	@echo "==> Infrastructure ready."

# --- Deploy ---

deploy-orchestrator:
	@echo "==> Deploying orchestrator..."
	gcloud run deploy orchestrator \
		--source=orchestrator/ \
		--region=$(REGION) \
		--allow-unauthenticated \
		--set-env-vars="GCP_PROJECT_ID=$(GCP_PROJECT_ID),CLOUD_TASKS_LOCATION=$(REGION),CLOUD_TASKS_QUEUE=$(QUEUE_NAME),SERVICE_ACCOUNT_EMAIL=$(ORCHESTRATOR_SA)" \
		--project=$(GCP_PROJECT_ID)
	@echo "==> Granting Cloud Tasks invoker role on orchestrator..."
	$(eval ORCH_URL := $(shell gcloud run services describe orchestrator --region=$(REGION) --format='value(status.url)' --project=$(GCP_PROJECT_ID)))
	@echo "Orchestrator URL: $(ORCH_URL)"

deploy-workers:
	@echo "==> Deploying workers..."
	gcloud run deploy workers \
		--source=workers/ \
		--region=$(REGION) \
		--no-allow-unauthenticated \
		--set-env-vars="GCP_PROJECT_ID=$(GCP_PROJECT_ID),CLOUD_TASKS_LOCATION=$(REGION),CLOUD_TASKS_QUEUE=$(QUEUE_NAME),SERVICE_ACCOUNT_EMAIL=$(ORCHESTRATOR_SA)" \
		--memory=1Gi \
		--timeout=300 \
		--project=$(GCP_PROJECT_ID)
	@echo "==> Granting invoker role to service account on workers..."
	gcloud run services add-iam-policy-binding workers \
		--region=$(REGION) \
		--member="serviceAccount:$(ORCHESTRATOR_SA)" \
		--role="roles/run.invoker" \
		--project=$(GCP_PROJECT_ID)
	$(eval WORKER_URL := $(shell gcloud run services describe workers --region=$(REGION) --format='value(status.url)' --project=$(GCP_PROJECT_ID)))
	@echo "Workers URL: $(WORKER_URL)"

deploy-all: setup-infra deploy-orchestrator deploy-workers
	@echo ""
	@echo "==> Deployment complete. Now set environment variables:"
	@echo ""
	@echo "Run these commands to wire the services together:"
	@echo ""
	$(eval ORCH_URL := $(shell gcloud run services describe orchestrator --region=$(REGION) --format='value(status.url)' --project=$(GCP_PROJECT_ID)))
	$(eval WORKER_URL := $(shell gcloud run services describe workers --region=$(REGION) --format='value(status.url)' --project=$(GCP_PROJECT_ID)))
	@echo "  gcloud run services update orchestrator --region=$(REGION) --update-env-vars=WORKER_BASE_URL=$(WORKER_URL),ORCHESTRATOR_BASE_URL=$(ORCH_URL)"
	@echo "  gcloud run services update workers --region=$(REGION) --update-env-vars=ORCHESTRATOR_BASE_URL=$(ORCH_URL)"
	@echo ""
	@echo "Then set secrets (GEMINI_API_KEY, GOOGLE_CSE_API_KEY, GOOGLE_CSE_CX, SEC_EDGAR_USER_AGENT) on both services."

# --- Grant orchestrator IAM for Cloud Tasks on itself ---

setup-iam:
	gcloud run services add-iam-policy-binding orchestrator \
		--region=$(REGION) \
		--member="serviceAccount:$(ORCHESTRATOR_SA)" \
		--role="roles/run.invoker" \
		--project=$(GCP_PROJECT_ID)
	gcloud run services add-iam-policy-binding workers \
		--region=$(REGION) \
		--member="serviceAccount:$(ORCHESTRATOR_SA)" \
		--role="roles/run.invoker" \
		--project=$(GCP_PROJECT_ID)
	gcloud projects add-iam-policy-binding $(GCP_PROJECT_ID) \
		--member="serviceAccount:$(ORCHESTRATOR_SA)" \
		--role="roles/cloudtasks.enqueuer"
