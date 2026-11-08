# Wiki-Go MCP Server

Wiki-Go exposes a [Model Context Protocol](https://modelcontextprotocol.io) endpoint that lets agents search, read and edit wiki documents with the same permissions as the HTTP API. The implementation lives in `internal/mcp`.

## Server overview

| Property | Value |
|---|---|
| Transport | Streamable HTTP, stateless. Each `POST` carries one JSON-RPC 2.0 message and gets one JSON reply. Other HTTP methods get `405`. |
| Protocol versions | `2025-06-18` (default), `2025-03-26` |
| Capabilities | `tools` |
| `serverInfo` | `name: "wiki-go"`, `title: <wiki title>`, `version: <build version>` |
| Methods | `initialize`, `ping`, `tools/list`, `tools/call` |
| Authentication | Checked on every request: session cookie, bearer session token or personal access token. Without one the reply is `401` with `WWW-Authenticate: Bearer realm="wiki-go"`. |
| Origin check | Browser requests from a different origin get `403`. Requests with no `Origin` header are accepted. |
| Request limit | 1 MiB per message (`413` above that) |
| Tool visibility | `tools/list` only returns the tools the caller's role may call. Document access rules (role and groups) also apply to every path argument. |
| Errors | Problems the agent can fix (bad arguments, not found, no access, revision mismatch) come back as a normal tool result with `isError: true`. Unexpected failures come back as JSON-RPC `-32603`. |

### Server instructions

Sent to the client in the `initialize` result:

> Wiki-Go documents are Markdown pages addressed by path, for example "guides/intro"; "/" is the home page. Find documents with search_documents or browse them with list_documents, then read one with read_document. To change a document pass the revision from read_document to edit_document (exact replacements) or write_document (whole content); a revision mismatch means someone else changed it, so read it again. write_document without revision creates a new document. Use add_comment to explain changes. Results only include the documents your role and groups may access. move_document and delete_document list the documents that link to the old path; fix them with edit_document. Ask the user before passing allow_access_change or before deleting: delete_document cannot be undone. restore_version undoes a change using the history from read_document.

### Concepts

- **Path**: documents are addressed by path, such as `guides/intro`. The home page is `/`.
- **Revision**: a hash of a document's current content, returned by `read_document`. Every tool that changes an existing document needs it, so a change made by someone else in the meantime is detected (optimistic concurrency).
- **Version**: a `yyyymmddhhmmss` timestamp of a previous content stored in the document's history. Every save keeps the previous content as a version.

## Making requests

The endpoint is `POST /mcp` on the wiki host, for example `http://127.0.0.1:8080/mcp`. Every request must carry a credential, because the server is stateless and authenticates each message separately.

### 1. Get a personal access token

MCP clients should use a personal access token. Tokens start with `wgo_pat_` and are created with the token API while logged in with a normal session (a personal access token cannot create other tokens):

```bash
# Log in and keep the session cookie
curl -s -c cookies.txt -X POST http://127.0.0.1:8080/api/login \
  -H 'Content-Type: application/json' \
  -d '{"username": "admin", "password": "secret", "keepLoggedIn": false}'

# Create a token for the MCP client
curl -s -b cookies.txt -X POST http://127.0.0.1:8080/api/tokens \
  -H 'Content-Type: application/json' \
  -d '{"name": "mcp-agent", "role": "editor", "expires_in_days": 30}'
```

The response contains the plain token in the `token` field. It is shown only once, so store it safely:

```json
{ "success": true, "token": "wgo_pat_…", … }
```

| Field | Description |
|---|---|
| `name` | 1–64 letters, digits, `.`, `_` or `-` |
| `role` | Optional cap on the token's role (`viewer`, `editor`, `admin`); it cannot exceed the owner's role. Empty means the owner's role. |
| `expires_in_days` | 0–3650; `0` means the token does not expire |

A token always acts with the owner's **current** role and groups, capped by its own `role`, so permission changes made by an admin apply on the next request. Use `viewer` for read-only agents; every write tool except `add_comment` needs `editor`.

### 2. Send the token in the `Authorization` header

Each MCP request is a JSON-RPC 2.0 message with these headers:

| Header | Value | Required |
|---|---|---|
| `Authorization` | `Bearer wgo_pat_…` | yes |
| `Content-Type` | `application/json` | yes |
| `MCP-Protocol-Version` | `2025-06-18` or `2025-03-26` | no; an unsupported value gets `400` |

The server also accepts a login session token as the bearer credential, or the `session_token` cookie of a browser session. Browser requests must come from the wiki's own origin.

```bash
TOKEN='wgo_pat_…'

# Initialize
curl -s -X POST http://127.0.0.1:8080/mcp \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-06-18"}}'

# List the tools available to this token
curl -s -X POST http://127.0.0.1:8080/mcp \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'MCP-Protocol-Version: 2025-06-18' \
  -d '{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}'

# Call a tool
curl -s -X POST http://127.0.0.1:8080/mcp \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'MCP-Protocol-Version: 2025-06-18' \
  -d '{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "read_document", "arguments": {"path": "guides/intro", "include": ["history"]}}}'
```

A tool call returns its text blocks in `result.content`:

```json
{
  "jsonrpc": "2.0",
  "id": 3,
  "result": {
    "content": [
      { "type": "text", "text": "path: guides/intro\nrevision: …\n\nhistory (newest first):\n- 20261106093000\n" },
      { "type": "text", "text": "# Intro\n\n…" }
    ]
  }
}
```

Notifications (messages without `id`, such as `notifications/initialized`) get `202 Accepted` with no body.

### 3. Configure an MCP client

Clients that support the Streamable HTTP transport only need the URL and the header. For example, in Claude Code:

```bash
claude mcp add --transport http wiki-go http://127.0.0.1:8080/mcp \
  --header "Authorization: Bearer wgo_pat_…"
```

Or in a JSON client configuration such as `.mcp.json`:

```json
{
  "mcpServers": {
    "wiki-go": {
      "type": "http",
      "url": "http://127.0.0.1:8080/mcp",
      "headers": { "Authorization": "Bearer ${WIKI_GO_TOKEN}" }
    }
  }
}
```

### Authentication and transport errors

| Status | Body `message` | Cause |
|---|---|---|
| `401` | `Unauthorized` | Missing, unknown or expired credential. The response includes `WWW-Authenticate: Bearer realm="wiki-go"`. |
| `403` | `Origin not allowed` | Browser request from another origin |
| `405` | `Method not allowed` | Anything other than `POST` |
| `400` | `Unsupported MCP protocol version` | Unknown `MCP-Protocol-Version` header |
| `413` | `Request too large (limit 1 MiB)` | Message larger than 1 MiB |

A valid token whose role is too low for a tool does not get an HTTP error: the tool is left out of `tools/list`, and calling it returns a tool result with `isError: true` and `Insufficient role: editor access required`.

## Tools summary

`tools/list` returns the tools in this order. `delete_document` comes last on purpose because it is the only change that cannot be undone.

| Tool | Minimum role | Read-only | Destructive | Idempotent |
|---|---|---|---|---|
| [`search_documents`](#search_documents) | viewer | yes | — | — |
| [`list_documents`](#list_documents) | viewer | yes | — | — |
| [`read_document`](#read_document) | viewer (editor for history and old versions) | yes | — | — |
| [`edit_document`](#edit_document) | editor | no | yes | no |
| [`write_document`](#write_document) | editor | no | yes | yes |
| [`add_comment`](#add_comment) | viewer | no | no | no |
| [`move_document`](#move_document) | editor | no | no | no |
| [`restore_version`](#restore_version) | editor | no | no | yes |
| [`delete_document`](#delete_document) | editor | no | yes | yes |

All tools declare `openWorldHint: false`. Every write is logged with the user name and token name.

---

## search_documents

**Title:** Search documents · **Role:** viewer

Full-text search over the wiki documents. Words are ANDed and matched case-insensitively, `"exact phrase"` matches a phrase and `not word` excludes a word. Returns the path, title and an excerpt of each match; pass the path to `read_document`.

### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `query` | string | yes | Search query, e.g. `deploy "port number" not draft` |
| `limit` | integer (1–50) | no | Maximum number of results (default 10, max 50) |

### Result

One text block: the total match count, then a `- path — title` line for each match shown, each followed by an excerpt.

### Rules and errors

- An empty `query` is rejected; use `list_documents` to browse.
- A `limit` below 1 is rejected; a `limit` above 50 is lowered to 50.
- Results only include documents the caller may access.

### Definition

```json
{
  "name": "search_documents",
  "title": "Search documents",
  "description": "Full-text search over the wiki documents. Words are ANDed and matched case-insensitively, \"exact phrase\" matches a phrase and \"not word\" excludes a word. Returns the path, title and an excerpt of each match; pass the path to read_document.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "query": { "type": "string", "description": "Search query, e.g. \"deploy \\\"port number\\\" not draft\"" },
      "limit": { "type": "integer", "description": "Maximum number of results (default 10, max 50)", "minimum": 1, "maximum": 50 }
    },
    "required": ["query"]
  },
  "annotations": { "readOnlyHint": true, "openWorldHint": false }
}
```

---

## list_documents

**Title:** List documents · **Role:** viewer

Lists the wiki documents as `path — title` lines in tree order. Use `prefix` to list one section and `depth` to limit how many levels below it are shown; an entry marked `+N below` has N deeper documents, listed by passing its path as `prefix`.

### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `prefix` | string | no | Section to list, e.g. `guides`; empty lists the whole wiki including the home page |
| `depth` | integer (≥ 0) | no | Levels below the prefix to show (default 2, 0 for all) |

### Result

One text block: a heading, then one `path — title` line per entry in tree order, with `(+N below)` on entries that have deeper documents beyond `depth`.

### Rules and errors

- Shows at most 500 entries; the heading says when the list was cut.
- Hidden folders (starting with `.`) are skipped.
- An invalid `prefix` or a negative `depth` is rejected.

### Definition

```json
{
  "name": "list_documents",
  "title": "List documents",
  "description": "Lists the wiki documents as \"path — title\" lines in tree order. Use prefix to list one section and depth to limit how many levels below it are shown; an entry marked \"+N below\" has N deeper documents, listed by passing its path as prefix.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "prefix": { "type": "string", "description": "Section to list, e.g. \"guides\"; empty lists the whole wiki including the home page" },
      "depth": { "type": "integer", "description": "Levels below the prefix to show (default 2, 0 for all)", "minimum": 0 }
    }
  },
  "annotations": { "readOnlyHint": true, "openWorldHint": false }
}
```

---

## read_document

**Title:** Read document · **Role:** viewer (editor for `version` or `include: ["history"]`)

Reads a wiki document. The result has two text blocks: first the path, the revision and the sections requested with `include`; then the Markdown source. Use `/` for the home page. Pass `version`, a timestamp from the history, to read an old version instead.

### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `path` | string | yes | Document path, e.g. `finance/finance-report` |
| `version` | string (`^[0-9]{14}$`) | no | Version timestamp (`yyyymmddhhmmss`) from the history |
| `include` | array of `attachments` \| `history` \| `comments` | no | Extra sections to return with the document |

### Result

Two text blocks:

1. Metadata: `path:`, then `revision:` (or `version:` when reading an old version), then the requested sections:
   - `attachments`: file name, MIME type and size of each attachment.
   - `history`: the version timestamps, newest first.
   - `comments`: author, time and Markdown of each comment, oldest first.
2. The raw Markdown content.

### Rules and errors

- A missing document and a document the caller cannot access get the same message: `Document not found or access denied`.
- An unknown `version` returns `Version not found`.

### Definition

```json
{
  "name": "read_document",
  "title": "Read document",
  "description": "Reads a wiki document. The result has two text blocks: first the path, the revision (a hash of the current content) and the sections requested with include; then the Markdown source. Use \"/\" for the home page. Pass version, a timestamp from the history, to read an old version instead. The history and old versions require the editor role.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "path": { "type": "string", "description": "Document path, e.g. \"finance/finance-report\"" },
      "version": { "type": "string", "description": "Version timestamp (yyyymmddhhmmss) from the history", "pattern": "^[0-9]{14}$" },
      "include": {
        "type": "array",
        "description": "Extra sections to return with the document",
        "items": { "type": "string", "enum": ["attachments", "history", "comments"] },
        "uniqueItems": true
      }
    },
    "required": ["path"]
  },
  "annotations": { "readOnlyHint": true, "openWorldHint": false }
}
```

---

## edit_document

**Title:** Edit document · **Role:** editor

Replaces exact text in a wiki document. Pass the revision from `read_document`; each edit's `old_text` must match exactly once in the current content unless `replace_all` is set. Edits apply in order and all or none are saved. The previous content is kept in the history.

### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `path` | string | yes | Document path, e.g. `guides/intro`; `/` for the home page |
| `revision` | string | yes | Revision returned by `read_document` |
| `edits` | array (1–50 items) | yes | Replacements, applied in order |
| `edits[].old_text` | string | yes | Exact text to replace, copied from the Markdown block of `read_document` |
| `edits[].new_text` | string | yes | Replacement text; may be empty to delete `old_text` |
| `edits[].replace_all` | boolean | no | Replace every occurrence instead of requiring a single match (default `false`) |

### Result

One text block: `Saved <path> (N edits applied).`, then `revision:` with the new revision and `previous version:` with the history timestamp of the old content.

### Rules and errors

- `revision` must match the current content, otherwise `Revision mismatch`: read the document again.
- `old_text` must not be empty and must differ from `new_text`.
- Each `old_text` must match exactly once unless `replace_all` is set; overlapping matches count separately (`---` matches twice in `----`).
- Each edit applies to the result of the previous one. If any edit fails, nothing is saved.
- Edits that would leave the document empty are rejected.

### Definition

```json
{
  "name": "edit_document",
  "title": "Edit document",
  "description": "Replaces exact text in a wiki document. Pass the revision from read_document; each edit's old_text must match exactly once in the current content unless replace_all is set. Edits apply in order and all or none are saved. The previous content is kept in the history. Requests are limited to 1 MiB.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "path": { "type": "string", "description": "Document path, e.g. \"guides/intro\"; \"/\" for the home page" },
      "revision": { "type": "string", "description": "Revision returned by read_document" },
      "edits": {
        "type": "array",
        "minItems": 1,
        "maxItems": 50,
        "items": {
          "type": "object",
          "properties": {
            "old_text": { "type": "string", "description": "Exact text to replace, copied from the Markdown block of read_document" },
            "new_text": { "type": "string", "description": "Replacement text; may be empty to delete old_text" },
            "replace_all": { "type": "boolean", "description": "Replace every occurrence instead of requiring a single match", "default": false }
          },
          "required": ["old_text", "new_text"]
        }
      }
    },
    "required": ["path", "revision", "edits"]
  },
  "annotations": { "readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": false }
}
```

---

## write_document

**Title:** Write document · **Role:** editor

Creates a wiki document or replaces its whole content. Without `revision` it only creates: it fails if the document exists. To replace an existing document pass the revision from `read_document`; prefer `edit_document` for partial changes. Start the content with front matter (`---\nlayout: kanban\n---`) for a kanban or links page.

### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `path` | string | yes | Document path, e.g. `guides/deploy`; `/` for the home page |
| `content` | string | yes | Full Markdown content, including the `#` title |
| `revision` | string | no | Revision from `read_document`; required to replace an existing document |

### Result

One text block: `Created <path>.`, `Saved <path>.` or `Unchanged <path>: the content is identical.`, then `revision:` and, when an old content was replaced, `previous version:`.

### Rules and errors

- `content` must not be empty.
- New paths may only use letters, digits, `-`, `_` and `/`; otherwise the error suggests a valid path.
- Without `revision` on an existing document: `Document exists; pass the revision from read_document to replace it`.
- With `revision` on a document that no longer exists: `Document not found or access denied`.
- Writing content identical to the current one succeeds without changes, so a retry after a success is safe.

### Definition

```json
{
  "name": "write_document",
  "title": "Write document",
  "description": "Creates a wiki document or replaces its whole content. Without revision it only creates: it fails if the document exists. To replace an existing document pass the revision from read_document; prefer edit_document for partial changes. New paths use letters, digits, '-', '_' and '/'. Start the content with front matter (---\\nlayout: kanban\\n---) for a kanban or links page. Requests are limited to 1 MiB.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "path": { "type": "string", "description": "Document path, e.g. \"guides/deploy\"; \"/\" for the home page" },
      "content": { "type": "string", "description": "Full Markdown content, including the # title" },
      "revision": { "type": "string", "description": "Revision from read_document; required to replace an existing document" }
    },
    "required": ["path", "content"]
  },
  "annotations": { "readOnlyHint": false, "destructiveHint": true, "idempotentHint": true, "openWorldHint": false }
}
```

---

## add_comment

**Title:** Add comment · **Role:** viewer

Adds a Markdown comment to a wiki document as the token owner. Use it to explain a change or to ask the document owners a question.

### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `path` | string | yes | Document path, e.g. `guides/intro`; `/` for the home page |
| `content` | string | yes | Comment in Markdown |

### Result

One text block: `Comment added to <path>.`

### Rules and errors

- Fails if comments are disabled in the wiki or not allowed in the document.
- `content` must not be empty.
- A second comment by the same user on the same document within the same second is rejected; retry it.

### Definition

```json
{
  "name": "add_comment",
  "title": "Add comment",
  "description": "Adds a Markdown comment to a wiki document as the token owner. Use it to explain a change or to ask the document owners a question.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "path": { "type": "string", "description": "Document path, e.g. \"guides/intro\"; \"/\" for the home page" },
      "content": { "type": "string", "description": "Comment in Markdown" }
    },
    "required": ["path", "content"]
  },
  "annotations": { "readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": false }
}
```

---

## move_document

**Title:** Move document · **Role:** editor

Moves or renames a wiki document together with its sub-documents, attachments, history and comments. `new_path` is the full new path, e.g. `archive/old-guide`. Fails if the move would change the access rules of any moved document unless `allow_access_change` is `true`: ask the user before setting it. Lists the documents that link to the old path so they can be fixed with `edit_document`.

### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `path` | string | yes | Current document path, e.g. `guides/old-name` |
| `new_path` | string | yes | Full new path, e.g. `archive/new-name`; letters, digits, `-`, `_` and `/` |
| `allow_access_change` | boolean | no | Confirm a move that changes which access rule applies to the moved documents (default `false`) |

### Result

One text block: `Moved <path> → <new_path> (N documents).`, then the documents that link to the old path (up to 20, then `and N more`).

### Rules and errors

- The home page cannot be moved, and no document can be moved to the home page.
- `new_path` must be a clean path, must differ from `path` and must not be inside it.
- A document must not already exist at `new_path`.
- The caller must have access to `new_path`, to its parent and to every moved document.
- If the move changes which access rule applies to any moved document, it fails with examples of the changes unless `allow_access_change: true`. This applies to admins too: it guards against exposing documents by mistake.

### Definition

```json
{
  "name": "move_document",
  "title": "Move document",
  "description": "Moves or renames a wiki document together with its sub-documents, attachments, history and comments. new_path is the full new path, e.g. \"archive/old-guide\". Fails if the move would change the access rules of any moved document unless allow_access_change is true: ask the user before setting it. Lists the documents that link to the old path so you can fix them with edit_document.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "path": { "type": "string", "description": "Current document path, e.g. \"guides/old-name\"" },
      "new_path": { "type": "string", "description": "Full new path, e.g. \"archive/new-name\"; letters, digits, '-', '_' and '/'" },
      "allow_access_change": { "type": "boolean", "description": "Confirm a move that changes which access rule applies to the moved documents", "default": false }
    },
    "required": ["path", "new_path"]
  },
  "annotations": { "readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": false }
}
```

---

## restore_version

**Title:** Restore version · **Role:** editor

Restores a document to a version from its history. Pass the version timestamp and the current revision from `read_document` (`include: ["history"]` lists the versions). The current content is kept in the history, so a restore can be undone.

### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `path` | string | yes | Document path; `/` for the home page |
| `version` | string (`^[0-9]{14}$`) | yes | Version timestamp (`yyyymmddhhmmss`) from the history |
| `revision` | string | yes | Current revision returned by `read_document` |

### Result

One text block: `Restored <path> to version <version>.`, then `revision:` and `previous version:`. If the current content already equals that version: `Unchanged <path>: the content is identical.`

### Rules and errors

- `version` must be a 14-digit timestamp and `revision` is required.
- An unknown `version` returns `Version not found`.
- `revision` must match the current content, otherwise `Revision mismatch`.

### Definition

```json
{
  "name": "restore_version",
  "title": "Restore version",
  "description": "Restores a document to a version from its history. Pass the version timestamp and the current revision from read_document (include history lists the versions). The current content is kept in the history, so a restore can be undone.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "path": { "type": "string", "description": "Document path; \"/\" for the home page" },
      "version": { "type": "string", "description": "Version timestamp (yyyymmddhhmmss) from the history", "pattern": "^[0-9]{14}$" },
      "revision": { "type": "string", "description": "Current revision returned by read_document" }
    },
    "required": ["path", "version", "revision"]
  },
  "annotations": { "readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false }
}
```

---

## delete_document

**Title:** Delete document · **Role:** editor

Permanently deletes a wiki document with its attachments, history and comments; this cannot be undone. Pass the revision from `read_document`. A document with sub-documents is only deleted with `recursive: true`; check them first with `list_documents`. Lists the documents that link to it.

### Parameters

| Name | Type | Required | Description |
|---|---|---|---|
| `path` | string | yes | Document path, e.g. `guides/obsolete` |
| `revision` | string | no | Revision from `read_document`; required when the path has a document |
| `recursive` | boolean | no | Also delete the sub-documents (default `false`) |

### Result

One text block: `Deleted <path> (N sub-documents, M attachments). This cannot be undone.`, then the documents that link to it (up to 20) and any warnings.

### Rules and errors

- The home page cannot be deleted.
- `revision` is required when the path has a `document.md`; a folder without one has no revision.
- A document with sub-documents fails unless `recursive: true`; the error gives the sub-document count.
- Fails if the section contains a document the caller cannot access.
- Agents should ask the user before calling this tool.

### Definition

```json
{
  "name": "delete_document",
  "title": "Delete document",
  "description": "Permanently deletes a wiki document with its attachments, history and comments; this cannot be undone. Pass the revision from read_document. A document with sub-documents is only deleted with recursive: true; check them first with list_documents. Lists the documents that link to it.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "path": { "type": "string", "description": "Document path, e.g. \"guides/obsolete\"" },
      "revision": { "type": "string", "description": "Revision from read_document; required when the path has a document" },
      "recursive": { "type": "boolean", "description": "Also delete the sub-documents", "default": false }
    },
    "required": ["path"]
  },
  "annotations": { "readOnlyHint": false, "destructiveHint": true, "idempotentHint": true, "openWorldHint": false }
}
```
