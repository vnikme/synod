from __future__ import annotations

import json
import logging
import os

from google.cloud import tasks_v2

logger = logging.getLogger(__name__)

_client: tasks_v2.CloudTasksClient | None = None


def _get_client() -> tasks_v2.CloudTasksClient:
    global _client
    if _client is None:
        _client = tasks_v2.CloudTasksClient()
    return _client


def enqueue_callback(job_id: str, session_id: str, target_url: str | None = None) -> None:
    """Enqueue a callback task to the orchestrator's /internal/route endpoint."""
    project = os.environ["GCP_PROJECT_ID"]
    location = os.environ["CLOUD_TASKS_LOCATION"]
    queue = os.environ["CLOUD_TASKS_QUEUE"]
    sa_email = os.environ["SERVICE_ACCOUNT_EMAIL"]

    if target_url is None:
        orchestrator_url = os.environ["ORCHESTRATOR_BASE_URL"].rstrip("/")
        target_url = f"{orchestrator_url}/internal/route"

    parent = f"projects/{project}/locations/{location}/queues/{queue}"
    payload = json.dumps({"job_id": job_id, "session_id": session_id}).encode()

    task = tasks_v2.Task(
        http_request=tasks_v2.HttpRequest(
            url=target_url,
            http_method=tasks_v2.HttpMethod.POST,
            headers={"Content-Type": "application/json"},
            body=payload,
            oidc_token=tasks_v2.OidcToken(
                service_account_email=sa_email,
                audience=target_url,
            ),
        ),
    )

    resp = _get_client().create_task(parent=parent, task=task)
    logger.info("Callback enqueued: target=%s job=%s task=%s", target_url, job_id, resp.name)
