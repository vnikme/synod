# System Architecture Document: Project Synod
**Deployment Target:** `synod.ai.church`
**Role:** Principal/Staff Software Engineer
**Date:** April 2026

---

## 1. Executive Summary

Project Synod is a production-grade, asynchronous Multi-Agent Task Solver designed to process complex business intelligence queries. The system mitigates the inherent non-determinism of Large Language Models (LLMs) through strict state management, defensive code execution, and dynamic context resolution.

The architecture employs a **Polyglot Microservices Model** deployed on Google Cloud Platform (GCP). It leverages Go for deterministic orchestration and concurrency, and Python for specialized data science workloads. Inter-service communication is handled asynchronously via Cloud Tasks to decouple ingestion from execution, preventing synchronous HTTP timeouts.

---

## 2. System Analysis & Operational Parameters

### 2.1. Target Audience & Business Value
*   **Users:** Enterprise analysts and technical program managers requiring multi-source data synthesis (e.g., SEC filings, market data).
*   **Value Proposition:** Automating workflows that require both qualitative research (document summarization) and quantitative analysis (code execution and charting), reducing Time-to-Resolution (TTR) from hours to under 3 minutes.

### 2.2. Service Level Objectives (SLOs) & Metrics
*   **Availability:** 99.9% uptime for the API ingestion layer.
*   **Task Success Rate:** >95% resolution without triggering infinite loop circuit breakers.
*   **Latency Target:** End-to-end execution between 30 and 180 seconds, dependent on the required agent depth.
*   **Cost Efficiency:** Maximum $0.15 compute/LLM cost per invocation, achieved via model tiering (e.g., Haiku/GPT-4o-mini for extraction, Opus/GPT-4o for orchestration).

### 2.3. Scale & Infrastructure Assumptions
*   **Traffic Profile:** Low QPS (Queries Per Second), highly spiky (batch processing and reporting seasons).
*   **Compute:** Serverless (GCP Cloud Run). Scales to zero to optimize idle costs.
*   **State:** GCP Firestore. Selected for its schema-less document model, native integration with GCP IAM, and zero-maintenance scaling.

---

## 3. Session Isolation & Out-of-Scope Items

To focus on the core orchestration and AI capabilities within the prototype time constraints, certain production features are explicitly deferred, while logical isolation is strictly maintained.

### 3.1. Out of Scope: End-User Authentication & Privacy
*   **No end-user IAM/JWT:** User authentication, identity verification (JWT), and strict privacy controls are **out of scope** for this prototype. 
*   The public API endpoints (`/api/v1/*`) do not enforce authentication headers.
*   **In scope: Service-to-service auth.** Internal endpoints (`/internal/*`) are protected by OIDC token verification at the application level (see §6.4).

