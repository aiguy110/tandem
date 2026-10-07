# Remote MCP server sign-in

Remote (HTTP) MCP servers such as `https://ai.developerhub.io/mcp` often require
an OAuth sign-in. An agent harness running under Tandem cannot do that sign-in
itself: it has no interactive terminal, and its advice ("run `/mcp`") does not
apply. So the **daemon** holds the authorization for every harness.
Implementation: `internal/mcpauth`.

## Flow

1. **Detect.** For every HTTP server in `~/.tandem/config.yml` (re-read every
   15 s, so `tandem mcp add` is picked up without a restart), and for every
   project-local server when an agent starts, the daemon sends one
   unauthenticated MCP `initialize`. A `401` (or a `403` Bearer challenge) marks
   the server **needs sign-in**. Results are cached for 30 minutes (1 minute for
   errors).
2. **Notify.** A needs-sign-in server raises a system notification
   (`mcp-auth:<key>`) in the Notifications rail with **Authorize** and
   **Dismiss**. The same states, plus Re-authorize / Sign out / Check again,
   are in the command palette under **Manage MCP servers…**.
3. **Authorize.** Clicking Authorize opens a tab immediately (inside the
   click, so popup blockers allow it), then sends `begin_mcp_auth` with the
   UI's origin. The daemon:
   - reads protected resource metadata (RFC 9728) from the `WWW-Authenticate`
     `resource_metadata` parameter, or the `.well-known` fallbacks, and the
     authorization server's RFC 8414 / OIDC metadata. A server without
     resource metadata is treated as its own authorization server with the
     default `/authorize`, `/token`, `/register` endpoints;
   - registers a public client by dynamic client registration (RFC 7591),
     reused for later sign-ins with the same redirect URI, unless
     `oauth.clientId` is configured (below);
   - returns an authorization-code + PKCE (S256) URL with a `resource`
     indicator (RFC 8707) and a single-use `state`.
   The provider redirects the browser to `<UI origin>/mcp/oauth/callback`,
   where the daemon validates `state` (15-minute lifetime), redeems the code,
   and stores the tokens.
4. **Use.** A session started afterwards is not given the server's URL.
   Instead it gets `http://<daemon>/mcp-proxy/<key>` with a per-session
   capability (`Authorization: Bearer <HMAC of the session ID>` +
   `X-Tandem-Session`). The proxy forwards Streamable HTTP requests (POST, GET
   SSE streams, DELETE) with the real access token, streaming responses as
   they arrive. It refreshes the token shortly before expiry, and on an
   upstream `401` it refreshes and retries once. A refresh the authorization
   server rejects (`invalid_grant`) drops the token and raises a "sign-in
   expired" notification.

The access token therefore never enters an agent process, its MCP config file
(`TANDEM_MCP_SERVERS_FILE`), or a transcript, and long sessions outlive their
token.

## Sessions started before sign-in

An agent session starting while a server needs sign-in does **not** get that
server: handing it over would only produce the harness's own dead-end auth
error. The transcript shows a notice with an Authorize button instead. Once
authorized, new sessions get the server. A running session gets it after its
harness restarts, because the MCP server list is fixed at ACP `session/new` /
`session/load`.

## Configuration

Nothing is needed for servers that support dynamic client registration. To pin
a pre-registered client (for authorization servers without registration), or
to override the requested scopes:

```yaml
mcpServers:
  example:
    type: http
    url: https://mcp.example.com/mcp
    oauth:
      clientId: abc123
      clientSecret: optional-for-confidential-clients
      scopes: [read, write]
```

The registered redirect URI must be `<Tandem UI origin>/mcp/oauth/callback`,
for example `http://127.0.0.1:7717/mcp/oauth/callback`.

A server configured with its own `Authorization` header is passed through
untouched; Tandem never adds OAuth to it.

## Storage

Client registrations and tokens live in `$TANDEM_HOME/mcp-auth.json`, mode
`0600`, written atomically. They are kept out of `tandem.db` so copying or
inspecting the database never exposes credentials. **Sign out** deletes the
tokens but keeps the client registration.

## Limits

- Federation: a server configured on an agent host is authorized from that
  host's own UI. A parent relays the host's sign-in notification, but its
  Authorize button is disabled there, because the OAuth callback must reach
  the host's daemon.
- The callback goes to the origin the UI was loaded from. With the Vite dev
  server (`npm run dev`), sign in from the daemon-served UI instead.
