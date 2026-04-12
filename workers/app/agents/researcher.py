from __future__ import annotations

import logging
import os
import time

import requests
from fastapi import APIRouter, HTTPException
from google import genai

from app.cloud_tasks import enqueue_callback
from app.models import Fact, JobStatus, ResearchFacts, TaskPayload
from app.store import get_job, get_session, update_job

logger = logging.getLogger(__name__)
router = APIRouter()

EDGAR_BASE = "https://data.sec.gov/api/xbrl/companyfacts"
EDGAR_RATE_DELAY = 0.12  # ~10 req/s
CSE_URL = "https://www.googleapis.com/customsearch/v1"

# Well-known CIK mappings for common tickers
TICKER_CIK = {
    "AAPL": "0000320193",
    "MSFT": "0000789019",
    "GOOGL": "0001652044",
    "AMZN": "0001018724",
    "META": "0001326801",
    "TSLA": "0001318605",
    "NVDA": "0001045810",
    "JPM": "0000019617",
    "V": "0001403161",
    "JNJ": "0000200406",
}

RESEARCHER_SYSTEM_PROMPT = """You are a research extraction agent. Given raw search results and/or financial data,
extract structured facts relevant to the user's original query.

Respond with valid JSON matching this schema:
{
  "facts": [
    {"key": "descriptive label", "value": "the data/finding", "source": "where this came from"}
  ]
}

Rules:
- Extract only facts relevant to the user's query.
- Each fact should be self-contained and concise.
- Include the source (URL, "SEC EDGAR", etc.) for every fact.
- If financial data is provided, extract quarterly figures as individual facts.
- Aim for 5-15 facts total."""


def _gemini_client() -> genai.Client:
    return genai.Client(api_key=os.environ["GEMINI_API_KEY"])


def _search_web(query: str) -> list[dict]:
    """Search via Google Custom Search Engine."""
    api_key = os.environ.get("GOOGLE_CSE_API_KEY")
    cx = os.environ.get("GOOGLE_CSE_CX")
    if not api_key or not cx:
        logger.warning("CSE credentials not configured, skipping web search")
        return []

    try:
        resp = requests.get(
            CSE_URL,
            params={"key": api_key, "cx": cx, "q": query, "num": 5},
            timeout=10,
        )
        resp.raise_for_status()
        items = resp.json().get("items", [])
        return [{"title": it.get("title", ""), "snippet": it.get("snippet", ""), "link": it.get("link", "")} for it in items]
    except Exception:
        logger.exception("Web search failed for query: %s", query)
        return []


def _fetch_edgar(cik: str) -> dict | None:
    """Fetch XBRL company facts from SEC EDGAR."""
    user_agent = os.environ.get("SEC_EDGAR_USER_AGENT", "Synod/1.0 (synod@ai.church)")
    url = f"{EDGAR_BASE}/CIK{cik}.json"

    try:
        time.sleep(EDGAR_RATE_DELAY)
        resp = requests.get(url, headers={"User-Agent": user_agent}, timeout=15)
        resp.raise_for_status()
        return resp.json()
    except Exception:
        logger.exception("EDGAR fetch failed for CIK %s", cik)
        return None


def _extract_quarterly_data(edgar_data: dict, metrics: list[str] | None = None) -> str:
    """Extract recent quarterly financial data from EDGAR XBRL response."""
    if metrics is None:
        metrics = ["Revenues", "RevenueFromContractWithCustomerExcludingAssessedTax", "NetIncomeLoss", "Assets"]

    facts = edgar_data.get("facts", {}).get("us-gaap", {})
    lines = []
    entity = edgar_data.get("entityName", "Unknown")
    lines.append(f"Entity: {entity}")

    for metric in metrics:
        entries = facts.get(metric, {}).get("units", {})
        for unit, values in entries.items():
            # Filter to 10-Q filings, take last 4 quarters
            quarterly = [v for v in values if v.get("form") == "10-Q"]
            quarterly.sort(key=lambda x: x.get("end", ""), reverse=True)
            for q in quarterly[:4]:
                lines.append(f"{metric} ({unit}): {q.get('val', 'N/A')} | period ending {q.get('end', 'N/A')} | filed {q.get('filed', 'N/A')}")

    return "\n".join(lines) if len(lines) > 1 else ""


def _detect_tickers(queries: list[str]) -> list[str]:
    """Simple ticker detection from queries."""
    found = []
    text = " ".join(queries).upper()
    for ticker in TICKER_CIK:
        if ticker in text:
            found.append(ticker)
    # Also check company names
    name_map = {"APPLE": "AAPL", "MICROSOFT": "MSFT", "GOOGLE": "GOOGL", "AMAZON": "AMZN", "TESLA": "TSLA", "NVIDIA": "NVDA"}
    for name, ticker in name_map.items():
        if name in text and ticker not in found:
            found.append(ticker)
    return found


@router.post("/internal/agent/researcher")
async def researcher_webhook(payload: TaskPayload):
    job_data = get_job(payload.job_id, payload.session_id)
    if job_data is None:
        raise HTTPException(status_code=404, detail="Job not found")

    # Idempotency: only process if status allows
    status = job_data.get("status")
    if status not in (JobStatus.IN_PROGRESS, JobStatus.NEEDS_CONTEXT):
        logger.info("Researcher: skipping job %s with status %s", payload.job_id, status)
        return {"status": "skipped"}

    prompt = job_data.get("prompt", "")
    queries = job_data.get("missing_queries", [])
    if not queries:
        queries = [prompt]

    # 1. Web search
    all_search_results = []
    for q in queries[:5]:  # Cap at 5 queries
        results = _search_web(q)
        all_search_results.extend(results)

    # 2. SEC EDGAR — detect tickers and fetch
    edgar_text = ""
    tickers = _detect_tickers(queries + [prompt])
    for ticker in tickers[:3]:  # Cap at 3 tickers
        cik = TICKER_CIK.get(ticker)
        if cik:
            data = _fetch_edgar(cik)
            if data:
                edgar_text += f"\n--- {ticker} ({cik}) ---\n"
                edgar_text += _extract_quarterly_data(data)

    # 3. LLM extraction
    context_parts = [f"User query: {prompt}"]
    if all_search_results:
        context_parts.append("Web search results:\n" + "\n".join(
            f"- {r['title']}: {r['snippet']} ({r['link']})" for r in all_search_results
        ))
    if edgar_text:
        context_parts.append(f"SEC EDGAR financial data:\n{edgar_text}")

    if not all_search_results and not edgar_text:
        context_parts.append("No external data was found. Generate facts based on general knowledge with a note that data could not be verified.")

    client = _gemini_client()
    model = os.environ.get("LLM_MODEL", "gemini-2.0-flash")

    resp = client.models.generate_content(
        model=model,
        contents="\n\n".join(context_parts),
        config={
            "system_instruction": RESEARCHER_SYSTEM_PROMPT,
            "response_mime_type": "application/json",
            "response_schema": ResearchFacts,
        },
    )

    research = ResearchFacts.model_validate_json(resp.text)
    facts_dicts = [f.model_dump() for f in research.facts]

    # 4. Update Firestore
    update_job(payload.job_id, payload.session_id, {
        "collected_facts": facts_dicts,
        "status": JobStatus.IN_PROGRESS.value,
        "active_agent": "researcher",
    })

    # 5. Callback to orchestrator
    enqueue_callback(payload.job_id, payload.session_id)

    logger.info("Researcher completed: job=%s facts=%d", payload.job_id, len(research.facts))
    return {"status": "completed", "facts_count": len(research.facts)}
