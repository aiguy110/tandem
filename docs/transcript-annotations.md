# Transcript annotations (highlight + comment → prompt)

Status: design / in progress. Desktop-only first pass; mobile UX deferred.

## Goal

Let a user select ("highlight") any span of the ACP transcript, attach a comment
to it, accumulate several such annotations, and then send them back to the agent
as **one** prompt in which each annotation is rendered as a quoted reference to
the original text followed by the user's note. Annotations are **persisted in the
daemon** so they can be added, viewed, and updated across devices before they are
sent.

This is a Google-Docs / Medium "margin comment" affordance whose "resolve" action
is "send everything to the model."

## Model

An **annotation** is a durable, mutable draft object anchored to one transcript
row:

```
Annotation {
  id        string   // uuid, daemon-assigned
  agentId   string
  seq       int64    // anchors to the representative event seq of a transcript row
  role      string   // "assistant" | "user" | "thought" | "tool"
  quote     string   // the selected text, trimmed + length-capped (~2 KiB)
  comment   string   // the user's note (may be empty while first drafting)
  createdAt int64    // unix millis
  updatedAt int64    // unix millis
}
```

Annotations are the durable "review tray": the set of pending annotations for an
agent syncs to every subscribed client. Sending consumes them (see below).

### Anchoring: quote-based, by representative seq

The transcript is built client-side in `TranscriptPane.build()` from the event
stream. Consecutive `message_chunk` / `thought_chunk` events are merged into one
rendered row whose React `key` embeds the **first** chunk's `seq` (e.g. `m1234`,
`t1234`, `u1234`, `tc<id>` for tools). We anchor an annotation to that
**representative seq** — the seq encoded in the row's `key` — plus the literal
selected `quote`.

We deliberately do **not** store character offsets. The reference we send the
model is the quoted text itself, so exact offsets are unnecessary and would be
meaningless anyway because assistant prose is rendered via
`dangerouslySetInnerHTML`. `seq` gives us (a) grouping by row and (b) a
scroll-to-target; `quote` gives the model the actual content.

An annotation whose `seq` no longer resolves to a rendered row (compaction, etc.)
is shown in the tray as "context unavailable" but keeps its comment. Acceptable
for v1.

Annotations are constrained to a **single row**. If a selection straddles rows,
the client clamps to the row containing the selection anchor.

## Wire contract

This section is the authoritative contract shared by the daemon and UI slices.

### Durable annotation type (both sides)

TS (`ui/src/wire.ts`):

```ts
export interface Annotation {
  id: string;
  agentId: string;
  seq: number;
  role: 'assistant' | 'user' | 'thought' | 'tool';
  quote: string;
  comment: string;
  createdAt: number;
  updatedAt: number;
}
```

Go (`internal/store`): a matching `Annotation` struct with the same JSON field
names (camelCase).

### Client → daemon messages

| `t` | fields | meaning |
|---|---|---|
| `add_annotation` | `agentId`, `seq`, `role`, `quote`, `comment`, `corrId` | create; daemon assigns `id`/timestamps |
| `update_annotation` | `agentId`, `id`, `comment`, `corrId` | edit comment text |
| `delete_annotation` | `agentId`, `id`, `corrId` | remove one |
| `clear_annotations` | `agentId`, `corrId` | remove all for the agent |

Each is ack'd with `{t:"ack", agentId, corrId, ...}` (or `commandError`). After a
successful mutation the daemon broadcasts the full list (below).

### Daemon → client messages

- On `subscribe`, include the current list in the existing `snapshot` message:
  add an `annotations: Annotation[]` field alongside `queuedPrompts`.
- On any mutation, broadcast `{t:"annotations", agentId, annotations: Annotation[]}`
  to **every connection subscribed to that agent** (cross-device sync). Model the
  helper on `broadcastClosed` (iterate `h.connections`, send only where
  `c.subs[agentID] != nil`).

### The `quote` prompt block

Extend `PromptBlock` with a third variant so a sent message persists structured,
replayable references (not a flattened blob).

TS (`ui/src/wire.ts`):

```ts
export type PromptBlock =
  | { type: 'text'; text: string }
  | ({ type: 'image' } & ImageAssetRef)
  | { type: 'quote'; refSeq: number; role: string; quote: string; comment: string };
```

Go (`internal/agentadapter/adapter.go` `PromptBlock`): add
`RefSeq int64 json:"refSeq,omitempty"`, `Quote string json:"quote,omitempty"`,
`Comment string json:"comment,omitempty"` (reuse existing `Type`).

**Flattening.** ACP agents only understand text/image content. The daemon must
convert every `quote` block to a `text` block *before handing the prompt to the
adapter* — but persist the original `quote` blocks in the `user_message` event so
replay stays structured. Put the flattening at the boundary where blocks are
converted to adapter input (in `Session.executePrompt` / the adapter's block
conversion), not in the wsserver handler, so the persisted `user_message` keeps
the rich blocks. Flattened rendering of one quote:

