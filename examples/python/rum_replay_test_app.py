"""
Minimal browser demo app for SOBS RUM replay/artifact testing.

Run manually:
    SOBS_BASE_URL=http://127.0.0.1:44317 EXAMPLE_APP_PORT=5005 python examples/python/rum_replay_test_app.py
"""

import hashlib
import hmac
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request

from flask import Flask, jsonify, render_template_string

app = Flask(__name__)

SOBS_BASE_URL = os.environ.get("SOBS_BASE_URL", "http://127.0.0.1:44317").rstrip("/")
EXAMPLE_APP_PORT = int(os.environ.get("EXAMPLE_APP_PORT", "5005"))
SOBS_API_KEY = os.environ.get("SOBS_API_KEY", "")
SOBS_RUM_ASSET_SIGNING_KEY = os.environ.get("SOBS_RUM_ASSET_SIGNING_KEY", "")


def _sign_asset_request(path: str, body: bytes, content_type: str, asset_type: str, asset_name: str) -> tuple[str, str]:
    timestamp = str(int(time.time()))
    payload = "\n".join(
        [
            "POST",
            path,
            timestamp,
            hashlib.sha256(body).hexdigest(),
            content_type.lower(),
            asset_type.lower(),
            asset_name,
        ]
    )
    signature = hmac.new(
        SOBS_RUM_ASSET_SIGNING_KEY.encode("utf-8"), payload.encode("utf-8"), hashlib.sha256
    ).hexdigest()
    return timestamp, signature


def _upload_asset_to_sobs(body: bytes, *, asset_type: str, asset_name: str, content_type: str) -> dict:
    if not SOBS_RUM_ASSET_SIGNING_KEY:
        raise RuntimeError("SOBS_RUM_ASSET_SIGNING_KEY is not set")

    path = "/v1/rum/assets"
    query = urllib.parse.urlencode({"type": asset_type, "name": asset_name})
    url = f"{SOBS_BASE_URL}{path}?{query}"
    ts, sig = _sign_asset_request(path, body, content_type, asset_type, asset_name)
    headers = {
        "Content-Type": content_type,
        "X-SOBS-Asset-Timestamp": ts,
        "X-SOBS-Asset-Signature": sig,
    }
    if SOBS_API_KEY:
        headers["X-API-Key"] = SOBS_API_KEY

    req = urllib.request.Request(url, data=body, headers=headers, method="POST")
    with urllib.request.urlopen(req, timeout=8) as resp:  # noqa: S310
        return json.loads(resp.read().decode("utf-8"))


