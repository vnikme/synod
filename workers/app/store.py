from __future__ import annotations

import logging
import os

from google.cloud import firestore

logger = logging.getLogger(__name__)

_client: firestore.Client | None = None


def _get_client() -> firestore.Client:
    global _client
    if _client is None:
        project = os.environ["GCP_PROJECT_ID"]
        _client = firestore.Client(project=project)
    return _client


def get_job(job_id: str, session_id: str) -> dict | None:
    """Retrieve a job document, enforcing session isolation."""
    doc = _get_client().collection("jobs").document(job_id).get()
    if not doc.exists:
        return None
    data = doc.to_dict()
    if data.get("session_id") != session_id:
        logger.warning("Session isolation violation: job=%s expected=%s got=%s", job_id, session_id, data.get("session_id"))
        return None
    return data


def update_job(job_id: str, session_id: str, updates: dict) -> None:
    """Update a job document after verifying session ownership."""
    job = get_job(job_id, session_id)
    if job is None:
        raise ValueError(f"Job {job_id} not found for session {session_id}")
    _get_client().collection("jobs").document(job_id).update(updates)


def get_session(session_id: str) -> dict | None:
    """Retrieve a session document."""
    doc = _get_client().collection("sessions").document(session_id).get()
    if not doc.exists:
        return None
    return doc.to_dict()
