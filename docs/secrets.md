# Secret broker

Tandem agents can ask a user for an API credential through the daemon-provided
`tandem-control` MCP server without placing the credential in model context.
`secret_request` records and displays only the service, reason, exact HTTPS
origin, and credential-header name. The notification rail collects the value in
a password field and submits it directly to an authenticated, non-replayable
daemon endpoint. Values and grants are memory-only and disappear on restart.

An approved request returns an opaque, session-bound grant ID. For network use,
the agent uses that ID with `secret_http_request`; the daemon verifies the exact
origin, rejects local/private destinations and redirects, injects the credential
header, limits response bodies, and redacts reflected secret bytes before
returning the response. Secret values are never included in MCP results, transcript events,
SQLite, structured logs, or browser snapshots. Request and resolution metadata
are durable so the UI can reconstruct pending requests after reconnect, but a
daemon restart invalidates any previously entered value.

This feature guarantees that the model need not see the credential. Tandem's
current agents still run as the daemon's host user without an OS sandbox, so it
does not claim that a malicious same-user process cannot inspect daemon memory
or credentials. That stronger guarantee requires separate users, containers, or
VMs with restricted process/filesystem access and controlled egress.

## Agent tools

- `secret_request(service, reason, origin, headerName?, prefix?)` blocks until
  the user grants or denies access and returns only a grant ID.
- `secret_http_request(grantId, url, method?, headers?, body?)` performs a
  request against the approved origin with daemon-side credential injection.
- `secret_file_request(service, reason, path)` asks for a credential scoped to
  one workspace-relative file and returns an opaque grant ID.
- `secret_file_read(path)` returns a revision and file content with active,
  file-scoped secrets replaced by `{{TANDEM_SECRET:grantId}}` markers. A missing
  file has revision `missing`.
- `secret_file_write(path, revision, content)` verifies that the file has not
  changed, materializes approved markers inside the daemon, and atomically
  writes the complete file with mode `0600`. It returns no file content.

This redacted read/edit/write loop lets a cooperative agent create and later
edit files such as `.env` without copying a secret into its prompt or tool
results. File approvals are exact-path and session scoped; traversal and
symlink targets are rejected. Grants and the broker's ability to recognize
existing secret bytes remain memory-only, so after a daemon restart the user
must grant the secret again before Tandem can redact that existing value.

Only HTTPS origins without credentials, paths, queries, or fragments are
accepted. Credential headers, `Host`, and `Cookie` cannot be supplied or
overridden by agent-authored headers.
