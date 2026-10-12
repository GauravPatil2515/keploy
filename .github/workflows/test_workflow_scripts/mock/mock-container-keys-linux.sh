#!/usr/bin/env bash
# e2e for container keys (the proven model, KEPLOY_PROVEN=1): `keploy mock
# record` names a dependency that runs in a container by the container, not by
# the random port Docker published it on.
#
# Three containers of one image play a Testcontainers setup: A is the suite's
# (called while the tests are collected, outside every test), B is test_one's
# own and C is test_two's own. Each is published on a random host port. The
# recording must carry, on each call's mock:
#   - A, outside tests and inside test_two:  container:<image>:8000#1
#   - B inside test_one, C inside test_two:  container:<image>:8000#2
# so a test's own container is numbered the same in every test, after the
# suite's. The containers are then replaced by new ones on new random ports and
# the recording replays. A recording made without the switch has no keys, and
# the switch's recording replays without the switch.
#
# Env: RECORD_BIN / REPLAY_BIN (the keploy binaries under test); DEP_IMAGE (the
# dependency's image, python:3.11-slim by default).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

if ! python3 -c "import pytest" 2>/dev/null; then
  python3 -m ensurepip --default-pip >/dev/null 2>&1 || true
  python3 -m pip install --quiet --disable-pip-version-check pytest
fi

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-keploy}"
IMAGE="${DEP_IMAGE:-python:3.11-slim}"
WORK="$(mktemp -d)"
cd "$WORK"
FAIL=0
CONTAINERS=()

cleanup() {
  [ "${#CONTAINERS[@]}" -gt 0 ] && docker rm -f "${CONTAINERS[@]}" >/dev/null 2>&1
  return 0
}
trap cleanup EXIT

docker pull -q "$IMAGE" >/dev/null || { echo "FAIL: cannot pull $IMAGE"; exit 1; }

# The dependency: answers every GET with which container it is and the path.
# It says "ready" once it listens, so the script waits for that line, not for
# a clock.
SERVER='
import http.server, json, os
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        b = json.dumps({"who": os.environ["WHO"], "path": self.path}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)
    def log_message(self, *a): pass
s = http.server.HTTPServer(("0.0.0.0", 8000), H)
print("ready", flush=True)
s.serve_forever()
'

# wait_ready ID: reads the container's log until its "ready" line (0), or
# until the log ends because the container exited first (1).
wait_ready() {
  local line ok=1
  coproc LOGS { docker logs -f "$1" 2>&1; }
  while IFS= read -r line <&"${LOGS[0]}"; do
    if [ "$line" = ready ]; then ok=0; break; fi
  done
  kill "$LOGS_PID" 2>/dev/null
  wait "$LOGS_PID" 2>/dev/null
  return $ok
}

# start_deps writes ports.json: each of A, B and C on a new random host port.
start_deps() {
  cleanup
  CONTAINERS=()
  : > ports.env
  for who in a b c; do
    id=$(docker run -d -e WHO="$who" -p 127.0.0.1::8000 "$IMAGE" python3 -c "$SERVER") || { echo "FAIL: cannot start container $who"; exit 1; }
    CONTAINERS+=("$id")
    wait_ready "$id" || { echo "FAIL: container $who exited before it listened"; docker logs "$id"; exit 1; }
    port=$(docker port "$id" 8000/tcp | grep -m1 '^127\.0\.0\.1:' | cut -d: -f2)
    [ -n "$port" ] || { echo "FAIL: container $who has no published port"; exit 1; }
    echo "$who=$port" >> ports.env
  done
  python3 - <<'PY'
import json
print(json.dumps(dict(l.strip().split("=") for l in open("ports.env") if l.strip())))
json.dump(dict(l.strip().split("=") for l in open("ports.env") if l.strip()), open("ports.json", "w"))
PY
}

cat > conftest.py <<'PY'
import os, json, urllib.request, pytest
AGENT=os.environ.get("KEPLOY_MOCK_AGENT")
TOKEN=os.environ.get("KEPLOY_MOCK_AGENT_TOKEN")
def _post(p,b):
    if not AGENT: return None
    h={"Content-Type":"application/json"}
    if TOKEN: h["Authorization"]="Bearer "+TOKEN
    r=urllib.request.Request(AGENT+p,data=json.dumps(b).encode(),headers=h,method="POST")
    return json.loads(urllib.request.urlopen(r,timeout=30).read() or b"{}")