### 3.2. In Scope: Logical Session Isolation
Despite lacking strict authentication, the system must support multiple concurrent clients without data contamination (mixing contexts or facts between different users' requests).
*   **Session Initialization:** A client starts a workflow by receiving or providing a unique `session_id` (UUID).
*   **Data Partitioning:** All data is strictly partitioned by this `session_id`. 
*   **Hierarchy:**
    *   **Session (Thread):** Bound to a `session_id`. Stores the long-term `chat_history` for multi-turn conversations.
    *   **Job (Task Graph):** Bound to a `session_id`. Represents a specific asynchronous multi-agent execution (`job_id`). The Blackboard (`CollectedFacts`, `GeneratedAssets`) belongs to the Job, ensuring that a new request in the same session gets a clean working board while retaining the conversational context.

---

## 4. High-Level Architecture

The system consists of five primary components interacting via an event-driven pattern.

### 4.1. Orchestration Agent (Go / Cloud Run)
*   **Function:** The LLM-driven central controller. Reads the Blackboard, asks Gemini to determine which specialized agent should act next, executes that agent, and enqueues the next iteration via Cloud Tasks. Exposes the public REST API.
*   **Design Choice:** Go is utilized for its low memory footprint, strict typing, and high concurrency. All agents run in the same Go binary — they are lightweight LLM wrappers, not separate services.

### 4.2. Specialized Agents (Go, in-process)
*   **Data Agent:** Fetches external data via Google Custom Search and SEC EDGAR XBRL API. Uses Gemini to extract structured facts from raw data. Writes facts to the Blackboard.
*   **Analyst Agent:** Generates Python analysis code via Gemini, sends it to the Sandbox service for execution, and stores results (charts, analysis output) on the Blackboard. Self-corrects by feeding execution errors back to Gemini (max 3 retries).
*   **Report Agent:** Synthesizes collected facts and analysis artifacts into a structured final report. Marks the job as COMPLETED.

### 4.3. Sandbox Service (Python / Cloud Run)
*   **Function:** Isolated code execution environment. Accepts Python code, validates it via AST against an import allowlist, executes in a subprocess with a restricted `__import__` hook and timeout, returns stdout and captured matplotlib charts as base64 PNG.
*   **Design Choice:** Python is used solely for sandboxed execution of LLM-generated data science code (pandas, matplotlib, numpy). No agent logic runs here.

### 4.4. The Blackboard (Firestore)
*   **Function:** The centralized `SessionState` and `JobState`. Acts as the single source of truth for all agents, ensuring statelessness in the Cloud Run containers.

### 4.5. Message Broker (Cloud Tasks)
*   **Function:** Provides reliable, asynchronous task delivery. Handles retries with exponential backoff for transient failures (e.g., LLM API rate limits, network drops) and supports task execution times up to 30 minutes.

---

## 5. Execution Workflow: Dynamic Context Resolution

The system eschews rigid Directed Acyclic Graphs (DAGs) in favor of a **Pull-Based State Model**. Agents dynamically request data if their context is insufficient.

1.  **Ingestion:** Client sends POST request containing an optional `session_id` and a `prompt`.
2.  **Setup:** Orchestrator generates a `session_id` (if not provided), creates a `Job` in Firestore, returns `202 Accepted` with `job_id` and `session_id`, and enqueues the initial routing task to Cloud Tasks.
3.  **Orchestration:** Cloud Tasks invokes `/internal/route`. The Orchestration Agent reads the Blackboard, asks Gemini which agent should act next, and executes that agent in-process.
4.  **Data Collection:** The Data Agent fetches external data (Google CSE, SEC EDGAR), extracts structured facts via Gemini, and writes them to the Blackboard.
5.  **Analysis:** The Analyst Agent generates Python code via Gemini, sends it to the Sandbox service for execution, and stores charts/results on the Blackboard. On failure, feeds the error back to Gemini for self-correction.
6.  **Dynamic Context Resolution:** If any agent determines insufficient context, it sets the job status to `NEEDS_CONTEXT` with `missing_queries`. The next orchestration iteration routes to the Data Agent to fulfill them.
7.  **Reporting:** The Report Agent synthesizes all collected facts and analysis into a structured final report, marks the job COMPLETED, and appends the report to session chat history.
8.  **Iteration:** After each agent completes, the Orchestrator enqueues another Cloud Task to `/internal/route` unless the job reached a terminal state (COMPLETED, FAILED, HITL).

---

## 6. Security & Defensive AI Mechanisms

### 6.1. LLM Output Validation & Self-Correction
*   LLM outputs are strictly enforced via schema validation (Struct Unmarshaling in Go, Pydantic in the Sandbox).
*   If an LLM returns malformed JSON, the agent catches the parsing error and automatically triggers a self-correction loop, feeding the error back to the LLM (maximum 3 retries) before failing the task.

### 6.2. Sandboxed Code Execution
*   The `Analyst Agent` generates LLM-produced Python code which is executed in a separate Sandbox service.
*   **Pre-flight Check:** The Sandbox parses code into an Abstract Syntax Tree (AST). An import allowlist restricts imports to safe modules (pandas, numpy, matplotlib, math, etc.). A custom `__import__` hook enforces this at runtime.
*   **Execution:** Code runs in a child process as a non-root user with restricted builtins (no exec, eval, compile, open) and a 30-second OS-level timeout. Exceptions are caught and fed back to the LLM for self-debugging.

### 6.3. Infinite Loop Circuit Breaker
*   To prevent agents from continuously requesting context without resolution, the Go Orchestrator atomically increments a `HopCount` on every state transition.
*   If `HopCount > 15`, execution is halted. The state transitions to `HITL` (Human in the Loop), and the system pauses, awaiting manual user clarification via the web UI.
*   `HopCount` is reset to 0 when a user replies and the job resumes.

### 6.4. Infrastructure Security
*   Internal endpoints (`/internal/route`) are protected by application-level OIDC middleware that validates the `Authorization: Bearer` token on each request.
*   The OIDC token audience must match `ORCHESTRATOR_BASE_URL` and the caller's email must match `SERVICE_ACCOUNT_EMAIL` (defense-in-depth).
*   Cloud Tasks is configured to send OIDC tokens with the service account identity when dispatching tasks.
*   The orchestrator Cloud Run service is deployed with `--allow-unauthenticated` to serve public API routes; internal route protection is enforced at the application layer.
*   The sandbox Cloud Run service is deployed with `--no-allow-unauthenticated`; only the orchestrator's service account (via `idtoken.NewClient`) can invoke it.
*   Local development can disable internal auth via `DISABLE_INTERNAL_AUTH=true`.

---

## 7. Observability & Telemetry

*   **Audit Trail:** Each agent execution writes an `AuditEntry` to a Firestore subcollection (`jobs/{jobID}/audit`). Entries include timestamp, acting agent, action type, token usage, and optional detail string. The orchestrator emits a "route" entry for each routing decision.
*   **Cost Tracking:** `TokenUsage` (prompt/completion/total) is returned from every LLM call (`GenerateJSON`, `GenerateText`) and accumulated atomically on the Job document via `AppendAuditLog`. The `token_usage` field on the Job provides a running total for cost monitoring.
*   **Client UX:** The orchestrator embeds a single-page web UI (Tailwind CSS + marked.js + DOMPurify) served at `GET /`. The frontend polls the `Job` document every 2 seconds to display the `active_agent`, status, and `token_usage`, providing transparency into the asynchronous execution process. When the job enters `HITL` status, the UI switches to reply mode, allowing users to provide clarification inline.