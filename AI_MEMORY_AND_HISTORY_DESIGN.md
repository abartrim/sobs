# AI Memory And History Design

## Goals
- Keep prompts small and cache-friendly while still using prior context.
- Add semantic memory retrieval (embedding-based) so only relevant memories are injected.
- Add model-authored turn summaries so chat history can be searched without a second per-turn summarization call.
- Consolidate overlapping/conflicting memories with an LLM pass when new memory candidates are saved.
- Reuse existing OTEL telemetry for chat history summaries.

## Prompt Structure (KV Cache Friendly)
Stable prefix first, volatile fields at the end.

1. Base system instructions (stable)
2. Page action manifest (stable for a page)
3. Relevant persistent memories (semantic top-k)
4. Relevant prior turn summaries (semantic top-k from OTEL)
5. Current page context and user question (last and most volatile)

This ordering increases prefix reuse in models with KV cache.

## Turn Summary Format
Assistant is instructed to append metadata at end:

```text
<assistant_meta>{
  "turn_summary": {
    "request": "...",
    "action": "...",
    "result": "..."
  },
  "memory_candidates": ["...", "..."]
}</assistant_meta>
```

Server behavior:
- Parse and remove this block from the user-visible final answer payload.
- Persist summary fields to OTEL log attributes.
- Emit a dedicated `turn.summary` event in `otel_logs`.

## History Search Strategy
No dedicated chat-turn table is required.

Source of truth:
- `otel_logs` rows for `ServiceName=sobs-ai-helper`
- `EventName='turn.summary'`
- Attributes:
  - `gen_ai.turn.summary.request`
  - `gen_ai.turn.summary.action`
  - `gen_ai.turn.summary.result`
  - `gen_ai.chat_id`, `gen_ai.turn_id`

Retrieval:
1. Pull recent candidate summaries for chat_id.
2. Rank with cosine similarity over embeddings.
3. Inject top-k concise summaries into prompt.

## Memory Storage
Dedicated memory table is used because memories are state/config, not telemetry events.

Table: `sobs_ai_memories`
- `Id`
- `ChatId`
- `MemoryText`
- `EmbeddingJson`
- `SourceTurnId`
- `IsDeleted`
- `Version`
- `UpdatedAt`

Engine: `ReplacingMergeTree(Version)`

## Embedding And Similarity
Current implementation uses deterministic local embeddings:
- Tokenize text
- Hash tokens into a fixed-size vector
- L2-normalize
- Cosine similarity for ranking

This avoids extra network calls and supports semantic-ish ranking immediately.

## Memory Consolidation Flow
When metadata includes `memory_candidates`:

1. Compute embedding for each candidate.
2. Find related existing memories above consolidation threshold.
3. If related found, call LLM once to reconcile:
   - merge overlapping memories
   - resolve conflicts (newer fact wins unless model indicates otherwise)
   - optionally ignore noisy candidate
4. Upsert merged memory and tombstone dropped memory ids.

Consolidation response contract:
```json
{
  "action": "merge|keep_new|ignore",
  "memory": "consolidated memory text",
  "drop_ids": ["old-id-1", "old-id-2"]
}
```

## Safety And Cost Controls
- Max memory candidates per turn: 3
- Max injected memories: 5
- Max injected prior summaries: 4
- String length clamps on summaries and memories
- Local embedding path avoids per-turn embedding model cost
- Consolidation LLM call only when related memories exist

## Observability
All major operations are logged via existing helper telemetry:
- `turn.start`, `turn.complete`, `turn.error`
- `turn.summary`
- Memory save ids in `gen_ai.memory.saved_ids`

## Future Extensions
- Swap local embeddings for model embeddings endpoint when configured.
- Add explicit memory/history tools in a multi-step tool loop if chat runtime adds tool-result round trips.
- Add memory confidence/importance score and decay.
