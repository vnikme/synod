"""Sandboxed Python code execution with AST validation and restricted imports."""

import ast
import base64
import builtins
import io
import multiprocessing
import traceback
from contextlib import redirect_stdout

# Use "spawn" instead of the default "fork". Uvicorn runs sync endpoints in a
# thread pool, so forking would copy orphaned lock state (logging, import locks,
# etc.) into the child process, causing deadlocks that hang until the timeout
# kills the process — preventing charts from ever being produced.
_mp_ctx = multiprocessing.get_context("spawn")

ALLOWED_MODULES = frozenset({
    "pandas", "numpy", "matplotlib", "matplotlib.pyplot",
    "math", "statistics", "collections", "itertools",
    "json", "csv", "datetime", "re", "textwrap",
})

BLOCKED_MODULES = frozenset({
    "os", "sys", "subprocess", "shutil", "socket",
    "ctypes", "importlib", "pathlib", "signal",
    "multiprocessing", "threading", "http", "urllib",
    "ftplib", "smtplib", "telnetlib", "xmlrpc",
    "pickle", "shelve", "marshal", "code", "codeop",
    "compileall", "py_compile",
})

ALLOWED_ROOTS = frozenset(m.split(".")[0] for m in ALLOWED_MODULES)


class CodeValidator(ast.NodeVisitor):
    """Validates code AST against import allowlist."""

    def __init__(self):
        self.violations: list[str] = []

    def visit_Import(self, node: ast.Import):
        for alias in node.names:
            root = alias.name.split(".")[0]
            if root in BLOCKED_MODULES:
                self.violations.append(f"Blocked import: {alias.name} (no network/OS access in sandbox)")
            elif root not in ALLOWED_ROOTS:
                self.violations.append(
                    f"Disallowed import: {alias.name}. "
                    f"Allowed: {', '.join(sorted(ALLOWED_MODULES))}. "
                    f"Embed data as Python literals instead of fetching it."
                )
        self.generic_visit(node)

    def visit_ImportFrom(self, node: ast.ImportFrom):
        if node.module:
            root = node.module.split(".")[0]
            if root in BLOCKED_MODULES:
                self.violations.append(f"Blocked import from: {node.module} (no network/OS access in sandbox)")
            elif root not in ALLOWED_ROOTS:
                self.violations.append(
                    f"Disallowed import from: {node.module}. "
                    f"Allowed: {', '.join(sorted(ALLOWED_MODULES))}. "
                    f"Embed data as Python literals instead of fetching it."
                )
        self.generic_visit(node)


def validate_code(code: str) -> list[str]:
    """Parse and validate code against security policy."""
    try:
        tree = ast.parse(code)
    except SyntaxError as e:
        return [f"Syntax error: {e}"]
    validator = CodeValidator()
    validator.visit(tree)
    return validator.violations


def _restricted_import(name, globals=None, locals=None, fromlist=(), level=0):
    """Import hook: enforce the same import allowlist at runtime.

    User code can call ``__import__`` directly, which bypasses AST checks for
    ``import`` statements. To prevent sandbox escapes, runtime imports must be
    restricted to the same allowlist enforced by ``CodeValidator``.

    Allowed third-party libraries are pre-imported before this hook is exposed
    to user code, but the actual import result is still delegated to Python's
    ``__import__`` so standard import semantics are preserved for cached and
    uncached modules alike.
    """
    root = name.split(".")[0]
    if root in BLOCKED_MODULES:
        raise ImportError(
            f"Import of '{name}' is blocked for security. "
            f"The sandbox has no network access. Embed all data directly in the code as Python literals."
        )
    if root not in ALLOWED_ROOTS:
        raise ImportError(
            f"Import of '{name}' is not allowed in the sandbox. "
            f"Allowed libraries: {', '.join(sorted(ALLOWED_MODULES))}. "
            f"Embed all data directly in the code as Python literals instead of fetching it."
        )
    return builtins.__import__(name, globals, locals, fromlist, level)


def _run_in_process(code: str, result_queue: multiprocessing.Queue):
    """Execute code in a restricted namespace within a child process."""
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    # Pre-import heavy allowed libraries before installing the restricted
    # import hook. This populates sys.modules so subsequent imports by
    # user code (and internal imports by these libs) hit the cache and
    # never trigger the restricted hook for os/sys/etc.
    import pandas  # noqa: F401
    import numpy  # noqa: F401

    stdout_buf = io.StringIO()
    charts: list[str] = []
    error: str | None = None

    try:
        safe_builtins = {
            k: v for k, v in vars(builtins).items()
            if k not in ("exec", "eval", "compile", "open", "breakpoint", "exit", "quit", "input")
        }
        safe_builtins["__import__"] = _restricted_import

        namespace = {"__builtins__": safe_builtins}

        with redirect_stdout(stdout_buf):
            exec(code, namespace)  # noqa: S102

        # Capture all matplotlib figures
        for fig_num in plt.get_fignums():
            fig = plt.figure(fig_num)
            buf = io.BytesIO()
            fig.savefig(buf, format="png", bbox_inches="tight", dpi=150)
            buf.seek(0)
            charts.append(base64.b64encode(buf.read()).decode())
        plt.close("all")

    except Exception:
        error = traceback.format_exc()

    result_queue.put({
        "success": error is None,
        "stdout": stdout_buf.getvalue(),
        "error": error or "",
        "charts": charts,
    })


def execute_code(code: str, timeout: int = 120) -> dict:
    """Validate and execute code in a sandboxed subprocess with timeout."""
    violations = validate_code(code)
    if violations:
        return {
            "success": False,
            "stdout": "",
            "error": "Code validation failed: " + "; ".join(violations),
            "charts": [],
        }

    queue = _mp_ctx.Queue()
    proc = _mp_ctx.Process(target=_run_in_process, args=(code, queue))
    proc.start()
    try:
        proc.join(timeout)

        if proc.is_alive():
            proc.terminate()
            proc.join(2)
            if proc.is_alive():
                proc.kill()
            return {
                "success": False,
                "stdout": "",
                "error": f"Execution timed out after {timeout}s",
                "charts": [],
            }

        try:
            return queue.get(timeout=5)
        except Exception:
            return {
                "success": False,
                "stdout": "",
                "error": "Execution produced no result (process may have crashed)",
                "charts": [],
            }
    finally:
        queue.close()
        queue.join_thread()