PAGE = """
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>SOBS RUM Replay Demo</title>
  <style>
    body {
      font-family: ui-sans-serif, system-ui, sans-serif;
      margin: 2rem;
      background: #f6f9fc;
      color: #1d2430;
    }
    .card {
      background: #fff;
      border: 1px solid #dbe4ef;
      border-radius: 12px;
      padding: 1rem;
      max-width: 960px;
    }
    .row { display: flex; flex-wrap: wrap; gap: .5rem; margin-bottom: .75rem; }
    button {
      border: 1px solid #2a5bd7;
      background: #2a5bd7;
      color: #fff;
      border-radius: 8px;
      padding: .5rem .75rem;
      cursor: pointer;
    }
    button.alt { background: #fff; color: #2a5bd7; }
    code { background: #eef3fb; padding: .15rem .35rem; border-radius: 6px; }
    .muted { color: #4a5972; font-size: .95rem; }
  </style>
</head>
<body>
  <div class="card">
    <h2>SOBS RUM Replay Demo</h2>
    <p class="muted">
      This page exercises error, breadcrumb, traceparent, replay, and artifact paths in
      <code>static/rum.js</code>.
    </p>

    <div class="row">
      <button id="btn-console">Console warn/error</button>
      <button class="alt" id="btn-breadcrumb">Add breadcrumb</button>
      <button id="btn-unhandled">Unhandled rejection</button>
      <button id="btn-throw">Throw uncaught error</button>
    </div>

    <div class="row">
      <button id="btn-capture">Capture exception()</button>
      <button class="alt" id="btn-replay">Replay + screenshot + capture</button>
      <button id="btn-fetch-fail">Failed fetch breadcrumb</button>
    </div>

    <p class="muted">
      Open <code>{{ sobs_base }}/rum</code> and <code>{{ sobs_base }}/errors</code> in another tab to
      see generated events.
    </p>
  </div>

  <script src="{{ sobs_base }}/static/rum.js"></script>
  <script>
    SOBS.init({
      endpoint: '{{ sobs_base }}/v1/rum',
      appName: 'rum-replay-demo',
      trackSPA: true
    });

    // Deterministic sample IDs for easier manual validation in UI.
    const replayId = 'replay-demo-001';
    const shotId = 'shot-demo-001';

    document.getElementById('btn-console').addEventListener('click', function () {
      console.warn('demo warn: user clicked console button');
      console.error('demo error: simulated widget failure');
    });

    document.getElementById('btn-breadcrumb').addEventListener('click', function () {
      SOBS.addBreadcrumb('demo.action', 'Manual demo breadcrumb', { page: location.pathname });
      alert('Breadcrumb added');
    });

    document.getElementById('btn-unhandled').addEventListener('click', function () {
      Promise.reject(new Error('demo unhandled rejection'));
    });

    document.getElementById('btn-throw').addEventListener('click', function () {
      setTimeout(function () {
        throw new Error('demo uncaught error');
      }, 0);
    });

    document.getElementById('btn-capture').addEventListener('click', function () {
      SOBS.captureException(new Error('demo captureException path'), {
        errorSource: 'captureException'
      });
      alert('captureException event sent');
    });

    document.getElementById('btn-replay').addEventListener('click', async function () {
      const replayResp = await fetch('/api/replay/upload', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ provider: 'rrweb', events: [{ type: 'meta', ts: Date.now() }] })
      });
      const upload = await replayResp.json();

      if (!replayResp.ok) {
        alert('Replay upload failed: ' + (upload.error || replayResp.status));
        return;
      }

      SOBS.setReplayContext(
        {
          id: upload.replay.id,
          url: upload.replay.url,
          provider: upload.replay.provider
        },
        { ttlMs: 15000, consumeOnce: true }
      );
      if (upload.artifact && upload.artifact.url) {
        SOBS.setArtifactContext(
          {
            type: upload.artifact.type || 'screenshot',
            id: upload.artifact.id || shotId,
            url: upload.artifact.url
          },
          { ttlMs: 15000, consumeOnce: true }
        );
      }
      SOBS.captureException(new Error('demo replay+artifact event'), {
        errorSource: 'captureException'
      });
      alert('Replay + artifact context attached to error event');
    });

    document.getElementById('btn-fetch-fail').addEventListener('click', async function () {
      try {
        await fetch('/api/fail', { method: 'GET' });
      } catch (e) {
        console.error('fetch failed as expected', e);
      }
    });
  </script>
</body>
</html>
"""


@app.route("/")
def index():
    return render_template_string(PAGE, sobs_base=SOBS_BASE_URL)


@app.route("/api/replay/upload", methods=["POST"])
def replay_upload():
    try:
        replay_payload = json.dumps(
            {
                "provider": "rrweb",
                "events": [{"type": "meta", "ts": int(time.time() * 1000)}],
            },
            ensure_ascii=False,
        ).encode("utf-8")
        replay = _upload_asset_to_sobs(
            replay_payload,
            asset_type="replay",
            asset_name="rrweb-events.json",
            content_type="application/json",
        )

        artifact = None
        screenshot_path = os.path.join(os.path.dirname(__file__), "..", "..", "static", "help", "summary.png")
        if os.path.exists(screenshot_path):
            with open(screenshot_path, "rb") as handle:
                screenshot_bytes = handle.read()
            artifact = _upload_asset_to_sobs(
                screenshot_bytes,
                asset_type="screenshot",
                asset_name="summary.png",
                content_type="image/png",
            )

        return jsonify(
            {
                "replay": {
                    "id": replay.get("id"),
                    "url": replay.get("url"),
                    "provider": "rrweb",
                },
                "artifact": artifact,
            }
        )
    except urllib.error.HTTPError as exc:
        return jsonify({"error": f"asset upload failed with HTTP {exc.code}"}), 502
    except Exception as exc:
        return jsonify({"error": f"asset upload failed: {exc}"}), 500


@app.route("/api/fail", methods=["GET"])
def fail():
    return jsonify({"error": "simulated failure"}), 503


if __name__ == "__main__":
    app.run(host="127.0.0.1", port=EXAMPLE_APP_PORT, debug=False)
