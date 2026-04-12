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

### 3.1. Out of Scope: Authentication & Privacy
*   **No IAM/JWT:** User authentication, identity verification (JWT), and strict privacy controls are **out of scope** for this prototype. 
*   The API endpoints will not enforce authentication headers.

### 3.2. In Scope: Logical Session Isolation
Despite lacking strict authentication, the system must support multiple concurrent clients without data contamination (mixing contexts or facts between different users' requests).
*   **Session Initialization:** A client starts a workflow by receiving or providing a unique `session_id` (UUID).
*   **Data Partitioning:** All data is strictly partitioned by this `session_id`. 
*   **Hierarchy:**
    *   **Session (Thread):** Bound to a `session_id`. Stores the long-term `chat_history` for multi-turn conversations.
    *   **Job (Task Graph):** Bound to a `session_id`. Represents a specific asynchronous multi-agent execution (`job_id`). The Blackboard (`CollectedFacts`, `GeneratedAssets`) belongs to the Job, ensuring that a new request in the same session gets a clean working board while retaining the conversational context.

---

## 4. High-Level Architecture

The system consists of four primary components interacting via an event-driven pattern.

### 4.1. Core Orchestrator (Go / Cloud Run)
*   **Function:** The central State Machine. Exposes the public REST API, evaluates user intent, mutates the global state, and dispatches tasks.
*   **Design Choice:** Go is utilized for its low memory footprint, strict typing, and high concurrency when handling inbound HTTP connections and webhooks.

### 4.2. Worker Fleet (Python / Cloud Run)
*   **Function:** Ephemeral execution environments exposing internal webhooks.
*   **Nodes:**
    *   `Researcher`: Executes web searches and fetches financial metrics (SEC APIs). Restricted from performing analysis to prevent context bloat.
    *   `Quant Analyst`: Reads extracted facts, generates Python code (Pandas/Matplotlib), executes it within a sandboxed environment, and outputs artifacts.

### 4.3. The Blackboard (Firestore)
*   **Function:** The centralized `SessionState` and `JobState`. Acts as the single source of truth for all agents, ensuring statelessness in the Cloud Run containers.

### 4.4. Message Broker (Cloud Tasks)
*   **Function:** Provides reliable, asynchronous task delivery. Handles retries with exponential backoff for transient failures (e.g., LLM API rate limits, network drops) and supports task execution times up to 30 minutes.

---

## 5. Execution Workflow: Dynamic Context Resolution

The system eschews rigid Directed Acyclic Graphs (DAGs) in favor of a **Pull-Based State Model**. Agents dynamically request data if their context is insufficient.

1.  **Ingestion:** Client sends POST request containing an optional `session_id` and a `prompt`.
2.  **Setup:** Go Orchestrator generates a `session_id` (if not provided), creates a `Job` in Firestore bound to the `session_id`, returns `202 Accepted` with the `job_id` and `session_id`, and enqueues the initial task to Cloud Tasks.
3.  **Worker Invocation:** Cloud Tasks invokes the target Python Worker (e.g., `Quant Analyst`) via an internal webhook.
4.  **State Evaluation:** The Worker reads the `Job` and associated `chat_history` from Firestore using the `job_id` and `session_id`.
5.  **Dynamic Request (Missing Context):** If the `Quant Analyst` lacks necessary data, it updates the Firestore document status to `NEEDS_CONTEXT`, populates the `missing_queries` array, enqueues a callback to the Orchestrator, and terminates successfully (HTTP 200).
6.  **Re-routing:** The Orchestrator receives the callback, reads the `NEEDS_CONTEXT` state, and enqueues a task for the `Researcher` to fulfill the missing queries.
7.  **Resolution:** Once the `Researcher` populates the Blackboard, the Orchestrator re-enqueues the `Quant Analyst`, which now successfully completes the task.

---

## 6. Security & Defensive AI Mechanisms

### 6.1. LLM Output Validation & Self-Correction
*   LLM outputs are strictly enforced via schema validation (Pydantic in Python, Struct Unmarshaling in Go).
*   If an LLM returns malformed JSON, the worker catches the parsing error and automatically triggers a self-correction loop, feeding the error back to the LLM (maximum 3 retries) before failing the task.

### 6.2. Sandboxed Code Execution
*   The `Quant Analyst` must execute LLM-generated Python code.
*   **Pre-flight Check:** The code is parsed into an Abstract Syntax Tree (AST). Imports of `os`, `sys`, and `subprocess` are strictly blocked.
*   **Execution:** Code is executed using restricted globals/locals and bounded by a 15-second OS-level timeout. Exceptions are caught and fed back to the LLM for self-debugging.

### 6.3. Infinite Loop Circuit Breaker
*   To prevent agents from continuously requesting context without resolution, the Go Orchestrator increments a `HopCount` on every state transition.
*   If `HopCount > 5`, execution is halted. The state transitions to `NEEDS_HUMAN_INPUT` (HITL - Human in the Loop), and the system pauses, awaiting manual user clarification.

### 6.4. Infrastructure Security
*   All internal Cloud Run endpoints (workers and orchestrator callbacks) require GCP IAM OIDC tokens. Public invocation is blocked.

---

## 7. Observability & Telemetry

*   **Audit Trail:** Every state mutation in Firestore includes a timestamp, the acting agent, and the delta.
*   **Cost Tracking:** Token consumption (Prompt/Completion) is aggregated per `job_id` to monitor API burn rates.
*   **Client UX:** The frontend polls the `Job` document (or connects via Server-Sent Events) to stream the `active_agent` and status, providing transparency into the asynchronous execution process.