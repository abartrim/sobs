"""
Minimal browser demo app for SOBS RUM replay/artifact testing.

Run manually:
    SOBS_BASE_URL=http://127.0.0.1:44317 EXAMPLE_APP_PORT=5005 python examples/python/rum_replay_test_app.py
"""

import base64
import binascii
import hashlib
import hmac
import json
import logging
import os
import time
import urllib.error
import urllib.parse
import urllib.request

from flask import Flask, jsonify, render_template_string, request

app = Flask(__name__)
logger = logging.getLogger(__name__)

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


def _post_json_to_sobs(path: str, payload: dict) -> dict:
    url = f"{SOBS_BASE_URL}{path}"
    headers = {"Content-Type": "application/json"}
    if SOBS_API_KEY:
        headers["X-API-Key"] = SOBS_API_KEY
    req = urllib.request.Request(
        url,
        data=json.dumps(payload, ensure_ascii=False).encode("utf-8"),
        headers=headers,
        method="POST",
    )
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
      <button data-demo-action="console">Console warn/error</button>
      <button class="alt" data-demo-action="breadcrumb">Add breadcrumb</button>
      <button data-demo-action="unhandled">Unhandled rejection</button>
      <button data-demo-action="throw">Throw uncaught error</button>
    </div>

    <div class="row">
      <button data-demo-action="capture">Capture exception()</button>
      <button class="alt" data-demo-action="replay">Replay + screenshot + capture</button>
      <button data-demo-action="fetch-fail">Failed fetch breadcrumb</button>
      <button class="alt" data-demo-action="clear-session">Clear session + reload</button>
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
      clientAuthTokenUrl: '/internal/sobs/rum-client-token',
      trackSPA: true,
      replay: {
        enabled: true,
        scriptUrl: 'https://cdn.jsdelivr.net/npm/rrweb@latest/dist/record/rrweb-record.min.js',
        screenshot: {
          enabled: true,
          scriptUrl: 'https://cdn.jsdelivr.net/npm/html2canvas@1.4.1/dist/html2canvas.min.js',
          mimeType: 'image/jpeg',
          quality: 0.7,
          maxEdge: 1400
        },
        maxEvents: 500,
        upload: async function (envelope) {
          const replayResp = await fetch('/api/replay/upload', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(envelope)
          });
          const upload = await replayResp.json();
          if (!replayResp.ok) {
            throw new Error(upload.error || ('Replay upload failed: ' + replayResp.status));
          }
          return upload;
        }
      }
    });

    const actions = {
      console: function () {
        console.warn('demo warn: user clicked console button');
        console.error('demo error: simulated widget failure');
      },
      breadcrumb: function () {
        SOBS.addBreadcrumb('demo.action', 'Manual demo breadcrumb', { page: location.pathname });
        alert('Breadcrumb added');
      },
      unhandled: function () {
        Promise.reject(new Error('demo unhandled rejection'));
      },
      throw: function () {
        setTimeout(function () {
          throw new Error('demo uncaught error');
        }, 0);
      },
      capture: function () {
        SOBS.captureException(new Error('demo captureException path'), {
          errorSource: 'captureException'
        });
        alert('captureException event sent');
      },
      replay: function () {
        SOBS.captureException(new Error('demo replay+artifact event'), {
          errorSource: 'captureException'
        });
        alert('Replay + screenshot context attached to error event');
      },
      'fetch-fail': async function () {
        try {
          await fetch('/api/fail', { method: 'GET' });
        } catch (e) {
          console.error('fetch failed as expected', e);
        }
      },
      'clear-session': async function () {
        // Clear browser-side state so a fresh RUM session is created on reload.
        try {
          localStorage.clear();
        } catch (e) {
          console.warn('localStorage clear failed', e);
        }
        try {
          sessionStorage.clear();
        } catch (e) {
          console.warn('sessionStorage clear failed', e);
        }

        try {
          document.cookie.split(';').forEach(function (cookie) {
            var eqPos = cookie.indexOf('=');
            var name = (eqPos > -1 ? cookie.slice(0, eqPos) : cookie).trim();
            if (!name) return;
            document.cookie = name + '=;expires=Thu, 01 Jan 1970 00:00:00 GMT;path=/';
          });
        } catch (e) {
          console.warn('cookie clear failed', e);
        }

        if (window.indexedDB && indexedDB.databases) {
          try {
            var dbs = await indexedDB.databases();
            await Promise.all((dbs || []).map(function (db) {
              if (!db || !db.name) return Promise.resolve();
              return new Promise(function (resolve) {
                var req = indexedDB.deleteDatabase(db.name);
                req.onsuccess = function () { resolve(); };
                req.onerror = function () { resolve(); };
                req.onblocked = function () { resolve(); };
              });
            }));
          } catch (e) {
            console.warn('indexedDB clear failed', e);
          }
        }

        location.reload();
      }
    };

    document.addEventListener('click', function (evt) {
      const btn = evt.target.closest('button[data-demo-action]');
      if (!btn) return;
      const action = btn.getAttribute('data-demo-action') || '';
      const handler = actions[action];
      if (typeof handler !== 'function') return;
      Promise.resolve(handler()).catch(function (err) {
        console.error('demo action failed', err);
      });
    });
  </script>
