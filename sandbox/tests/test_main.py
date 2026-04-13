"""Tests for the FastAPI sandbox API endpoints."""

import pytest
from fastapi.testclient import TestClient

from app.main import app

client = TestClient(app)


class TestHealthEndpoint:
    def test_health_returns_ok(self):
        resp = client.get("/health")
        assert resp.status_code == 200
        assert resp.json() == {"status": "ok"}


class TestExecuteEndpoint:
    def test_simple_code(self):
        resp = client.post("/execute", json={"code": "print('hello')"})
        assert resp.status_code == 200
        data = resp.json()
        assert data["success"] is True
        assert "hello" in data["stdout"]

    def test_blocked_import(self):
        resp = client.post("/execute", json={"code": "import os"})
        assert resp.status_code == 200
        data = resp.json()
        assert data["success"] is False
        assert "Blocked" in data["error"]

    def test_chart_generation(self):
        code = """
import matplotlib.pyplot as plt
plt.figure()
plt.plot([1, 2, 3], [1, 4, 9])
"""
        resp = client.post("/execute", json={"code": code})
        assert resp.status_code == 200
        data = resp.json()
        assert data["success"] is True
        assert len(data["charts"]) == 1

    def test_missing_code_field(self):
        resp = client.post("/execute", json={})
        assert resp.status_code == 422  # Pydantic validation

    def test_empty_code(self):
        resp = client.post("/execute", json={"code": ""})
        assert resp.status_code == 200
        data = resp.json()
        assert data["success"] is True  # empty code is valid Python

    def test_pandas_computation(self):
        code = """
import pandas as pd
df = pd.DataFrame({'x': [10, 20, 30]})
print(df['x'].mean())
"""
        resp = client.post("/execute", json={"code": code})
        assert resp.status_code == 200
        data = resp.json()
        assert data["success"] is True
        assert "20" in data["stdout"]

    def test_runtime_error(self):
        resp = client.post("/execute", json={"code": "1/0"})
        assert resp.status_code == 200
        data = resp.json()
        assert data["success"] is False
        assert "ZeroDivisionError" in data["error"]
