"""Reproduction tests for the Brent crude oil charting timeout.

The deployed sandbox times out (60s) when plotting 19 data points but
succeeds with 3. These tests run with a shorter timeout to fail fast
locally instead of waiting a full minute.
"""

import pytest
from app.executor import execute_code


# --- Exact code that WORKS on the deployed sandbox (3 points) ---
WORKING_CODE_3_POINTS = """\
import pandas as pd
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

prices = [61, 100, 106.41]
plt.figure(figsize=(12, 6))
plt.plot([0, 1, 2], prices, marker="o")
plt.title("Test")
print("done")
"""

# --- Exact code that HANGS on the deployed sandbox (19 points) ---
HANGING_CODE_19_POINTS = """\
import pandas as pd
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

prices = [61, 100, 106.41, 95.92, 100.23, 102.22, 108.01, 112.57, 112.78, 118.35, 101.16, 109.03, 109.77, 109.27, 94.75, 95.92, 95.20, 103.11, 98.03]
indices = list(range(len(prices)))

plt.figure(figsize=(12, 6))
plt.plot(indices, prices, marker="o")
plt.title("Test")
print("done")
"""

# --- Full chart code the LLM would typically generate ---
FULL_CHART_CODE = """\
import pandas as pd
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from datetime import datetime

data = {
    "date": ["2026-01-01", "2026-03-12", "2026-03-20", "2026-03-23", "2026-03-24",
             "2026-03-25", "2026-03-26", "2026-03-27", "2026-03-30", "2026-03-31",
             "2026-04-01", "2026-04-02", "2026-04-06", "2026-04-07", "2026-04-08",
             "2026-04-09", "2026-04-10", "2026-04-12", "2026-04-13"],
    "price": [61, 100, 106.41, 95.92, 100.23, 102.22, 108.01, 112.57, 112.78, 118.35,
              101.16, 109.03, 109.77, 109.27, 94.75, 95.92, 95.20, 103.11, 98.03]
}

df = pd.DataFrame(data)
df["date"] = pd.to_datetime(df["date"])

plt.figure(figsize=(12, 6))
plt.plot(df["date"], df["price"], marker="o", linewidth=2, color="darkblue")
plt.title("Brent Crude Oil Price Since January 1, 2026")
plt.xlabel("Date")
plt.ylabel("Price (USD/barrel)")
plt.grid(True, alpha=0.3)
plt.tight_layout()
print("Chart generated successfully")
"""

# --- Narrowing: is it pd.to_datetime or date-axis rendering? ---
DATES_NO_PLOT = """\
import pandas as pd
from datetime import datetime

dates = ["2026-01-01", "2026-03-12", "2026-03-20", "2026-03-23", "2026-03-24",
         "2026-03-25", "2026-03-26", "2026-03-27", "2026-03-30", "2026-03-31",
         "2026-04-01", "2026-04-02", "2026-04-06", "2026-04-07", "2026-04-08",
         "2026-04-09", "2026-04-10", "2026-04-12", "2026-04-13"]
prices = [61, 100, 106.41, 95.92, 100.23, 102.22, 108.01, 112.57, 112.78, 118.35,
          101.16, 109.03, 109.77, 109.27, 94.75, 95.92, 95.20, 103.11, 98.03]
df = pd.DataFrame({"date": pd.to_datetime(dates), "price": prices})
print(df.to_string())
"""

DATE_AXIS_PLOT = """\
import pandas as pd
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

dates = pd.to_datetime(["2026-01-01", "2026-03-12", "2026-03-20", "2026-03-23",
    "2026-03-24", "2026-03-25", "2026-03-26", "2026-03-27", "2026-03-30",
    "2026-03-31", "2026-04-01", "2026-04-02", "2026-04-06", "2026-04-07",
    "2026-04-08", "2026-04-09", "2026-04-10", "2026-04-12", "2026-04-13"])
prices = [61, 100, 106.41, 95.92, 100.23, 102.22, 108.01, 112.57, 112.78, 118.35,
          101.16, 109.03, 109.77, 109.27, 94.75, 95.92, 95.20, 103.11, 98.03]
plt.figure(figsize=(12, 6))
plt.plot(dates, prices, marker="o")
plt.title("Test")
print("done")
"""

# Same as DATE_AXIS_PLOT but with tight_layout
DATE_AXIS_TIGHT = """\
import pandas as pd
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

dates = pd.to_datetime(["2026-01-01", "2026-03-12", "2026-03-20", "2026-03-23",
    "2026-03-24", "2026-03-25", "2026-03-26", "2026-03-27", "2026-03-30",
    "2026-03-31", "2026-04-01", "2026-04-02", "2026-04-06", "2026-04-07",
    "2026-04-08", "2026-04-09", "2026-04-10", "2026-04-12", "2026-04-13"])
prices = [61, 100, 106.41, 95.92, 100.23, 102.22, 108.01, 112.57, 112.78, 118.35,
          101.16, 109.03, 109.77, 109.27, 94.75, 95.92, 95.20, 103.11, 98.03]
plt.figure(figsize=(12, 6))
plt.plot(dates, prices, marker="o")
plt.title("Test")
plt.tight_layout()
print("done")
"""