```
> [assistant] "…quote text…"
  <comment text>
```

Multiple quote blocks are each rendered this way, in order, ahead of any trailing
free-text block.

## Sending flow

1. User has N annotations in the tray (persisted) and optionally free text in the
   prompt box.
2. On send, the client builds `blocks`: one `{type:'quote', …}` per annotation
   (in transcript order), then a trailing `{type:'text'}` if the box is non-empty.
   Reuses the existing `prompt` message path (`store.prompt`).
3. After the prompt is accepted, the client sends `clear_annotations` for the
   agent (annotations have been consumed into a message). The daemon broadcasts
   the now-empty list.
4. The persisted `user_message` carries the `quote` blocks; the transcript
   renders each as a **citation chip** (quote + comment) that scrolls to `refSeq`
   and flashes the row on click.

## UI surfaces (all in `TranscriptPane.tsx` + `store.ts`)

- **Row identity.** In `build()`, thread each item's representative `seq` onto the
  `Item`, and in `Row()` stamp `data-seq`, `data-role`, `data-key` on the wrapper
  `div` so a DOM selection can be resolved to an anchor. Skip `permission` and
  `plan` rows (not annotatable).
- **Selection capture.** On `mouseup` within `.transcript`, read
  `window.getSelection()`; if non-empty, walk from `anchorNode` to the nearest
  `[data-seq]`, build the anchor `{seq, role, quote}`, and show a floating
  **Comment** button at `range.getBoundingClientRect()` (same pattern as the
  existing `scroll-latest` button). Don't offer it on the actively-streaming last
  message row.
- **Comment popover.** Clicking the button opens a small popover with a textarea +
  "Add". On Add → `store.addAnnotation(agentId, anchor, comment)` → `add_annotation`
  message; clear the DOM selection.
- **Review tray.** Render `annotations[agentId]` in the slot above the prompt bar
  (reuse the `prompt-queue` visual pattern). Each chip shows quote snippet +
  comment, an edit affordance (→ `update_annotation`), and a remove button (→
  `delete_annotation`). Clicking the snippet scrolls to `seq` and flashes the row.
- **Citation rendering.** Extend the `user` row renderer to draw `quote` blocks as
  citation chips (currently it maps only `text`/`image` blocks).
- **Store slice.** `annotations: Record<string, Annotation[]>` keyed by agent,
  hydrated from the `snapshot` and replaced wholesale by `annotations` broadcasts.
  Actions: `addAnnotation`, `updateAnnotation`, `removeAnnotation`,
  `clearAnnotations` (each sends the matching WS message; state is authoritative
  from the broadcast, so treat local changes optimistically or wait for the echo —
  prefer waiting for the echo for cross-device correctness).

## Daemon surfaces

- **Store** (`internal/store/store.go`): new `annotations` table + index on
  `agentId`; added via the existing `migration` list (not the base `schema`
  const, so existing DBs migrate). Methods `UpsertAnnotation`,
  `ListAnnotations(agentId) []Annotation`, `DeleteAnnotation(id)`,
  `DeleteAnnotationsForAgent(agentId) int`. Table:

  ```sql
  CREATE TABLE IF NOT EXISTS annotations (
    id        TEXT PRIMARY KEY,
    agentId   TEXT NOT NULL,
    seq       INTEGER NOT NULL,
    role      TEXT NOT NULL,
    quote     TEXT NOT NULL,
    comment   TEXT NOT NULL DEFAULT '',
    createdAt INTEGER NOT NULL,
    updatedAt INTEGER NOT NULL
  );
  CREATE INDEX IF NOT EXISTS annotations_agent ON annotations(agentId);
  ```

  Deleting an agent should also delete its annotations (extend `DeleteAgent`).
- **wsserver** (`internal/wsserver/wsserver.go`): handle the four inbound
  messages; add `annotations` to the `snapshot` payload; add a
  `broadcastAnnotations(agentID)` helper. Reuse `requireSession` / `commandAck` /
  `commandError`. Inbound struct gains `Seq`, `Role`, `Quote`, `Comment` fields
  (some already exist — check before adding).
- **PromptBlock flatten**: implement quote→text flattening at the adapter
  boundary; keep rich blocks in the persisted `user_message`.

## Tests

- Go: store CRUD + `DeleteAgent` cascade; wsserver add/update/delete/clear +
  snapshot inclusion + broadcast to a second subscriber; quote-block flatten
  preserves persisted blocks but hands text to the adapter.
- UI: store reducer for `annotations` snapshot/broadcast; `build()` seq threading;
  compose blocks on send.

## Out of scope (v1)

- Mobile / touch selection UX.
- Resolved/archived annotation states (send simply clears).
- Multi-row annotations; character-offset anchoring; annotation replies/threads.