@pytest.fixture(autouse=True)
def keploy_scope(request):
    resp=_post("/agent/scope/begin",{"name":request.node.name,"canSkip":True})
    if resp and resp.get("action")=="skip":
        pytest.skip(resp.get("reason") or "keploy: not run this replay")
    yield
    _post("/agent/scope/end",{"name":request.node.name,"outcome":"passed"})
PY

cat > deps.py <<'PY'
import json, urllib.request
PORTS = json.load(open("ports.json"))
def get(who, path):
    # urllib opens one connection per call, so every call is its own
    # connection, numbered in the scope it is made in.
    with urllib.request.urlopen("http://127.0.0.1:%s%s" % (PORTS[who], path), timeout=30) as r:
        return json.load(r)
PY

cat > test_containers.py <<'PY'
from deps import get
# The suite's container, called while the tests are collected: outside tests.
BOOT = get("a", "/boot")
def test_one():
    assert BOOT["who"] == "a"
    assert get("b", "/one")["who"] == "b"
def test_two():
    assert get("c", "/two-c")["who"] == "c"
    assert get("a", "/two-a")["who"] == "a"
PY

CMD="python3 -m pytest -q -p no:cacheprovider test_containers.py"

# keys_of NAME: "<path> <containerKey or ->" for each HTTP mock of the set.
keys_of() {
  python3 - "keploy/$1/mocks.yaml" <<'PY'
import re, sys
try:
    docs = open(sys.argv[1]).read().split("\n---")
except FileNotFoundError:
    docs = []
for d in docs:
    if not re.search(r"^kind: Http$", d, re.M):
        continue
    url = re.search(r"^\s+url: (\S+)$", d, re.M)
    key = re.search(r"^\s+containerKey: (\S+)$", d, re.M)
    path = re.sub(r"^https?://[^/]+", "", url.group(1)) if url else "?"
    print(path, key.group(1) if key else "-")
PY
}

echo "== 1. record under the proven switch =="
start_deps
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" mock record -c "$CMD" --name ck --disable-tele 2>&1 | tee rec.log
RC=${PIPESTATUS[0]}
[ "$RC" -eq 0 ] || { echo "FAIL: record exited $RC"; FAIL=1; }
grep -q "2 passed" rec.log || { echo "FAIL: pytest did not pass both tests while recording"; FAIL=1; }
keys_of ck | sort | tee keys.txt
want="container:${IMAGE}:8000"
cat > want.txt <<EOF
/boot ${want}#1
/one ${want}#2
/two-a ${want}#1
/two-c ${want}#2
EOF
if ! diff -u want.txt keys.txt; then
  echo "FAIL: the recorded container keys are not the suite's #1 and each test's own #2"
  FAIL=1
fi

echo "== 2. replay against new containers on new random ports =="
start_deps
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$REPLAY_BIN" mock replay -c "$CMD" --name ck --disable-tele 2>&1 | tee rep.log
RC=${PIPESTATUS[0]}
[ "$RC" -eq 0 ] || { echo "FAIL: replay under the switch exited $RC"; FAIL=1; }
grep -q "2 passed" rep.log || { echo "FAIL: pytest did not pass both tests in replay"; FAIL=1; }

echo "== 3. the switch's recording replays without the switch =="
start_deps
sudo -E env PATH="$PATH" KEPLOY_PROVEN= "$REPLAY_BIN" mock replay -c "$CMD" --name ck --disable-tele 2>&1 | tee rep-off.log
RC=${PIPESTATUS[0]}
[ "$RC" -eq 0 ] || { echo "FAIL: replay without the switch exited $RC"; FAIL=1; }
grep -q "2 passed" rep-off.log || { echo "FAIL: pytest did not pass both tests in the replay without the switch"; FAIL=1; }

echo "== 4. without the switch nothing is keyed =="
start_deps
sudo -E env PATH="$PATH" KEPLOY_PROVEN= "$RECORD_BIN" mock record -c "$CMD" --name ck-off --disable-tele 2>&1 | tee rec-off.log
RC=${PIPESTATUS[0]}
[ "$RC" -eq 0 ] || { echo "FAIL: record without the switch exited $RC"; FAIL=1; }
keys_of ck-off | sort | tee keys-off.txt
[ "$(wc -l < keys-off.txt)" -eq 4 ] || { echo "FAIL: expected 4 HTTP mocks without the switch"; FAIL=1; }
if grep -v ' -$' keys-off.txt; then
  echo "FAIL: a recording made without the switch carries container keys"
  FAIL=1
fi

if [ "$FAIL" -ne 0 ]; then
  echo "container keys e2e FAILED"
  exit 1
fi
echo "container keys e2e passed"