# Same but with xlabel/ylabel
DATE_AXIS_LABELS = """\
import pandas as pd
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

dates = pd.to_datetime(["2026-01-01", "2026-03-12", "2026-03-20", "2026-03-23",
    "2026-03-24", "2026-03-25", "2026-03-26", "2026-03-27", "2026-03-30",
    "2026-03-31", "2026-04-01", "2026-04-02", "2026-04-06", "2026-04-07",
    "2026-04-08", "2026-04-09", "2026-04-10", "2026-04-12", "2026-04-13"])
prices = [61, 100, 106.41, 95.92, 100.23, 102.22, 108.01, 112.57, 112.78, 118.35,
          101.16, 109.03, 109.77, 109.27, 94.75, 95.92, 95.20, 103.11, 98.03]
plt.figure(figsize=(12, 6))
plt.plot(dates, prices, marker="o")
plt.title("Test")
plt.xlabel("Date")
plt.ylabel("Price (USD/barrel)")
print("done")
"""

# Same but with grid
DATE_AXIS_GRID = """\
import pandas as pd
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

dates = pd.to_datetime(["2026-01-01", "2026-03-12", "2026-03-20", "2026-03-23",
    "2026-03-24", "2026-03-25", "2026-03-26", "2026-03-27", "2026-03-30",
    "2026-03-31", "2026-04-01", "2026-04-02", "2026-04-06", "2026-04-07",
    "2026-04-08", "2026-04-09", "2026-04-10", "2026-04-12", "2026-04-13"])
prices = [61, 100, 106.41, 95.92, 100.23, 102.22, 108.01, 112.57, 112.78, 118.35,
          101.16, 109.03, 109.77, 109.27, 94.75, 95.92, 95.20, 103.11, 98.03]
plt.figure(figsize=(12, 6))
plt.plot(dates, prices, marker="o")
plt.title("Test")
plt.grid(True, alpha=0.3)
print("done")
"""

# Short timeout so we don't wait 60s for a hang.
SHORT_TIMEOUT = 15


class TestBrentChartReproduction:
    """Reproduce the sandbox hang with 19-point Brent crude chart."""

    def test_3_points_succeeds(self):
        """Baseline: small plot works fine."""
        result = execute_code(WORKING_CODE_3_POINTS, timeout=SHORT_TIMEOUT)
        assert result["success"] is True, f"Expected success, got error: {result['error']}"
        assert "done" in result["stdout"]
        assert len(result["charts"]) == 1

    def test_19_points_succeeds(self):
        """19 points with integer x-axis works."""
        result = execute_code(HANGING_CODE_19_POINTS, timeout=SHORT_TIMEOUT)
        assert result["success"] is True, f"Expected success, got error: {result['error']}"
        assert "done" in result["stdout"]
        assert len(result["charts"]) == 1

    def test_dates_no_plot(self):
        """pd.to_datetime + DataFrame without plotting."""
        result = execute_code(DATES_NO_PLOT, timeout=SHORT_TIMEOUT)
        assert result["success"] is True, f"Expected success, got error: {result['error']}"

    def test_date_axis_plot(self):
        """Plotting with datetime x-axis — baseline."""
        result = execute_code(DATE_AXIS_PLOT, timeout=SHORT_TIMEOUT)
        assert result["success"] is True, f"Expected success, got error: {result['error']}"
        assert "done" in result["stdout"]
        assert len(result["charts"]) == 1

    def test_date_axis_tight_layout(self):
        """Date axis + tight_layout."""
        result = execute_code(DATE_AXIS_TIGHT, timeout=SHORT_TIMEOUT)
        assert result["success"] is True, f"Expected success, got error: {result['error']}"
        assert "done" in result["stdout"]
        assert len(result["charts"]) == 1

    def test_date_axis_labels(self):
        """Date axis + xlabel/ylabel."""
        result = execute_code(DATE_AXIS_LABELS, timeout=SHORT_TIMEOUT)
        assert result["success"] is True, f"Expected success, got error: {result['error']}"
        assert "done" in result["stdout"]
        assert len(result["charts"]) == 1

    def test_date_axis_grid(self):
        """Date axis + grid."""
        result = execute_code(DATE_AXIS_GRID, timeout=SHORT_TIMEOUT)
        assert result["success"] is True, f"Expected success, got error: {result['error']}"
        assert "done" in result["stdout"]
        assert len(result["charts"]) == 1

    def test_full_brent_chart_succeeds(self):
        """Full LLM-generated chart with dates. Should not time out."""
        result = execute_code(FULL_CHART_CODE, timeout=SHORT_TIMEOUT)
        assert result["success"] is True, f"Expected success, got error: {result['error']}"
        assert "Chart generated successfully" in result["stdout"]
        assert len(result["charts"]) == 1
