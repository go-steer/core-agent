#!/usr/bin/env python3
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""The approval sink — the witness for the gated-daemon UAT (#647).

This is deliberately the dumbest possible HTTP server: it accepts any
POST, and prints one JSON line per request to stdout. `kubectl logs`
is then the evidence, and `run.sh` reads its assertions out of that log
rather than out of the daemon's.

Why a receiver rather than the daemon's own log. The daemon logs
"approval notification sent" when `alert.Sender.Send` returns nil, which
is a claim about a function call, not about a delivery. #647's whole
premise is that an operator who cannot watch the console must be TOLD;
"we called Send and it did not error" is the system reporting on itself,
and the same argument that made the #652 evals grade the world instead
of the transcript applies here. Silence is zero deliveries, and only the
recipient can say whether one arrived.

Two paths, because two different things arrive here and conflating them
would make the central assertion unfalsifiable:

  POST /approval      the gate's out-of-band notification: somebody has
                      to approve something. Carries request_id, tool,
                      session and the respond instruction.
  POST /agent-alert   the agent's OWN alert tool firing. This is the
                      gated action itself, so its arrival is the proof
                      that the approval actually released the call, and
                      its ABSENCE is the proof that a denial or an
                      expiry actually stopped it.

Anything else gets 404, so a misrouted target shows up as a missing
assertion rather than as a passing one.
"""

import http.server
import json
import sys
import time

PATHS = ("/approval", "/agent-alert")


class Handler(http.server.BaseHTTPRequestHandler):
    # The base class logs one line per request to stderr in Apache
    # format. That interleaves with our JSON on the same kubectl logs
    # stream and makes the log unparseable, so it is silenced and the
    # JSON line below is the only output.
    def log_message(self, fmt, *args):  # noqa: A003 - base class name
        pass

    def do_POST(self):  # noqa: N802 - base class name
        if self.path not in PATHS:
            self.send_error(404, "unknown sink path %r" % self.path)
            return
        try:
            n = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            n = 0
        raw = self.rfile.read(n) if n else b""
        # The body is echoed BOTH parsed and raw. Parsed is what the
        # assertions read; raw is what an operator reads when an
        # assertion fails and the question becomes "what did it
        # actually send". A sink that only keeps its own parse of the
        # payload cannot answer that.
        try:
            body = json.loads(raw.decode("utf-8"))
        except Exception:
            body = None
        line = {
            "at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "path": self.path,
            "body": body,
            "raw": raw.decode("utf-8", "replace"),
        }
        print(json.dumps(line, sort_keys=True), flush=True)
        self.send_response(204)
        self.end_headers()

    def do_GET(self):  # noqa: N802 - base class name
        # A liveness path, so the Deployment can have a probe and the
        # UAT can tell "the sink is not up yet" from "the notification
        # never came" — which are the same observation otherwise, and
        # the wrong one of the two is the one that looks like a pass.
        if self.path == "/healthz":
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.end_headers()
            self.wfile.write(b"ok\n")
            return
        self.send_error(404, "unknown sink path %r" % self.path)


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8080
    srv = http.server.ThreadingHTTPServer(("", port), Handler)
    print(json.dumps({"at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                      "path": "-", "body": None,
                      "raw": "approval sink listening on :%d" % port}),
          flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
