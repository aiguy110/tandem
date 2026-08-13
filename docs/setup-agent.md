# Tandem setup guide for coding agents

Use this guide when a user launches `tandem setup --agent`, or when Tandem asks
you to help with a configuration compatibility upgrade.

## Safety and completion

Read `$TANDEM_HOME/config.yml` (normally `~/.tandem/config.yml`) before making
changes. Explain proposed changes to the user first. Preserve top-level
`agents:`, `harnesses:`, and any unrecognized keys: they are operator-owned and
not part of Tandem's setup settings.

Tandem owns `settings.configVersion`. Do not edit it directly. After the user
confirms the configuration is complete, have them run:

```sh
tandem setup --complete
```

That command records the compatibility version after preserving all other
settings.

## Fresh installation

For a fresh installation, help the user choose only the settings they need:

- `projectRoots`: directories Tandem scans for repositories. The default is
  `~/Projects`.
- `bind` and `port`: the daemon address. Keep the default localhost bind unless
  the user deliberately has a secure remote-access plan.
- `browserDriver`: `local` uses installed Chrome/Chromium; `steel` needs a
  reachable Steel service and its URL/API key.
- `node`: `managed` lets Tandem provision its pinned Node runtime; `system`
  uses a user-supplied command or `node` on PATH.
- `languageModel` and `voice` are optional OpenAI-compatible endpoints for
  transcript-derived features and spoken responses.

The full field schema and environment-variable precedence are documented in
`config.yml.example`, `docs/browser.md`, and `docs/voice.md` in this same
release.

## Compatibility history

### Untracked configuration to version 1

Version 1 is the first release that records `settings.configVersion`. Earlier
configurations have no version marker. They remain compatible with this
release; inspect them for the settings above, make only user-approved changes,
then have the user run `tandem setup --complete` to record version 1.

### Version 1

No additional migration is required. A user may still run `tandem setup` to
review or edit settings interactively.
