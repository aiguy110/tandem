# Master and slave deployments

One Tandem UI can control agents running on several hosts. Start the ordinary,
UI-facing instance as the **master**, then point each additional instance at it:

```sh
tandem --master https://tandem.example.net
```

The URL is the master's normal HTTP origin. `https://` uses an encrypted WebSocket;
`http://` is supported for trusted private networks, but registration credentials,
agent traffic, terminal contents, and browser frames are then unencrypted. Prefer TLS
or a private authenticated network such as Tailscale whenever traffic leaves one host.

## Registration and trust

The slave makes an outbound connection to the master, so the master does not need to
reach an inbound port on the slave. A first-time connection creates a notification in
the master's UI with **Accept** and **Reject** actions. Acceptance establishes durable
trust: Tandem generates and stores a credential at both ends and uses it to reconnect
automatically after either daemon restarts. Rejection does not establish trust.

The slave's hostname is its initial display name. Host identities, credentials, and
approval state live under each daemon's `TANDEM_HOME`; deleting or changing that home
therefore creates a new identity that requires approval.

Federation is deliberately one level deep. An instance started with `--master` rejects
attempts by other slaves to register with it and returns an explanatory error. Masters
may accept many directly connected slaves.

## Spawning and control

The spawn palette includes a host selector. Repository discovery, configured agents,
launch variants, workspace provisioning, and the resulting process all belong to the
selected host. Remote agents appear in the normal agent rail with a host label. Agent
IDs are namespaced at the master so equal local names on two hosts cannot collide.

The master proxies the same live controls available for a local agent, including:

- prompts, queued prompts, interrupts, permissions, modes, and configuration;
- transcript events, raw terminal and workspace-shell input/output;
- lifecycle, rename, close preview, diff, and close operations;
- browser screencast frames, input, takeover notifications, and control ownership.

The slave continues to serve its own local UI. Local users and the master are peer
controllers of the slave's daemon-owned state, so updates and control changes are
visible to both. Browser control remains serialized by the existing per-agent control
token.

The slave reconnects automatically after a network interruption. Its active agents keep
running while disconnected and are presented to the master again after the connection
and subscriptions recover.

## Session history

The master's resume palette includes the local host and every connected slave. Session
listing and search run against each selected host's own persisted/imported history index;
results carry a host identity so equal agent or session IDs cannot collide. Resuming a
remote result happens on the host that owns its history and returns a namespaced live
agent to the master's ordinary agent rail. A disconnected host remains visible but cannot
be searched or resumed until it reconnects.
