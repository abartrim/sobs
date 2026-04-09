"""
SOBS Notifications – webhook receiver example.

This script starts a minimal HTTP server that acts as a webhook target for SOBS
notification channels, then demonstrates how to configure the channel and rules
via the SOBS API.

Usage
-----
1. Start SOBS:
       docker run -p 44317:4317 ghcr.io/abartrim/sobs:latest

2. Start this webhook receiver in a separate terminal:
       pip install flask
       python examples/notifications/webhook_example.py

3. The script prints the curl commands needed to:
   a. Create a webhook notification channel pointing at this server.
   b. Create a notification rule that fires when an error rate exceeds a
      threshold.
   c. Trigger a test notification to verify the channel.

Webhook URL caveat
------------------
- If SOBS runs in Docker, use `http://host.docker.internal:<WEBHOOK_PORT>/webhook`.
- If SOBS runs directly on your host (not in Docker), use `http://localhost:<WEBHOOK_PORT>/webhook`.

Environment variables
---------------------
SOBS_URL            Base URL of your SOBS instance (default: http://localhost:44317)
SOBS_API_KEY        Optional API key for ingest endpoints (not used by this script)
SOBS_BASIC_AUTH_USERNAME  Optional Web UI basic auth username
SOBS_BASIC_AUTH_PASSWORD  Optional Web UI basic auth password
SOBS_BEARER_TOKEN   Optional bearer token for external auth mode
WEBHOOK_PORT        Port this receiver listens on (default: 5050)
"""

import base64
import json
import os
import textwrap

try:
    from flask import Flask, jsonify
    from flask import request as flask_request

    _HAS_DEPS = True
except ImportError:
    _HAS_DEPS = False

SOBS_URL = os.environ.get("SOBS_URL", "http://localhost:44317").rstrip("/")
SOBS_API_KEY = os.environ.get("SOBS_API_KEY", "")
SOBS_BASIC_AUTH_USERNAME = os.environ.get("SOBS_BASIC_AUTH_USERNAME", "")
SOBS_BASIC_AUTH_PASSWORD = os.environ.get("SOBS_BASIC_AUTH_PASSWORD", "")
SOBS_BEARER_TOKEN = os.environ.get("SOBS_BEARER_TOKEN", "")
WEBHOOK_PORT = int(os.environ.get("WEBHOOK_PORT", "5050"))

# ---------------------------------------------------------------------------
# 1. Minimal webhook receiver (Flask)
# ---------------------------------------------------------------------------

if _HAS_DEPS:
    receiver_app = Flask("sobs-webhook-receiver")

    @receiver_app.route("/webhook", methods=["POST"])
    def receive_webhook():
        payload = flask_request.get_json(silent=True) or {}
        print("\n[WEBHOOK RECEIVED]")
        print(json.dumps(payload, indent=2))
        return jsonify({"status": "ok"})


# ---------------------------------------------------------------------------
# 2. Helper – print curl commands to configure SOBS
# ---------------------------------------------------------------------------


def _auth_header() -> str:
    if SOBS_BEARER_TOKEN:
        return f'-H "Authorization: Bearer {SOBS_BEARER_TOKEN}"'
    if SOBS_BASIC_AUTH_USERNAME and SOBS_BASIC_AUTH_PASSWORD:
        raw = f"{SOBS_BASIC_AUTH_USERNAME}:{SOBS_BASIC_AUTH_PASSWORD}".encode("utf-8")
        encoded = base64.b64encode(raw).decode("ascii")
        return f'-H "Authorization: Basic {encoded}"'
    return ""


