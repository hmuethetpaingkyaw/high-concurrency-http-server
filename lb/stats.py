"""Print HAProxy's per-instance numbers (make lbstats). Reads the stats CSV on :8404."""
import csv
import sys
import urllib.request

URL = "http://localhost:8404/stats;csv"
COLUMNS = ("svname", "status", "scur", "stot", "hrsp_2xx", "hrsp_5xx", "econ", "wretr", "qcur", "qmax", "chkfail")

try:
    text = urllib.request.urlopen(URL, timeout=3).read().decode()
except OSError as e:
    sys.exit(f"cannot reach {URL}: {e} (is the load balancer up? make up)")

lines = text.splitlines()
lines[0] = lines[0].lstrip("# ")  # the header line starts with "# "
rows = [r for r in csv.DictReader(lines) if r["pxname"] == "apps"]

print(*(f"{c:>9}" for c in COLUMNS))
for r in rows:
    print(*(f"{r.get(c, ''):>9}" for c in COLUMNS))
print("\nscur = in flight now, stot = total sessions, econ = failed connects, wretr = retries, qcur/qmax = queued")
