# Parent and child deployments

One Tandem UI can control agents running on several hosts. Tandem instances ("hosts")
form a tree: each has at most one **parent** and any number of **children**. A parent
controls its children and everything below them; hosts above a given host are its
**ancestors**, and "upstream"/"downstream" mean toward or away from the root.

A **link** joins a child to its parent. Either end may open it:

- **The child dials its parent** -- the usual case. Start the UI-facing instance
  normally, then point each additional instance at it:

  ```sh
  tandem --parent https://tandem.example.net
  ```

  or, equivalently, in the child's `config.yml`:

  ```yaml
  settings:
    federation:
      parent:
        url: https://tandem.example.net
        proxy: socks5://127.0.0.1:1080   # optional; see below
  ```

  `--parent` wins over the setting. (`--master` and `TANDEM_MASTER_PROXY` are older
  spellings of `--parent` and `TANDEM_PARENT_PROXY` and still work.)

- **The parent dials the child** ("adoption") -- for a child the parent can reach but
  that cannot reach it, such as a Tandem inside a container. See
  [Adopting a child](#adopting-a-child).

Once a link is up it behaves the same whichever end dialed.

The URL is the parent's normal HTTP origin. `https://` uses an encrypted WebSocket;
`http://` is supported for trusted private networks, but registration credentials,
agent traffic, terminal contents, and browser frames are then unencrypted. Prefer TLS
or a private authenticated network such as Tailscale whenever traffic leaves one host.

## Reaching a parent through a proxy

When a child cannot dial its parent directly -- a NAT'd or firewalled network, or a
parent reachable only inside an SSH tunnel -- set `TANDEM_PARENT_PROXY` to a SOCKS5
URL on the child:

```sh
TANDEM_PARENT_PROXY=socks5://127.0.0.1:1080 tandem --parent https://tandem.example.net
```

Credentials are accepted as URL userinfo (`socks5://user:pass@host:1080`) and are
redacted by `tandem debug config`. Both halves of the transport honor the setting: the
registration/status calls and the durable WebSocket tunnel. Nothing else changes --
inbound serving, agent processes, and the browser subsystem dial as they always did.
Dials to adopted children use each child entry's own `proxy` instead.

Host names in the parent URL are resolved by the proxy rather than locally, so a name
that only resolves on the far side of the tunnel still works. SOCKS5 sits below TLS, so
an `https://` parent still terminates its own TLS end to end and the proxy sees only
ciphertext.

## Adopting a child

List the children a host should dial under `settings.federation.children` in its
`config.yml`:

```yaml
settings:
  federation:
    children:
      - url: http://127.0.0.1:17717    # the child's HTTP origin
        name: devbox                   # optional label; default is the child's hostname
        joinToken: 3f0c...             # optional; must match the child's join token
        proxy: socks5://127.0.0.1:1080 # optional
```

The child needs no configuration beyond being reachable. On the child, this daemon is
about to hand admin access to whoever dialed it, so the **child** decides whether to
accept:

- **Join token.** Start the child with `TANDEM_JOIN_TOKEN=<secret>` (or
  `settings.federation.joinToken`) and give the parent the same value. A parent that
  presents it is adopted straight away.
- **Approval.** Without a matching token, the child shows an **Adopt this host?**
  notification with Accept and Reject. The parent keeps retrying until someone answers.

Either way the parent generates a credential for that child URL, and once the child has
accepted it the credential alone is enough: the join token can then be dropped from the
parent's config. A child keeps the host ID it already had, so being adopted does not
rename it for its own children.

A host still has only one parent. A host started with `--parent` refuses adoption, and a
child already adopted by a connected parent refuses another parent's join token. (Its
operator can still accept another parent's request, which replaces the first one.)

The parent trusts the child because you configured that URL. When the link leaves the
machine, use `https://` or a private network, as for any link.

For a container, have the Tandem inside listen on all interfaces and publish its port
on the host's loopback only, so nothing else can reach its UI:

```sh
docker run -p 127.0.0.1:17717:7717 -e TANDEM_BIND=0.0.0.0 -e TANDEM_JOIN_TOKEN=3f0c... ...
```

Adoption needs Tandem v0.19 or later on the child; an older child answers the parent's
dial with 404 and the parent logs that.

## Registration and trust

A child that dials its parent makes an outbound connection, so the parent does not need to
reach an inbound port on the child. A first-time connection creates a notification in
the parent's UI with **Accept** and **Reject** actions. Acceptance establishes durable
trust: Tandem generates and stores a credential at both ends and uses it to reconnect
automatically after either daemon restarts. Rejection does not establish trust.

The child's hostname is its initial display name, and also seeds its host ID — a slug of
that name plus a short random suffix (`boremox-3f9a1c`), limited to letters, digits, `-`,
`_` and `.`. Host IDs are not secrets (the credential issued on acceptance is), so they
are kept short and readable: the parent namespaces a remote agent as
`fed~<hostId>~<agentId>`, and that string shows up in the UI.

Host identities, credentials, and approval state live under each daemon's `TANDEM_HOME`;
deleting or changing that home therefore creates a new identity that requires approval.

Federation forms a rooted tree. Every instance may have at most one upstream parent and
may accept many directly connected children, including when it is itself registered with
an upstream. A link the child dials needs no inbound reachability on the child, and an
adopted link needs none on the parent.

Trust is hop-by-hop: accepting a child delegates control of that child and the subtree it
advertises. An upstream parent can therefore discover and manage descendant hosts without
holding their link credentials. Descendant addresses carry an opaque route, while the host
catalog includes `parentId`, `route`, and `depth` metadata so clients can present the real
topology. Tandem rejects cyclic or excessively deep advertised routes; the supported
maximum depth is eight links.

## Changing a host's ID

A host that generates a new ID for itself — hosts registered before the short-ID format
do this once, on the first start after updating — replaces its old record rather than
adding a second one. Its registration names the ID it is replacing and presents that
record's credential as proof the two IDs are the same host. The parent then transfers
trust to the new ID and deletes the old row, so the change costs no approval and leaves
nothing behind. The proof is offered only to the parent that issued the credential, and
an unproven claim is ignored: the request falls back to ordinary approval.

A host that lost its stored identity altogether — a reinstall, a new `TANDEM_HOME` — has
no credential left to prove anything with, so it arrives as a first-time registration.
Because two machines may legitimately share a hostname, the parent will not guess: when
a pending registration's name matches an existing record that is not currently connected,
the approval notification names that record and offers **Accept and replace** alongside
**Accept**. Replacing deletes the superseded record; plain acceptance keeps both.

A host whose record was deleted or replaced finds its credential refused at the tunnel
and registers again under the same ID, so removing a record never strands the daemon
running on that machine — it re-appears as a pending registration.

## Protocol versions and skew

Both peers report `protocolVersion` (the federation wire version, `federation.ProtocolVersion`)
and `buildVersion` (the release) on every connection: in the registration request and
response, in the tunnel `hello`/`welcome` messages, and in each heartbeat. The parent
stores the host's pair on its durable child record and exposes it to the UI on each host
entry; a host predating version reporting reports `0` and an empty build.

**Bumping the version.** The tunnel carries opaque browser-protocol envelopes, which is
why federation has survived many command additions without a protocol change. Preserve
that: *add only*. A new command type or a new field costs nothing across versions and
must not bump `ProtocolVersion`. Bump it only for a genuinely breaking change — new or
reinterpreted tunnel/register/heartbeat framing, changed credential handling, or an
existing field whose meaning changes.

**Skew is reported, not refused.** A peer one version off still connects, because most
commands stay mutually intelligible. Each side raises a notification naming the other's
version and which end to update, so the failure mode is a readable message rather than a
command that silently does nothing. The notice is dismissible, clears when a matching
version connects, and is re-raised on the next connection if the skew persists.

## Spawning and control

The spawn palette includes every reachable host, including descendants, in its host
selector. Repository discovery, configured agents, launch variants, workspace
provisioning, and the resulting process all belong to the selected host. Remote agents
appear in the normal agent rail with a host label. Agent IDs are namespaced at the
viewing parent so equal local names on two hosts cannot collide.

The command palette's **Fleet View** displays the complete hierarchy. Each node shows its
name, connection state, Tandem build, and federation protocol version; directed arrows
point from children to their parents.

The parent proxies the same live controls available for a local agent, subject to the
controlled host's access policy (see below), including:

- prompts, queued prompts, interrupts, permissions, modes, and configuration;
- transcript events, raw terminal and workspace-shell input/output;
- lifecycle, rename, close preview, diff, and close operations;
- browser screencast frames, input, takeover notifications, and control ownership;
- spoken transcript playback: the clip is rendered (and cached) by the host that owns
  the transcript, using that host's configured voice provider, and travels back over
  the tunnel so the parent can serve it from its ordinary audio route. A parent with no
  voice provider of its own can still play a remote agent's messages.

The child continues to serve its own local UI. Local users and the parent are peer
controllers of the child's daemon-owned state, so updates and control changes are
visible to both. Browser control remains serialized by the existing per-agent control
token.

The child reconnects automatically after a network interruption. Its active agents keep
running while disconnected and are presented to the parent again after the connection
and subscriptions recover.

## Controlling hosts above and beside you

Links are still opened by the child, but commands can travel both ways over them. A
child can see and drive its parent, the parent's other children, and anything above the
parent, when those hosts allow it. Nothing extra has to be reachable: a sibling is reached
through the parent both share.

Every host decides who may control it, in its own `config.yml`:

```yaml
settings:
  federation:
    access:
      - from: laptop-*      # host ID glob, "*", or "ancestors"
        level: operate
      - from: ancestors     # lower the default for this host's parents
        level: view
```

The first rule that matches wins. After the configured rules come two defaults:
`ancestors: admin` and `*: none`. So by default nothing changes: parents control their
subtrees as before, and a host is invisible to everything else. Levels include everything
below them:

| Level | Allows |
|---|---|
| `none` | nothing; the host is left out of the requester's host list |
| `view` | agent lists, transcript/terminal/screencast subscriptions, history search, diffs, listings |
| `message` | everything in `view`, plus agent messaging: `agent_directory`, `agent_message_deliver`, `agent_link_request` (see [agent-messaging.md](agent-messaging.md)) |
| `operate` | everything in `message`, plus prompts, interrupts, approvals, terminal, workspace-shell and browser input, renames, annotations |
| `admin` | spawn, resume, close, settings, installs, notification actions, and any command not classified above |

`message` lets agents on the requesting host discover this host's listed agents and send
them messages, but only over a link a human granted on this host; it grants no control
over agents. It is opt-in for siblings, e.g. `- from: "*"` / `level: message`. A relayed
envelope's `from.host` must equal the command's origin, so a host cannot send as another
host.

`from` matches the requesting host's ID as shown in Fleet View (`boremox-3f9a1c`). A
Tandem without a parent generates a durable ID of the same form for itself. `ancestors`
matches this host's parent, that parent's parent, and so on. Access rules are read at
startup, so restart the daemon after changing them.

The host that executes a command enforces its own policy. Each relay stamps who the
command came from, and a parent accepts a child's claim only for that child or a host in
the subtree the child advertises. A relay can still impersonate hosts it relays for, so a
rule can't safely give a host routed through some relay more than that relay itself would
get. When a parent sends a child its view of the fleet, it leaves out every host whose
policy does not give that child at least `view`. Hosts reached through a parent are never
advertised further up, and they never forward their system notifications. Their
notifications, and the actions on them, stay with that host's own operators.

In the host list, the spawn palette, and Fleet View, a host shows the access you have to
it. Spawning is offered only on hosts where you have `admin`. A host's own policy can also
hide its agents from its parent: if its parent doesn't have `view`, the host sends an
empty agent list upstream.

Upstream control needs federation protocol 3 on both ends of each link. A host still on an
older version keeps working in the original, downward-only way.

## Session history

The parent's resume palette includes the local host and every connected child. Session
listing and search run against each selected host's own persisted/imported history index;
results carry a host identity so equal agent or session IDs cannot collide. Resuming a
remote result happens on the host that owns its history and returns a namespaced live
agent to the parent's ordinary agent rail. A disconnected host remains visible but cannot
be searched or resumed until it reconnects.
