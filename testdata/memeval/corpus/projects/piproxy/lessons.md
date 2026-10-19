---
type: journal
---

## 2026-06-01 10:00

**[mistake]** (source: agent)

Assumed session_submissions ended_at marks a final flush; heartbeats every 2 minutes keep advancing it, so "session ended" must be inferred from idle time.
---

## 2026-06-03 14:20

**[lesson]** (source: distiller, conversation: x1)

GetProviderByPath iterated a Go map, so routing between OpenAI and DeepSeek on the shared /openai path was nondeterministic. Routing must never depend on map iteration order.
---

## 2026-06-05 09:12

**[lesson]** (source: agent)

Duffel journals reject PUT /api/fs writes; compaction must archive and recreate the journal instead of overwriting it.
