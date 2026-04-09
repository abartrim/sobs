"""
SOBS AI transparency and assistant example.

This script demonstrates two AI-related SOBS features:

1. **AI Transparency** (`POST /v1/ai`) – record every LLM call your application
   makes so SOBS can track model usage, cost, and latency on the AI page
   (http://localhost:44317/ai).

2. **AI Contextual Helper** (`POST /api/ai/helper`) – send a natural-language
   question to the SOBS assistant, which has access to your live telemetry data
   (logs, errors, traces, metrics) and returns an AI-generated answer.

Prerequisites
-------------
1. Start SOBS:
       docker run -p 44317:4317 ghcr.io/abartrim/sobs:latest

2. Configure an AI provider in Settings → AI:
   - Endpoint URL:  https://api.openai.com/v1
   - Model:         gpt-4o-mini  (or any OpenAI-compatible model)
   - API Key:       sk-...

   Or set env vars before starting SOBS:
       SOBS_AI_ENDPOINT_URL=https://api.openai.com/v1
       SOBS_AI_MODEL=gpt-4o-mini
       SOBS_AI_API_KEY=sk-...

3. Install dependencies:
       pip install requests

Run:
       python examples/python/ai_agent_example.py
"""

import os
import time
import uuid

try:
    import requests
except ImportError:
    raise SystemExit("Install requests: pip install requests")

SOBS_ENDPOINT = os.environ.get("SOBS_ENDPOINT", "http://localhost:44317")
SOBS_API_KEY = os.environ.get("SOBS_API_KEY", "")
SERVICE_NAME = "ai-demo-service"

_SESSION = requests.Session()
if SOBS_API_KEY:
    _SESSION.headers["X-API-Key"] = SOBS_API_KEY


# ---------------------------------------------------------------------------
# 1. AI Transparency – record an LLM call to SOBS
# ---------------------------------------------------------------------------


def record_ai_call(
    prompt: str,
    response: str,
    *,
    provider: str = "openai",
    model: str = "gpt-4o-mini",
    tokens_in: int = 0,
    tokens_out: int = 0,
    duration_ms: float = 0.0,
    trace_id: str | None = None,
    span_id: str | None = None,
    tags: dict | None = None,
) -> None:
    """Send an AI call event to SOBS for transparency and cost tracking."""
    payload: dict = {
        "service": SERVICE_NAME,
        "provider": provider,
        "model": model,
        "prompt": prompt,
        "response": response,
        "tokens_in": tokens_in,
        "tokens_out": tokens_out,
        "duration_ms": duration_ms,
    }
    if trace_id:
        payload["trace_id"] = trace_id
    if span_id:
        payload["span_id"] = span_id
    if tags:
        payload["tags"] = tags

    r = _SESSION.post(f"{SOBS_ENDPOINT}/v1/ai", json=payload, timeout=5)
    r.raise_for_status()
    print(f"[AI transparency] recorded call – model={model} tokens_in={tokens_in} tokens_out={tokens_out}")


def simulate_llm_call(prompt: str) -> tuple[str, int, int]:
    """Simulate an LLM call (replace with your real LLM client)."""
    time.sleep(0.05)
    response = f"(simulated response to: {prompt[:40]}…)"
    return response, len(prompt.split()), 12


# ---------------------------------------------------------------------------
# 2. AI Contextual Helper – ask the SOBS assistant a question
# ---------------------------------------------------------------------------


def ask_sobs_helper(question: str, chat_id: str | None = None) -> dict:
    """
    Send a question to the SOBS AI assistant.

    The assistant has access to live telemetry (logs, errors, traces, metrics)
    and returns a structured response with an AI-generated answer.

    Parameters
    ----------
    question:   Natural-language question about your observability data.
    chat_id:    Optional conversation ID to maintain context across turns.
                Pass the ``chat_id`` returned by a previous call to continue
                the same conversation.

    Returns
    -------
    dict with keys:
        answer      – AI-generated answer text
        chat_id     – conversation ID (pass back for follow-up questions)
        actions     – list of suggested UI actions (optional)
    """
    payload: dict = {"question": question}
    if chat_id:
        payload["chat_id"] = chat_id

    r = _SESSION.post(
        f"{SOBS_ENDPOINT}/api/ai/helper",
        json=payload,
        timeout=60,
    )
    r.raise_for_status()
    return r.json()


# ---------------------------------------------------------------------------
# 3. Example – GitHub agent / work item flow
# ---------------------------------------------------------------------------


def check_agent_settings() -> None:
    """
    SOBS Agent Flows (Settings → Agents) can automatically:
    - Scan GitHub issues and link them to errors/traces.
    - Create work items from SOBS errors and assign them to your team.
    - Run scheduled research tasks against your live telemetry.

    This function just prints the settings page URL and a reminder.
    Configure agent rules at http://localhost:44317/settings/agents.
    """
    print("\n[Agent Flows]")
    print(f"  Configure agent rules at: {SOBS_ENDPOINT}/settings/agents")
    print("  Agents can link GitHub issues to errors, create work items,")
    print("  and run scheduled analysis tasks.")
    print("  Required settings: AI endpoint + model (Settings → AI)")


# ---------------------------------------------------------------------------
# 4. Entrypoint
# ---------------------------------------------------------------------------


def main():
    print(f"SOBS endpoint: {SOBS_ENDPOINT}")
    print(f"Auth: {'API key set' if SOBS_API_KEY else 'no auth'}\n")

    # ---- AI Transparency ----
    prompt = "Summarise the latest deployment health for the checkout service."
    response, tokens_in, tokens_out = simulate_llm_call(prompt)
    trace_id = uuid.uuid4().hex[:32]
    start = time.monotonic()
    record_ai_call(
        prompt=prompt,
        response=response,
        provider="openai",
        model="gpt-4o-mini",
        tokens_in=tokens_in,
        tokens_out=tokens_out,
        duration_ms=(time.monotonic() - start) * 1000 + 50,
        trace_id=trace_id,
        tags={"env": "production", "feature": "deployment-health"},
    )

    # ---- Second call (simulate a longer reasoning call) ----
    prompt2 = "What anomalies were detected in the past hour for service=api-gateway?"
    response2, t2_in, t2_out = simulate_llm_call(prompt2)
    record_ai_call(
        prompt=prompt2,
        response=response2,
        provider="openai",
        model="gpt-4o",
        tokens_in=t2_in,
        tokens_out=t2_out,
        duration_ms=1230,
        tags={"env": "production", "feature": "anomaly-query"},
    )

    # ---- AI Contextual Helper ----
    print("\n[AI Helper] Asking SOBS assistant …")
    try:
        result = ask_sobs_helper("Are there any error spikes in the last 30 minutes? Which services are affected?")
        print(f"  Answer: {result.get('answer', '(no answer)')}")
        chat_id = result.get("chat_id")

        if chat_id:
            # Follow-up question in the same conversation
            result2 = ask_sobs_helper(
                "Which trace IDs are associated with those errors?",
                chat_id=chat_id,
            )
            print(f"  Follow-up: {result2.get('answer', '(no answer)')}")
    except Exception as exc:
        print(f"  [skipped – AI not configured or unavailable: {exc}]")
        print(f"  Configure AI at: {SOBS_ENDPOINT}/settings/ai")

    check_agent_settings()

    print(f"\nDone – view AI transparency at {SOBS_ENDPOINT}/ai")


if __name__ == "__main__":
    main()
