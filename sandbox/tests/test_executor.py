"""Tests for the sandboxed Python code executor."""

from app.executor import execute_code, validate_code


# --- AST Validation ---


class TestCodeValidator:
    """Tests for CodeValidator AST-based import checking."""

    def test_allowed_imports(self):
        code = "import pandas as pd\nimport numpy as np\nimport json"
        violations = validate_code(code)
        assert violations == []

    def test_allowed_from_imports(self):
        code = "from datetime import datetime\nfrom collections import Counter"
        violations = validate_code(code)
        assert violations == []

    def test_matplotlib_allowed(self):
        code = "import matplotlib\nimport matplotlib.pyplot as plt"
        violations = validate_code(code)
        assert violations == []

    def test_blocked_os(self):
        code = "import os"
        violations = validate_code(code)
        assert len(violations) == 1
        assert "os" in violations[0]

    def test_blocked_subprocess(self):
        code = "import subprocess"
        violations = validate_code(code)
        assert len(violations) == 1
        assert "subprocess" in violations[0]

    def test_blocked_sys(self):
        code = "import sys"
        violations = validate_code(code)
        assert len(violations) == 1
        assert "sys" in violations[0]

    def test_blocked_socket(self):
        code = "import socket"
        violations = validate_code(code)
        assert len(violations) == 1
        assert "socket" in violations[0]

    def test_blocked_shutil(self):
        code = "import shutil"
        violations = validate_code(code)
        assert len(violations) == 1
        assert "shutil" in violations[0]

    def test_blocked_ctypes(self):
        code = "import ctypes"
        violations = validate_code(code)
        assert len(violations) == 1
        assert "ctypes" in violations[0]

    def test_blocked_from_import(self):
        code = "from os.path import join"
        violations = validate_code(code)
        assert len(violations) == 1
        assert "os" in violations[0]

    def test_blocked_pickle(self):
        code = "import pickle"
        violations = validate_code(code)
        assert len(violations) == 1

    def test_blocked_http(self):
        code = "import http.client"
        violations = validate_code(code)
        assert len(violations) == 1

    def test_blocked_urllib(self):
        code = "from urllib.request import urlopen"
        violations = validate_code(code)
        assert len(violations) == 1

    def test_disallowed_unknown_module(self):
        code = "import requests"
        violations = validate_code(code)
        assert len(violations) == 1
        assert "Disallowed" in violations[0]

    def test_multiple_violations(self):
        code = "import os\nimport subprocess\nimport socket"
        violations = validate_code(code)
        assert len(violations) == 3

    def test_syntax_error(self):
        code = "def foo(:"
        violations = validate_code(code)
        assert len(violations) == 1
        assert "Syntax error" in violations[0]

    def test_empty_code(self):
        code = ""
        violations = validate_code(code)
        assert violations == []

    def test_no_imports(self):
        code = "x = 42\nprint(x)"
        violations = validate_code(code)
        assert violations == []

    def test_mixed_allowed_blocked(self):
        code = "import pandas\nimport os"
        violations = validate_code(code)
        assert len(violations) == 1
        assert "os" in violations[0]


# --- Code Execution ---


