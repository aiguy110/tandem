# Migration contract fixtures

These golden files freeze the Node daemon behavior at the start of the Go migration. They
are inputs to both implementations, not a replacement for the public protocol documents.

- `npm run contract:generate` regenerates canonical JSON deterministically.
- `npm run contract:test` regenerates in memory, checks every checked-in golden byte for
  byte, creates a seeded SQLite database in a temporary directory, and reopens/replays it
  through the production Node `Db` implementation.
- The SQLite database itself is deliberately not checked in. The test checkpoints WAL on
  close and verifies that no WAL sidecar remains.

Paths, timestamps, commit IDs, tokens, and byte payloads are synthetic constants so fixture
output is independent of the checkout and machine. `sqlite-schema.json` is introspected from
a database initialized by the production store.
