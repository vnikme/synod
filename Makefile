GCP_PROJECT_ID ?= $(shell gcloud config get-value project)
REGION ?= us-central1
ORCHESTRATOR_SA ?= $(shell gcloud projects describe $(GCP_PROJECT_ID) --format='value(projectNumber)')-compute@developer.gserviceaccount.com

.PHONY: setup-infra deploy-orchestrator deploy-sandbox deploy-all setup-iam

setup-infra:
	@echo "==> Creating Firestore database..."
	gcloud firestore databases create --location=$(REGION) --project=$(GCP_PROJECT_ID) 2>/dev/null || true
	@echo "==> Creating Cloud Tasks queue..."
	gcloud tasks queues create synod-queue --location=$(REGION) --project=$(GCP_PROJECT_ID) 2>/dev/null || true
	@echo "==> Infrastructure ready."

deploy-orchestrator:
	@echo "==> Deploying orchestrator to Cloud Run..."
	gcloud run deploy orchestrator \
		--source=orchestrator/ \
		--region=$(REGION) \
		--project=$(GCP_PROJECT_ID) \
		--allow-unauthenticated \
		--memory=512Mi \
		--timeout=300 \
		--set-env-vars="GCP_PROJECT_ID=$(GCP_PROJECT_ID),CLOUD_TASKS_LOCATION=$(REGION),CLOUD_TASKS_QUEUE=synod-queue"
	@echo "==> Granting Cloud Tasks invoker role on orchestrator..."
	gcloud run services add-iam-policy-binding orchestrator \
		--region=$(REGION) \
		--member="serviceAccount:$(ORCHESTRATOR_SA)" \
		--role="roles/run.invoker" \
		--project=$(GCP_PROJECT_ID)

deploy-sandbox:
	@echo "==> Deploying sandbox to Cloud Run..."
	gcloud run deploy sandbox \
		--source=sandbox/ \
		--region=$(REGION) \
		--project=$(GCP_PROJECT_ID) \
		--no-allow-unauthenticated \
		--memory=1Gi \
		--timeout=60 \
		--set-env-vars="GCP_PROJECT_ID=$(GCP_PROJECT_ID)"
	@echo "==> Granting invoker role on sandbox..."
	gcloud run services add-iam-policy-binding sandbox \
		--region=$(REGION) \
		--member="serviceAccount:$(ORCHESTRATOR_SA)" \
		--role="roles/run.invoker" \
		--project=$(GCP_PROJECT_ID)

deploy-all: setup-infra deploy-sandbox deploy-orchestrator setup-iam
	@echo "==> All services deployed."
	@echo ""
	@echo "Next steps:"
	@echo "1. Set env vars on orchestrator: GEMINI_API_KEY, ORCHESTRATOR_BASE_URL, SANDBOX_URL, SERVICE_ACCOUNT_EMAIL"
	@echo "2. Optionally set: GOOGLE_CSE_API_KEY, GOOGLE_CSE_CX, SEC_EDGAR_USER_AGENT, LLM_MODEL"
	@echo "3. Test: curl -X POST <orchestrator-url>/api/v1/tasks -d '{\"prompt\":\"...\"}'"

setup-iam:
	@echo "==> Configuring IAM bindings..."
	gcloud run services add-iam-policy-binding orchestrator \
		--region=$(REGION) \
		--member="serviceAccount:$(ORCHESTRATOR_SA)" \
		--role="roles/run.invoker" \
		--project=$(GCP_PROJECT_ID)
	gcloud run services add-iam-policy-binding sandbox \
		--region=$(REGION) \
		--member="serviceAccount:$(ORCHESTRATOR_SA)" \
		--role="roles/run.invoker" \
		--project=$(GCP_PROJECT_ID)
	@echo "==> IAM bindings configured."