def print_setup_commands():
    auth = _auth_header()
    webhook_url = f"http://host.docker.internal:{WEBHOOK_PORT}/webhook"

    print(textwrap.dedent(f"""
    =========================================================
    SOBS Notifications – setup commands
    =========================================================

    These commands configure SOBS to send a notification
    whenever a metric rule threshold is crossed.

    Note: notification settings routes use Web UI auth (Basic/Bearer),
    not X-API-Key. Set SOBS_BASIC_AUTH_USERNAME/SOBS_BASIC_AUTH_PASSWORD
    or SOBS_BEARER_TOKEN if your SOBS instance requires auth.

    Replace the webhook URL if you are not running inside Docker
    (use http://localhost:{WEBHOOK_PORT}/webhook for bare-metal runs).

    ---------------------------------------------------------
    Step 1 – Create a webhook notification channel
    ---------------------------------------------------------
    curl -s -X POST {SOBS_URL}/settings/notifications/channels \\
      {auth} \\
      -d "name=ops-webhook" \\
      -d "channel_type=webhook" \\
      -d "webhook_url={webhook_url}" \\
      -d "webhook_method=POST" \\
      -d 'webhook_headers={{"Content-Type":"application/json"}}' \\
      -d 'webhook_body_template={{"event":"{{{{event}}}}","service":"{{{{service}}}}","value":"{{{{value}}}}"}}' \\
      -w "\\nHTTP %{{http_code}}\\n"

    ---------------------------------------------------------
    Step 2 – Test the channel (replace CHANNEL_ID with the
          channel ID shown in Settings -> Notifications)
    ---------------------------------------------------------
    curl -s -X POST {SOBS_URL}/api/notifications/channels/<CHANNEL_ID>/test \\
      {auth} \\
      -w "\\nHTTP %{{http_code}}\\n"

    ---------------------------------------------------------
    Step 3 – Trigger a check manually
    ---------------------------------------------------------
    curl -s -X POST {SOBS_URL}/api/notifications/check \\
      {auth} \\
      -w "\\nHTTP %{{http_code}}\\n"

    =========================================================
    Notification channel types supported by SOBS
    =========================================================
    webhook     – HTTP/HTTPS endpoint with optional custom headers and body template
    slack       – Slack incoming webhook URL
    email       – SMTP (TLS supported)
    browser_push – Web Push (VAPID; requires SOBS_VAPID_PRIVATE_KEY)

    =========================================================
    Configuring rules (via the UI at /settings)
    =========================================================
    Rules evaluate conditions on metric signals and fire when:
      • A metric value exceeds (gt/gte) or falls below (lt/lte) a threshold
      • Severity: warning | critical
      • Conditions are combined with AND or OR logic
      • A per-rule cooldown prevents alert storms

    Open http://localhost:44317/settings in your browser to
    create and manage rules interactively.
    """))


# ---------------------------------------------------------------------------
# Slack example (curl only – no Python dependencies needed)
# ---------------------------------------------------------------------------

SLACK_CURL_EXAMPLE = textwrap.dedent("""
    # Create a Slack notification channel.
    # Generate an Incoming Webhook URL in your Slack app settings and substitute
    # it for <your-slack-incoming-webhook-url> below.
    curl -s -X POST {sobs_url}/settings/notifications/channels \\
  {auth} \\
      -d "name=slack-ops" \\
      -d "channel_type=slack" \\
      -d "slack_webhook_url=<your-slack-incoming-webhook-url>" \\
      -w "\\nHTTP %{{http_code}}\\n"
""")


# ---------------------------------------------------------------------------
# E-mail example
# ---------------------------------------------------------------------------

EMAIL_CURL_EXAMPLE = textwrap.dedent("""
    # Create an e-mail notification channel (TLS SMTP)
    curl -s -X POST {sobs_url}/settings/notifications/channels \\
      {auth} \\
      -d "name=email-ops" \\
      -d "channel_type=email" \\
      -d "smtp_host=smtp.example.com" \\
      -d "smtp_port=587" \\
      -d "smtp_user=alerts@example.com" \\
      -d "smtp_password=<your-smtp-password>" \\
      -d "from_addr=alerts@example.com" \\
      -d "to_addr=oncall@example.com" \\
      -d "use_tls=1" \\
      -w "\\nHTTP %{{http_code}}\\n"
""")


# ---------------------------------------------------------------------------
# Entrypoint
# ---------------------------------------------------------------------------

if __name__ == "__main__":
    auth = _auth_header()
    print_setup_commands()
    print(SLACK_CURL_EXAMPLE.format(sobs_url=SOBS_URL, auth=auth))
    print(EMAIL_CURL_EXAMPLE.format(sobs_url=SOBS_URL, auth=auth))

    if not _HAS_DEPS:
        print("Install Flask to start the webhook receiver: pip install flask")
    else:
        print(f"\nStarting webhook receiver on http://0.0.0.0:{WEBHOOK_PORT}/webhook …")
        print("Press Ctrl+C to stop.\n")
        receiver_app.run(host="0.0.0.0", port=WEBHOOK_PORT)
