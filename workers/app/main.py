import logging
import os

from fastapi import FastAPI

from app.agents.analyst import router as analyst_router
from app.agents.researcher import router as researcher_router

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s %(message)s",
)

app = FastAPI(title="Synod Workers", version="1.0.0")

app.include_router(researcher_router)
app.include_router(analyst_router)


@app.get("/health")
async def health():
    return {"status": "ok"}
