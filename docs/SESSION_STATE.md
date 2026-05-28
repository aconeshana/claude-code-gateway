# Session State Reference

This document is the authoritative field reference for `session.Session` and
`session.SessionInfo`. Every gateway feature that reads or writes session
state goes through `session.Manager`'s API; the fields below describe what
those API surfaces actually carry.

**Source-of-truth rule** — see also CLAUDE.md "Conventions":

1. `~/.claude/projects/*.jsonl` is the canonical record of conversation
   history (managed by Claude CLI, not by us).
2. `gateway_state.json` is gateway-only augmentation: business fields like
   label, summary, owner that Claude CLI doesn't track.
3. In-memory `manager.sessions[]` is a cache built from those two sources.
   Modules MUST go through `manager` APIs; never read `gateway_state.json`
   or jsonl files directly.
4. When session-related logic changes (new field, new origin value, new
   API), update this document in the same commit.

---

## Identity & timestamps

| Field | Type | Source of truth | Meaning |
| ----- | ---- | --------------- | ------- |
| `ID` | `string` (UUID) | manager (created on import/create) | Gateway-internal handle. Stable across reactivate. |
| `CLISessionID` | `string` (UUID) | jsonl filename | Runtime-internal id. Used with `--resume` to reattach Claude CLI. Empty until the process emits its first `init` message. |
| `CreatedAt` | `time.Time` | manager (`time.Now()` on create) or imported | When the gateway first knew about this session. |
| `LastActivity` | `time.Time` | jsonl mtime (for imports) or last `SendMessage`/`ResolveResumable` call | Used in UI as "active 显示最后沟通时间 \| idle 显示最后修改时间". |
| `ArchivedAt` | `time.Time` | manager (`time.Now()` on `Archive`) | Zero unless `Status==StatusArchived`. |

## Lifecycle (user-facing)

| Field | Type | Source of truth | Meaning |
| ----- | ---- | --------------- | ------- |
| `Status` | `session.Status` | manager | One of `StatusActive` / `StatusIdle` / `StatusArchived`. See "Status state machine" below. |

### Status state machine

```
              Create / Resume                       Reactivate
   (start)  ─────────────────►   Active   ◄──────────────────────  Idle
                                   │                                  ▲
                                   │ TransitionToIdle (process exit)  │
                                   ├─────────────────────────────────►│
                                   │                                  │
                                   │ Archive                          │ Archive
                                   ▼                                  ▼
                                Archived ◄────────────────────────────┘
                                   │
                                   │ RemoveArchived (user delete)
                                   ▼
                                (gone)
```

Reactivate from Archived also produces a fresh Active session (with a new
gateway `ID`, same `CLISessionID`).

## Runtime phase (independent of Status)

| Field | Type | Source of truth | Meaning |
| ----- | ---- | --------------- | ------- |
| `State` (string of `State` enum) | `string` | manager (driven by runtime callbacks) | Process-level phase: `starting` → `ready` → `processing` / `waiting_permission` → `idle` → `stopped`/`error`. Only meaningful while `Status==Active`. |
| `PendingTurns` | `int` | manager | Number of user messages queued but not yet acknowledged. |

## User metadata

| Field | Type | Source of truth | Meaning |
| ----- | ---- | --------------- | ------- |
| `OwnerID` | `string` | manager (per-channel concept; e.g. Feishu open_id) | Empty for unowned sessions discovered on disk. |
| `Label` | `string` | manager (from `/new <label>` or auto-set on first message) | Short user-facing nickname. Often empty for `external` sessions. |
| `Summary` | `string` | manager → SummaryStore (`gateway_state.json`) | AI-generated 12-20 char Chinese description. Empty = either not generated yet, or worker classified the session as meta-like (`_skip_meta_`). |
| `CustomTitle` | `string` | jsonl (Claude CLI `/rename`) | User-assigned name via Claude CLI's `/rename`. Takes display precedence over `Summary`. |
| `ChatID` | `string` | inbound message metadata | Channel-specific chat identifier (Feishu open_chat_id, etc.). |
| `ChannelKind` | `string` | constants in `internal/channel` (`KindFeishu`, ...) | Which channel implementation owns this session. |

## Provenance

| Field | Type | Source of truth | Meaning |
| ----- | ---- | --------------- | ------- |
| `Origin` | `string` | constants in `internal/session` | How the session entered the manager. See table below. |

### Origin values

| Constant | Wire value | How a session ends up with this | Visible to users? |
| -------- | ---------- | ------------------------------- | ----------------- |
| `OriginFeishu` | `"feishu"` | Created via `/new` or auto-resolve in a Feishu chat | Yes |
| `OriginWS` | `"ws"` | Created via the WebSocket gateway transport | Yes (to the connected WS client) |
| `OriginExternal` | `"external"` | Discovered on disk; not created via this gateway | Only with `GATEWAY_SHARE_EXTERNAL_SESSIONS=true` |
| `OriginAdmin` | `"admin"` | Spawned by the gateway's own admin/summary worker. Detected by (a) cwd under `claudeRT.AdminWorkdirPrefix` or (b) fingerprint match against the worker prompt marker `[GATEWAY_ADMIN_SESSION_v1]` | **Never** — filtered out of every user-facing view, every worker enqueue, and the `maxSessions` cap |

## Runtime configuration

| Field | Type | Source of truth | Meaning |
| ----- | ---- | --------------- | ------- |
| `WorkingDir` | `string` | jsonl `cwd` field (for imports) or `CreateOpts.WorkingDir` | Absolute cwd the CLI process was spawned with. Used by `/diff`, `show_project` grouping, and admin-internal detection. Effectively immutable post-creation. |
| `PermissionMode` | `string` | constants in `internal/runtime/claude` | One of `PermissionDefault` / `PermissionAuto` / `PermissionForward`. |

## Augmentation (worker / discovery only)

These live in `session.ExternalAugmentation` (persisted under
`gateway_state.json.external_summaries[cli_id]`), keyed by `CLISessionID`.
Access via manager: `SetExternalSummary` / `ExternalSummary` /
`CountFreshExternalSummaries` / `PurgeExternalSummaries`.

| Field | Source of truth | Meaning |
| ----- | --------------- | ------- |
| `Summary` | summary worker | AI-generated text. Empty + `PromptVersion==current` means "worker classified as meta-like and chose not to summarize" — don't re-enqueue. |
| `CustomTitle` | jsonl (carried alongside summary so cleanup logic stays consistent) | Same as the top-level field; mirrored here for atomic write/read. |
| `PromptVersion` | summary worker | The `bridge.SummaryPromptVersion` constant at the time the summary was generated. Discovery uses this to invalidate stale outputs when the worker prompt changes. |

---

## Modifying this schema

When you add, remove, or change the semantics of a field above:

1. Update `session.Session` / `session.SessionInfo` in
   `internal/session/session.go` (or `ExternalAugmentation` in `manager.go`).
2. Update the constants table here if you added a new `Origin` /
   `Status` / `PermissionMode` value.
3. Update this document. **CLAUDE.md requires the doc change in the same
   commit as the schema change.**
4. If the persisted JSON shape changed:
   - bump the relevant version constant (`cacheSchemaVersion` for
     discovery cache, `SummaryPromptVersion` for augmentation summaries);
   - add a legacy-read branch in `persist.JSONStore.readFile`.
5. Add or update tests covering the new behaviour.