</body>
</html>
"""


@app.route("/")
def index():
    return render_template_string(PAGE, sobs_base=SOBS_BASE_URL)


@app.route("/internal/sobs/rum-client-token", methods=["POST"])
def issue_rum_client_token():
    origin = request.headers.get("Origin") or request.host_url.rstrip("/")
    try:
        data = _post_json_to_sobs(
            "/v1/rum/client-token",
            {
                "appName": "rum-replay-demo",
                "origin": origin,
            },
        )
        token = str(data.get("token") or "")
        if not token:
            return jsonify(data), 200
        return jsonify({"token": token, "expiresAt": data.get("expiresAt"), "origin": data.get("origin")}), 200
    except urllib.error.HTTPError as exc:
        return jsonify({"error": f"token request failed with HTTP {exc.code}"}), 502
    except Exception:
        logger.exception("token request failed")
        return jsonify({"error": "token request failed"}), 500


@app.route("/api/replay/upload", methods=["POST"])
def replay_upload():
    try:
        payload = {}
        try:
            payload = json.loads((request.get_data(cache=False) or b"{}").decode("utf-8"))
        except Exception:
            payload = {}

        events = payload.get("events") if isinstance(payload, dict) else None
        if not isinstance(events, list) or not events:
            events = [{"type": "meta", "ts": int(time.time() * 1000)}]

        replay_payload = json.dumps(
            {
                "provider": "rrweb",
                "events": events[-500:],
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
        screenshot = payload.get("screenshot") if isinstance(payload, dict) else None
        if isinstance(screenshot, dict):
            data_url = str(screenshot.get("dataUrl") or "")
            if data_url.startswith("data:") and ";base64," in data_url:
                meta, encoded = data_url.split(",", 1)
                content_type = meta[5:].split(";", 1)[0] or "image/png"
                extension = "png"
                if content_type == "image/jpeg":
                    extension = "jpg"
                elif content_type == "image/webp":
                    extension = "webp"
                try:
                    screenshot_bytes = base64.b64decode(encoded, validate=True)
                except (binascii.Error, ValueError):
                    screenshot_bytes = b""
                if screenshot_bytes:
                    artifact = _upload_asset_to_sobs(
                        screenshot_bytes,
                        asset_type="screenshot",
                        asset_name=f"capture-{int(time.time())}.{extension}",
                        content_type=content_type,
                    )

        if artifact is None:
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
    except Exception:
        logger.exception("asset upload failed")
        return jsonify({"error": "asset upload failed"}), 500


@app.route("/api/fail", methods=["GET"])
def fail():
    return jsonify({"error": "simulated failure"}), 503


if __name__ == "__main__":
    app.run(host="127.0.0.1", port=EXAMPLE_APP_PORT, debug=False)
