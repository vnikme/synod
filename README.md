# Project Synod: Multi-Agent Task Solver

![Synod Architecture Overview](https://img.shields.io/badge/Architecture-Polyglot%20Microservices-blue)
![State Model](https://img.shields.io/badge/State%20Model-Blackboard%20(Pull--Based)-brightgreen)
![GCP Native](https://img.shields.io/badge/Infrastructure-GCP%20Serverless-orange)

An asynchronous, event-driven Multi-Agent Task Solver deployed entirely on Google Cloud Platform.
**Prepared for the wand.ai Take-Home Assignment.**

**Live:** [https://synod.ai.church](https://synod.ai.church)

## 1. System Overview

Project Synod accepts high-level, plain-language business intelligence requests (e.g., *"Summarize Tesla's 10-K risk factors"* or *"Compare Apple and Microsoft revenue and draw a chart"*) and orchestrates a fleet of specialized LLM agents to synthesize a factual, well-reasoned response.

To handle long-running, non-deterministic agent executions without encountering API Gateway or HTTP timeouts, the system employs an **Asynchronous Event-Driven Architecture**. The ingestion API immediately responds with an `HTTP 202 Accepted`, delegating the heavy lifting to ephemeral Cloud Run workers via **Cloud Tasks**.

### Key Architectural Decisions
*   **Polyglot:** **Go** handles strict orchestration, HTTP routing, and concurrency. **Python** executes data science workloads (pandas, matplotlib) in isolated, sandboxed containers.
*   **No "Bloatware" Frameworks:** We explicitly avoided abstraction-heavy frameworks like LangChain or LangGraph. State management and orchestration are custom-built in Go to demonstrate a deep, engineering-first understanding of LLM control flow, fault tolerance, and defensive AI.
*   **Dynamic Context Resolution (The "Pull" Model):** Unlike rigid DAG pipelines where Agent A blindly pushes data to Agent B, agents actively query the shared state. If the `Analyst` lacks data to draw a chart, it pauses, signals `NEEDS_CONTEXT` with missing queries, and the Orchestrator dynamically routes a sub-task to the `Data Agent` before resuming the Analyst.

## 2. Infrastructure & Scale (GCP Native)

The application is built for enterprise scale using serverless infrastructure:
1.  **Core Orchestrator (Go / Cloud Run):** API Gateway, State Machine, and embedded Web UI.
2.  **Worker Fleet (Python / Cloud Run):** Ephemeral, sandboxed code execution environment (non-root).
3.  **The Blackboard (Firestore):** A central `JobState` document containing `CollectedFacts`, `GeneratedAssets`, and conversational `chat_history`.
4.  **Message Broker (Cloud Tasks):** Decouples orchestration from execution. Rate-limited (10/s, 5 concurrent), bounded retries (5 max attempts, 5s–60s backoff).

## 3. "Defensive AI" Mechanisms

Building with LLMs requires anticipating failure. The system implements:
*   **Sandboxed Code Execution:** The `Analyst` agent dynamically generates Python code. Before execution, the code is parsed via an Abstract Syntax Tree (AST) to block malicious imports (`os`, `sys`, `subprocess`). Execution runs as a non-root user with restricted builtins and a strict OS-level timeout.
*   **Self-Correction Loops:** If a Python worker returns malformed JSON, or if the generated Python script throws a `Traceback`, the worker catches the exception and feeds the error back into the LLM prompt for self-correction (max 3 retries) before failing.
*   **Infinite Loop Circuit Breaker:** The Go router tracks `hop_count`. If agents bounce context requests back and forth indefinitely (`HopCount > 15`), execution is halted.
*   **Human-in-the-Loop (HITL):** For ambiguous requests, the Orchestrator transitions to a `HITL` state, pausing the graph and prompting the user for clarification via the web UI before resuming.
*   **Explicit Orchestration Boundaries:** The Go orchestrator is structured around clear state transitions and component boundaries to keep routing behavior predictable and easier to validate as the system evolves.
*   **Request Body Limits:** Public endpoints enforce 1 MB max body size to prevent memory exhaustion.

## 4. Web UI

The orchestrator embeds a single-page web UI (Tailwind CSS + marked.js) served at the root path. Features:
*   Chat-style interface for task submission and conversational interaction
*   Real-time status polling (active agent, progress)
*   HITL reply — the UI detects when the system needs clarification and switches to reply mode
*   Markdown report rendering with DOMPurify sanitization
*   Inline chart display (base64 PNG assets)
*   Session persistence via sessionStorage (per-tab, cleared on browser close)

## 5. API Usage

### 5.1 Ingestion
```bash
curl -X POST https://synod.ai.church/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{"prompt": "Compare Apple and Microsoft revenue and draw a chart."}'

# Response: HTTP 202 Accepted
# {"job_id": "uuid", "session_id": "uuid"}
```

### 5.2 Status Polling
```bash
curl "https://synod.ai.church/api/v1/tasks/{job_id}?session_id={session_id}"

# Response: HTTP 200 OK
# Returns the entire Blackboard state, including "status", "active_agent", and "final_result".
```

### 5.3 HITL Reply
```bash
curl -X POST "https://synod.ai.church/api/v1/tasks/{job_id}/reply" \
  -H "Content-Type: application/json" \
  -d '{"session_id": "uuid", "message": "Focus on Q3 2024 specifically."}'

# Response: HTTP 202 Accepted — resumes orchestration asynchronously.
```

## 6. Local Setup & Deployment

### Prerequisites
*   Google Cloud CLI (`gcloud`)
*   Go 1.23+ and Python 3.11+
*   A GCP Project with billing enabled

### Deployment
1.  Initialize GCP infrastructure (Firestore, Cloud Tasks, Service Accounts, IAM):
    ```bash
    bash deploy/gcp/00_setup.gcp.sh
    ```
2.  Create `deploy/gcp/.env` from the example and fill in secrets:
    ```bash
    cp deploy/gcp/.env.example deploy/gcp/.env
    # Edit: GEMINI_API_KEY, SEC_EDGAR_USER_AGENT
    ```
3.  Deploy both services (sandbox first, then orchestrator):
    ```bash
    bash deploy/gcp/02_deploy.sh
    ```
    Or individually:
    ```bash
    bash deploy/gcp/02_deploy.sh sandbox
    bash deploy/gcp/02_deploy.sh orchestrator
    ```
4.  Visit the orchestrator's Cloud Run URL (printed after deploy) to use the web UI.

### Makefile (convenience wrapper)
```bash
make setup              # Run 00_setup.gcp.sh
make build              # Build docker images to Artifact Registry
make deploy-all         # Deploy both services
make deploy-orchestrator
make deploy-sandbox
make test               # Run Go unit tests (vet + test)
```

## 7. Troubleshooting

### Stuck Jobs ("IN_PROGRESS" with No Activity)

If a job shows a status like `"ANALYST in progress"` indefinitely with no Cloud Tasks activity, the job is stuck in `IN_PROGRESS` state. This can happen if a transient Firestore error occurs during the post-execution callback (between agent completion and orchestrator re-enqueue).

**Symptoms:**
- UI shows an agent "in progress" for more than 5 minutes
- No pending Cloud Tasks for the job
- GCP logs show `CRITICAL` entries mentioning "manual intervention required"

**Recovery (Firestore Console):**
1. Open the [Firestore Console](https://console.cloud.google.com/firestore) for your project.
2. Navigate to the job document under `sessions/{session_id}/jobs/{job_id}`.
3. Set `status` to `"QUEUED"` and `active_agent` to `"orchestrator"`.
4. Re-enqueue by sending a POST to `/internal/route` with `{"job_id": "...", "session_id": "..."}`, or wait for the next user interaction to trigger a new job.

**Root Cause (Fixed):** Previously, `handleAgentExec` could return HTTP 500 after a successful agent execution if the Firestore re-read failed. This triggered a Cloud Tasks retry, but the retry's `ClaimQueuedJob` saw `IN_PROGRESS` (not `QUEUED`) and silently ACKed as a stale delivery — leaving the job permanently stuck. The fix ensures HTTP 200 is always returned after a successful claim (the "point of no return" pattern), with `CRITICAL`-level logging for any post-claim failures that require operator attention.

### Key Log Messages

| Log Level | Message Pattern | Meaning |
|-----------|----------------|---------|
| `CRITICAL` | `failed to mark job as FAILED` | Agent failed AND the status update to FAILED also failed. Job stuck IN_PROGRESS. |
| `CRITICAL` | `callback transition failed` | Agent succeeded but the QUEUED+orchestrator transition failed. Job stuck IN_PROGRESS. |
| `CRITICAL` | `enqueue callback failed` | Agent succeeded, job is QUEUED+orchestrator, but no Cloud Task was created. Job is recoverable but stalled. |
| `WARN` | `stale/duplicate delivery` | A Cloud Tasks retry arrived for an already-claimed job. Usually benign (at-least-once delivery). |

## 8. Trade-offs (24-Hour Constraint)
*   **Authentication:** JWT verification and strict IDOR prevention are omitted for this prototype. However, strict logical session isolation (via `session_id`) and service-to-service OIDC auth are implemented.
*   **Database:** Firestore was chosen for rapid prototyping over PostgreSQL (JSONB). In a mature production environment, Postgres would provide stronger transactional guarantees.
*   **Secrets Management:** API keys are passed as plain environment variables. In production, GCP Secret Manager with `--set-secrets` should be used.
*   **Data Sources:** The `Data Agent` uses Gemini's built-in Google Search grounding and the SEC EDGAR API. In production, a commercial financial data API would improve extraction reliability.