class TestExecuteCode:
    """Tests for execute_code sandboxed execution."""

    def test_simple_print(self):
        result = execute_code("print('hello world')")
        assert result["success"] is True
        assert "hello world" in result["stdout"]
        assert result["error"] == ""
        assert result["charts"] == []

    def test_math_computation(self):
        result = execute_code("import math\nprint(math.sqrt(144))")
        assert result["success"] is True
        assert "12.0" in result["stdout"]

    def test_pandas_usage(self):
        code = """
import pandas as pd
df = pd.DataFrame({'a': [1, 2, 3], 'b': [4, 5, 6]})
print(df.sum().to_dict())
"""
        result = execute_code(code)
        assert result["success"] is True
        assert "a" in result["stdout"]

    def test_numpy_usage(self):
        code = "import numpy as np\nprint(np.mean([1, 2, 3, 4, 5]))"
        result = execute_code(code)
        assert result["success"] is True
        assert "3.0" in result["stdout"]

    def test_chart_generation(self):
        code = """
import matplotlib.pyplot as plt
plt.figure()
plt.plot([1, 2, 3], [4, 5, 6])
plt.title('Test Chart')
"""
        result = execute_code(code)
        assert result["success"] is True
        assert len(result["charts"]) == 1
        # Chart should be base64-encoded PNG
        import base64
        decoded = base64.b64decode(result["charts"][0])
        assert decoded[:4] == b"\x89PNG"

    def test_multiple_charts(self):
        code = """
import matplotlib.pyplot as plt
plt.figure()
plt.plot([1, 2], [3, 4])
plt.figure()
plt.bar([1, 2], [3, 4])
"""
        result = execute_code(code)
        assert result["success"] is True
        assert len(result["charts"]) == 2

    def test_blocked_import_at_validation(self):
        result = execute_code("import os\nos.listdir('.')")
        assert result["success"] is False
        assert "Blocked import" in result["error"]
        assert result["charts"] == []

    def test_runtime_import_blocked(self):
        # __import__ is intercepted at runtime too
        result = execute_code("m = __import__('os')")
        # This should be caught: either by AST or by restricted import hook
        assert result["success"] is False

    def test_open_builtin_removed(self):
        result = execute_code("f = open('/etc/passwd', 'r')")
        assert result["success"] is False

    def test_eval_builtin_removed(self):
        result = execute_code("eval('1+1')")
        assert result["success"] is False

    def test_exec_builtin_removed(self):
        result = execute_code("exec('print(1)')")
        assert result["success"] is False

    def test_exception_returns_traceback(self):
        result = execute_code("raise ValueError('test error')")
        assert result["success"] is False
        assert "ValueError" in result["error"]
        assert "test error" in result["error"]

    def test_undefined_variable(self):
        result = execute_code("print(undefined_var)")
        assert result["success"] is False
        assert "NameError" in result["error"]

    def test_timeout(self):
        code = "while True: pass"
        result = execute_code(code, timeout=2)
        assert result["success"] is False
        assert "timed out" in result["error"]

    def test_statistics_module(self):
        code = "import statistics\nprint(statistics.mean([1, 2, 3, 4, 5]))"
        result = execute_code(code)
        assert result["success"] is True
        assert "3" in result["stdout"]

    def test_json_module(self):
        code = "import json\nprint(json.dumps({'key': 'value'}))"
        result = execute_code(code)
        assert result["success"] is True
        assert "key" in result["stdout"]

    def test_re_module(self):
        code = "import re\nprint(re.findall(r'\\d+', 'abc123def456'))"
        result = execute_code(code)
        assert result["success"] is True
        assert "123" in result["stdout"]

    def test_datetime_module(self):
        code = "from datetime import datetime\nprint(datetime(2024, 1, 1).isoformat())"
        result = execute_code(code)
        assert result["success"] is True
        assert "2024" in result["stdout"]

    def test_collections_module(self):
        code = "from collections import Counter\nprint(Counter([1,1,2,3]).most_common())"
        result = execute_code(code)
        assert result["success"] is True


# --- Forkserver / realistic workload ---


class TestForkserverPerformance:
    """Integration tests verifying forkserver preload keeps imports fast."""

    def test_matplotlib_pandas_chart_completes_quickly(self):
        """Realistic LLM-generated code similar to the Brent crude oil query.

        With spawn, this would take 60-120s on a cold 1-vCPU Cloud Run
        instance because matplotlib+pandas+numpy are re-imported from
        scratch.  With forkserver preload, imports should be sub-second.
        """
        code = """
import pandas as pd
import numpy as np
import matplotlib.pyplot as plt
from datetime import datetime, timedelta

# Simulated Brent crude oil price data (embedded, no network access)
dates = pd.date_range(start="2026-02-01", periods=50, freq="B")
np.random.seed(42)
prices = 75.0 + np.cumsum(np.random.randn(50) * 0.8)

df = pd.DataFrame({"date": dates, "price": prices})

plt.figure(figsize=(12, 6))
plt.plot(df["date"], df["price"], linewidth=2, color="#2196F3")
plt.fill_between(df["date"], df["price"], alpha=0.1, color="#2196F3")
plt.title("Brent Crude Oil Price (from 1 Feb 2026)", fontsize=14)
plt.xlabel("Date")
plt.ylabel("Price (USD/barrel)")
plt.grid(True, alpha=0.3)
plt.tight_layout()

print(f"Mean price: ${df['price'].mean():.2f}")
print(f"Min: ${df['price'].min():.2f}, Max: ${df['price'].max():.2f}")
"""
        result = execute_code(code, timeout=30)

        assert result["success"] is True, f"execution failed: {result['error']}"
        assert len(result["charts"]) == 1
        assert "Mean price" in result["stdout"]

        # Validate via returned timing metrics (not wall-clock in the test
        # process) to avoid flakiness on slow/loaded CI runners.
        timings = result.get("timings", {})
        assert "child_imports_s" in timings, f"missing child_imports_s in {timings}"
        assert "total_s" in timings, f"missing total_s in {timings}"
        assert timings["child_imports_s"] < 10, (
            f"child imports took {timings['child_imports_s']}s — "
            f"forkserver preload should make this sub-second"
        )
        assert timings["total_s"] < 25, (
            f"reported total runtime was {timings['total_s']}s — "
            f"executor performance may have regressed"
        )

    def test_timings_include_all_phases(self):
        """Verify the timing instrumentation returns all expected phases."""
        result = execute_code("print('hello')")
        assert result["success"] is True
        timings = result.get("timings", {})
        expected_keys = {"spawn_s", "join_s", "queue_get_s", "total_s",
                         "child_imports_s", "child_exec_s"}
        missing = expected_keys - set(timings.keys())
        assert not missing, f"missing timing keys: {missing} (got: {set(timings.keys())})"
