from __future__ import annotations

import json
import logging
import os

from fastapi import APIRouter, HTTPException
from google import genai

from app.cloud_tasks import enqueue_callback
from app.models import AnalysisCode, Asset, FinalAnswer, JobStatus, TaskPayload
from app.sandbox import execute_code
from app.store import get_job, get_session, update_job

logger = logging.getLogger(__name__)
router = APIRouter()

MAX_CODE_RETRIES = 3

CODE_GEN_SYSTEM_PROMPT = """You are a Python data analyst. Given collected facts and a user request,
generate self-contained Python code that analyzes the data and creates visualizations.

Respond with valid JSON matching this schema:
{
  "code": "the complete Python code as a single string",
  "explanation": "brief description of what the code does"
}

Rules:
- Use ONLY these imports: pandas (as pd), matplotlib.pyplot (as plt), json, math, datetime
- Do NOT import os, sys, subprocess, or any other system modules.
- The code must be completely self-contained — define all data inline (e.g., as dicts or lists).
- Create at least one matplotlib figure using plt.figure() and plt.show() or plt.savefig().
- Use plt.title(), plt.xlabel(), plt.ylabel() for clear labeling.
- Print any numerical results or summaries to stdout.
- Handle edge cases (empty data, missing values) gracefully.
- Use a clean, professional style for charts (consider plt.style.use('seaborn-v0_8-whitegrid') or similar)."""

AGGREGATION_SYSTEM_PROMPT = """You are an executive summary writer. Given the original query, collected facts,
and analysis results, produce a final structured answer.

Respond with valid JSON matching this schema:
{
  "summary": "A clear, concise executive summary answering the user's original question (2-4 paragraphs)",
  "key_findings": ["finding 1", "finding 2", ...]
}

Rules:
- Reference specific data points from the collected facts.
- Mention the chart/visualization if one was generated.
- Be precise with numbers and time periods.
- Write in a professional analyst tone."""


def _gemini_client() -> genai.Client:
    return genai.Client(api_key=os.environ["GEMINI_API_KEY"])


def _build_facts_context(job_data: dict) -> str:
    """Format collected facts into a readable context string."""
    facts = job_data.get("collected_facts", [])
    if not facts:
        return "No facts collected."
    lines = []
    for f in facts:
        lines.append(f"- {f['key']}: {f['value']} (source: {f['source']})")
    return "\n".join(lines)


def _generate_code(client: genai.Client, model: str, prompt: str, facts_ctx: str, error_context: str = "") -> AnalysisCode:
    """Call Gemini to generate analysis code."""
    user_content = f"User request: {prompt}\n\nCollected facts:\n{facts_ctx}"
    if error_context:
        user_content += f"\n\nPrevious code attempt failed with this error:\n{error_context}\nPlease fix the code."

    resp = client.models.generate_content(
        model=model,
        contents=user_content,
        config={
            "system_instruction": CODE_GEN_SYSTEM_PROMPT,
            "response_mime_type": "application/json",
            "response_schema": AnalysisCode,
        },
    )
    return AnalysisCode.model_validate_json(resp.text)


def _generate_final_answer(client: genai.Client, model: str, prompt: str, facts_ctx: str, stdout: str, has_chart: bool) -> FinalAnswer:
    """Call Gemini to produce the aggregated final answer."""
    chart_note = "A chart visualization was generated and is attached." if has_chart else "No chart was generated."
    user_content = f"Original query: {prompt}\n\nCollected facts:\n{facts_ctx}\n\nAnalysis output:\n{stdout}\n\n{chart_note}"

    resp = client.models.generate_content(
        model=model,
        contents=user_content,
        config={
            "system_instruction": AGGREGATION_SYSTEM_PROMPT,
            "response_mime_type": "application/json",
            "response_schema": FinalAnswer,
        },
    )
    return FinalAnswer.model_validate_json(resp.text)


@router.post("/internal/agent/analyst")
async def analyst_webhook(payload: TaskPayload):
    job_data = get_job(payload.job_id, payload.session_id)
    if job_data is None:
        raise HTTPException(status_code=404, detail="Job not found")

    status = job_data.get("status")
    if status != JobStatus.IN_PROGRESS:
        logger.info("Analyst: skipping job %s with status %s", payload.job_id, status)
        return {"status": "skipped"}

    prompt = job_data.get("prompt", "")
    facts_ctx = _build_facts_context(job_data)

    client = _gemini_client()
    model = os.environ.get("LLM_MODEL", "gemini-2.0-flash")

    # Code generation + sandbox execution with retries
    last_error = ""
    exec_result = None
    for attempt in range(1, MAX_CODE_RETRIES + 1):
        logger.info("Analyst: code generation attempt %d for job %s", attempt, payload.job_id)

        try:
            analysis = _generate_code(client, model, prompt, facts_ctx, error_context=last_error)
        except Exception:
            logger.exception("Analyst: code generation failed, attempt %d", attempt)
            last_error = "LLM failed to generate valid code JSON"
            continue

        exec_result = execute_code(analysis.code)
        if exec_result.success:
            logger.info("Analyst: code executed successfully on attempt %d", attempt)
            break

        last_error = exec_result.error
        logger.warning("Analyst: execution failed attempt %d: %s", attempt, last_error[:200])

    # Build result regardless of code execution success
    assets = []
    stdout = ""
    has_chart = False

    if exec_result and exec_result.success:
        stdout = exec_result.stdout
        for i, fig_b64 in enumerate(exec_result.figures):
            assets.append(Asset(type="image/png", data=fig_b64).model_dump())
            has_chart = True
    elif exec_result:
        stdout = f"Code execution failed after {MAX_CODE_RETRIES} attempts. Last error: {exec_result.error}"
    else:
        stdout = "Code generation failed completely."

    # Generate final aggregated answer
    try:
        final = _generate_final_answer(client, model, prompt, facts_ctx, stdout, has_chart)
        final_text = final.summary
        if final.key_findings:
            final_text += "\n\nKey Findings:\n" + "\n".join(f"• {f}" for f in final.key_findings)
    except Exception:
        logger.exception("Analyst: final answer generation failed")
        final_text = f"Analysis completed. Raw output:\n{stdout}"

    # Update Firestore
    update_job(payload.job_id, payload.session_id, {
        "status": JobStatus.COMPLETED.value,
        "active_agent": "analyst",
        "generated_assets": assets,
        "final_result": final_text,
    })

    # Callback to orchestrator
    enqueue_callback(payload.job_id, payload.session_id)

    logger.info("Analyst completed: job=%s assets=%d", payload.job_id, len(assets))
    return {"status": "completed", "assets_count": len(assets)}
