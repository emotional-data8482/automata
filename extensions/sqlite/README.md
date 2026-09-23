# SQLite runtime store

This module is the supported local persistent `core.Store` adapter for
`core.Runtime`. It passes the `core/storetest` conformance suite.

It uses WAL, `synchronous=FULL`, one database connection, and a nonblocking OS
advisory lock held for the store lifetime. The supported envelope is one owner
process on a local filesystem with reliable advisory locking. Opening a second
owner fails with `ErrOwned`; unsupported schema versions are rejected before
persistent PRAGMA changes. Prefix scans seek directly to their key range, so
reading one run's transcript or events does not scan other runs' rows.

The owner lock uses Unix `flock`, so the adapter is supported on Unix-like
systems only. Process-kill recovery is tested with subprocess crashes; the
module does not claim power-loss or distributed ownership guarantees. Measured
operating bounds are recorded in `docs/durable-runtime.md` and reproducible with
`go test -run '^$' -bench SQLite`.
