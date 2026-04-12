from __future__ import annotations

import ast
import base64
import io
import logging
import multiprocessing
import sys
import traceback
from dataclasses import dataclass, field

logger = logging.getLogger(__name__)

# Imports that are always blocked
BLOCKED_MODULES = frozenset({
    "os", "sys", "subprocess", "shutil", "socket", "ctypes",
    "pathlib", "signal", "importlib", "builtins", "code",
    "compileall", "compile", "codeop", "webbrowser", "http",
    "ftplib", "smtplib", "telnetlib", "xmlrpc", "pickle",
})

# Imports that are explicitly allowed in the sandbox
ALLOWED_MODULES = frozenset({
    "pandas", "matplotlib", "matplotlib.pyplot", "numpy",
    "json", "math", "datetime", "collections", "itertools",
    "statistics", "re", "io", "base64",
})


@dataclass
class ExecutionResult:
    success: bool
    stdout: str = ""
    error: str = ""
    figures: list[str] = field(default_factory=list)  # base64 PNGs


def validate_code(code: str) -> tuple[bool, str]:
    """Validate code via AST analysis. Returns (is_valid, error_message)."""
    try:
        tree = ast.parse(code)
    except SyntaxError as e:
        return False, f"Syntax error: {e}"

    for node in ast.walk(tree):
        # Check import statements
        if isinstance(node, ast.Import):
            for alias in node.names:
                root_module = alias.name.split(".")[0]
                if root_module in BLOCKED_MODULES:
                    return False, f"Blocked import: {alias.name}"

        elif isinstance(node, ast.ImportFrom):
            if node.module:
                root_module = node.module.split(".")[0]
                if root_module in BLOCKED_MODULES:
                    return False, f"Blocked import: from {node.module}"

        # Block exec/eval calls
        elif isinstance(node, ast.Call):
            if isinstance(node.func, ast.Name) and node.func.id in ("exec", "eval", "compile", "__import__"):
                return False, f"Blocked function call: {node.func.id}"

        # Block attribute access to dangerous builtins
        elif isinstance(node, ast.Attribute):
            if isinstance(node.value, ast.Name) and node.value.id == "__builtins__":
                return False, "Blocked access to __builtins__"

    return True, ""


def _run_in_process(code: str, result_queue: multiprocessing.Queue) -> None:
    """Execute code in a restricted subprocess and push results to queue."""
    import matplotlib
    matplotlib.use("Agg")  # Non-interactive backend
    import matplotlib.pyplot as plt
    import pandas as pd

    stdout_capture = io.StringIO()
    old_stdout = sys.stdout
    sys.stdout = stdout_capture

    try:
        restricted_globals = {
            "__builtins__": {
                "print": print,
                "range": range,
                "len": len,
                "int": int,
                "float": float,
                "str": str,
                "list": list,
                "dict": dict,
                "tuple": tuple,
                "set": set,
                "bool": bool,
                "abs": abs,
                "round": round,
                "min": min,
                "max": max,
                "sum": sum,
                "sorted": sorted,
                "enumerate": enumerate,
                "zip": zip,
                "map": map,
                "filter": filter,
                "isinstance": isinstance,
                "type": type,
                "True": True,
                "False": False,
                "None": None,
            },
            "pd": pd,
            "pandas": pd,
            "plt": plt,
            "matplotlib": matplotlib,
        }

        # Make common imports available
        import json as _json
        import math as _math
        import datetime as _datetime
        restricted_globals["json"] = _json
        restricted_globals["math"] = _math
        restricted_globals["datetime"] = _datetime

        exec(code, restricted_globals)

        # Capture matplotlib figures
        figures = []
        for fig_num in plt.get_fignums():
            fig = plt.figure(fig_num)
            buf = io.BytesIO()
            fig.savefig(buf, format="png", dpi=150, bbox_inches="tight")
            buf.seek(0)
            figures.append(base64.b64encode(buf.read()).decode("utf-8"))
            plt.close(fig)

        sys.stdout = old_stdout
        result_queue.put(ExecutionResult(
            success=True,
            stdout=stdout_capture.getvalue(),
            figures=figures,
        ))
    except Exception:
        sys.stdout = old_stdout
        result_queue.put(ExecutionResult(
            success=False,
            stdout=stdout_capture.getvalue(),
            error=traceback.format_exc(),
        ))


def execute_code(code: str, timeout: int = 15) -> ExecutionResult:
    """Validate and execute code in a sandboxed subprocess with timeout."""
    is_valid, error_msg = validate_code(code)
    if not is_valid:
        return ExecutionResult(success=False, error=f"Validation failed: {error_msg}")

    result_queue = multiprocessing.Queue()
    proc = multiprocessing.Process(target=_run_in_process, args=(code, result_queue))
    proc.start()
    proc.join(timeout=timeout)

    if proc.is_alive():
        proc.kill()
        proc.join(timeout=2)
        return ExecutionResult(success=False, error=f"Execution timed out after {timeout}s")

    if result_queue.empty():
        return ExecutionResult(success=False, error="Process exited without producing results")

    return result_queue.get()
