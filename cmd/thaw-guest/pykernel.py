# A persistent Python interpreter for thaw-guest. One JSON line in, one
# JSON line out. Variables live on between requests, which is the point:
# the snapshot captures them, and a restored VM answers without rebuilding.
import contextlib
import io
import json
import sys
import traceback

env = {"__name__": "__thaw__"}

for line in sys.stdin:
    req = json.loads(line)
    buf = io.StringIO()
    ok, err = True, ""
    try:
        with contextlib.redirect_stdout(buf):
            exec(compile(req["code"], "<thaw>", "exec"), env)
    except Exception:
        ok, err = False, traceback.format_exc(limit=3)
    sys.stdout.write(json.dumps({"ok": ok, "out": buf.getvalue(), "err": err}) + "\n")
    sys.stdout.flush()
