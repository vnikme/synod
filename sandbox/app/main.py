from fastapi import FastAPI
from pydantic import BaseModel

from app.executor import execute_code

app = FastAPI(title="Synod Sandbox")


class ExecuteRequest(BaseModel):
    code: str


class ExecuteResponse(BaseModel):
    success: bool
    stdout: str = ""
    error: str = ""
    charts: list[str] = []


@app.get("/health")
def health():
    return {"status": "ok"}


@app.post("/execute", response_model=ExecuteResponse)
def execute(req: ExecuteRequest):
    return execute_code(req.code)
