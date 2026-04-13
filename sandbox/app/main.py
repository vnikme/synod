import logging
import time

from fastapi import FastAPI
from pydantic import BaseModel, Field

from app.executor import execute_code

logger = logging.getLogger("sandbox")

# Pre-warm matplotlib font cache at startup so child processes don't
# spend 30s+ building it on first use.
try:
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    fig, ax = plt.subplots()
    ax.plot([0, 1], [0, 1])
    plt.close(fig)
    logger.info("matplotlib font cache warmed")
except Exception as e:
    logger.warning("matplotlib warm-up failed: %s", e)

logger.info("sandbox ready (forkserver with preloaded matplotlib/pandas/numpy)")

app = FastAPI(title="Synod Sandbox")


class ExecuteRequest(BaseModel):
    code: str


class ExecuteResponse(BaseModel):
    success: bool
    stdout: str = ""
    error: str = ""
    charts: list[str] = Field(default_factory=list)
    timings: dict[str, float] = Field(default_factory=dict)


@app.get("/health")
def health():
    return {"status": "ok"}


@app.post("/execute", response_model=ExecuteResponse)
def execute(req: ExecuteRequest):
    logger = logging.getLogger("sandbox.endpoint")
    t0 = time.monotonic()
    result = execute_code(req.code)
    elapsed = round(time.monotonic() - t0, 3)
    timings = result.get("timings", {})
    timings["endpoint_total_s"] = elapsed
    result["timings"] = timings
    logger.info(
        "POST /execute: success=%s elapsed=%.3fs code_len=%d",
        result["success"], elapsed, len(req.code),
        extra={"timings": timings},
    )
    return result
