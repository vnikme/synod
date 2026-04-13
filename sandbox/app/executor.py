"""Sandboxed Python code execution with AST validation and restricted imports."""

import ast
import base64
import builtins
import io
import logging
import multiprocessing
import time
import traceback
from contextlib import redirect_stdout

# Use "forkserver" instead of the default "fork" or "spawn".
#
# "fork" is unsafe: uvicorn runs sync endpoints in a thread pool, so forking
# copies orphaned lock state (logging, import locks, etc.) into the child,
# causing deadlocks.
#
# "spawn" is safe but slow: it starts a brand-new Python interpreter per
# execution. On Cloud Run with 1 vCPU, importing matplotlib + pandas + numpy
# from scratch takes over 120s, exceeding the sandbox timeout.
#
# "forkserver" is both safe and fast: a dedicated server process (no threads)
# is started once and pre-imports the heavy libraries. Each child is forked
# from this clean server, inheriting the loaded modules instantly.
#
# Do not preload matplotlib.pyplot: _run_in_process explicitly selects the
# Agg backend with matplotlib.use("Agg") before importing pyplot. MPLBACKEND
# is also set in the Dockerfile, but preloading pyplot would lock in backend
# selection too early.
if "forkserver" in multiprocessing.get_all_start_methods():
    _mp_ctx = multiprocessing.get_context("forkserver")
    _mp_ctx.set_forkserver_preload(["matplotlib", "pandas", "numpy"])
else:
    # Fallback for platforms without forkserver (Windows). Imports will be
    # slower but correct.
    _mp_ctx = multiprocessing.get_context("spawn")


def _noop_warmup_target():
    """No-op target used to eagerly start the forkserver process."""


def _warm_forkserver():
    """Pay one-time forkserver startup/preload cost at import time.

    Without this, the first ``proc.start()`` triggers forkserver creation
    and library preloading, adding unaccounted latency to ``spawn_s`` that
    is outside the execution timeout.
    """
    if _mp_ctx.get_start_method() != "forkserver":
        return
    t0 = time.monotonic()
    proc = _mp_ctx.Process(target=_noop_warmup_target)
    try:
        proc.start()
        proc.join()
        logging.getLogger("sandbox.executor").info(
            "forkserver warmed in %.3fs", time.monotonic() - t0)
    except Exception:
        logging.getLogger("sandbox.executor").exception(
            "failed to warm forkserver")
        if proc.is_alive():
            proc.terminate()
            proc.join()


_warm_forkserver()

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
    import sys as _sys  # always available; not exposed to user code

    timings: dict[str, float] = {}

    try:
        # Write directly to stderr so the parent can see progress even if the
        # child is killed by the timeout before it puts results on the queue.
        t0 = time.monotonic()
        print("[sandbox-child] starting imports", file=_sys.stderr, flush=True)
        import matplotlib
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
        import pandas  # noqa: F401
        import numpy  # noqa: F401
        timings["imports_s"] = round(time.monotonic() - t0, 3)
        print(f"[sandbox-child] imports done in {timings['imports_s']}s", file=_sys.stderr, flush=True)

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

            t1 = time.monotonic()
            with redirect_stdout(stdout_buf):
                exec(code, namespace)  # noqa: S102
            timings["exec_s"] = round(time.monotonic() - t1, 3)

            # Capture all matplotlib figures
            t2 = time.monotonic()
            for fig_num in plt.get_fignums():
                fig = plt.figure(fig_num)
                buf = io.BytesIO()
                fig.savefig(buf, format="png", bbox_inches="tight", dpi=150)
                buf.seek(0)
                charts.append(base64.b64encode(buf.read()).decode())
            plt.close("all")
            timings["charts_s"] = round(time.monotonic() - t2, 3)

        except Exception:
            error = traceback.format_exc()
            timings["exec_s"] = timings.get("exec_s", round(time.monotonic() - t0 - timings.get("imports_s", 0), 3))

        result_queue.put({
            "success": error is None,
            "stdout": stdout_buf.getvalue(),
            "error": error or "",
            "charts": charts,
            "timings": timings,
        })

    except Exception:
        # Top-level catch: import failures or unexpected crashes. Always send
        # an error payload so the parent doesn't get the generic "no result".
        try:
            result_queue.put({
                "success": False,
                "stdout": "",
                "error": traceback.format_exc(),
                "charts": [],
                "timings": timings,
            })
        except Exception:
            pass  # queue itself is broken; parent will handle via timeout


def execute_code(code: str, timeout: int = 60) -> dict:
    """Validate and execute code in a sandboxed subprocess with timeout."""
    logger = logging.getLogger("sandbox.executor")
    timings: dict[str, float] = {}
    wall_start = time.monotonic()

    violations = validate_code(code)
    if violations:
        return {
            "success": False,
            "stdout": "",
            "error": "Code validation failed: " + "; ".join(violations),
            "charts": [],
            "timings": {},
        }

    t0 = time.monotonic()
    queue = _mp_ctx.Queue()
    proc = _mp_ctx.Process(target=_run_in_process, args=(code, queue))
    proc.start()
    timings["spawn_s"] = round(time.monotonic() - t0, 3)

    try:
        t1 = time.monotonic()
        proc.join(timeout)
        timings["join_s"] = round(time.monotonic() - t1, 3)

        if proc.is_alive():
            proc.terminate()
            proc.join(2)
            if proc.is_alive():
                proc.kill()
            timings["total_s"] = round(time.monotonic() - wall_start, 3)
            logger.warning("execution timed out", extra={"timings": timings})
            return {
                "success": False,
                "stdout": "",
                "error": (
                    f"Execution timed out after {timeout}s. "
                    "See sandbox service logs for phase (imports vs exec)."
                ),
                "charts": [],
                "timings": timings,
            }

        try:
            t2 = time.monotonic()
            result = queue.get(timeout=5)
            timings["queue_get_s"] = round(time.monotonic() - t2, 3)
        except Exception:
            timings["total_s"] = round(time.monotonic() - wall_start, 3)
            logger.error("no result from subprocess", extra={"timings": timings})
            return {
                "success": False,
                "stdout": "",
                "error": "Execution produced no result (process may have crashed)",
                "charts": [],
                "timings": timings,
            }

        # Merge child-process timings into parent timings
        child_timings = result.pop("timings", {})
        timings.update({f"child_{k}": v for k, v in child_timings.items()})
        timings["total_s"] = round(time.monotonic() - wall_start, 3)
        result["timings"] = timings

        logger.info(
            "execution complete: success=%s charts=%d",
            result["success"], len(result.get("charts", [])),
            extra={"timings": timings},
        )
        return result
    finally:
        queue.close()
        queue.join_thread()
