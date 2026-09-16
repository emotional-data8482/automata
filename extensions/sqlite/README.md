# SQLite runtime store

This module is the local persistent `core.Store` adapter selected by the
durable-runtime T01 prototype and exposed for the T02 public lifecycle.

It uses WAL, `synchronous=FULL`, one database connection, and a nonblocking OS
advisory lock held for the store lifetime. The supported T02 envelope is one
owner process on a local filesystem with reliable advisory locking. Opening a
second owner fails with `ErrOwned`; unsupported schema versions are rejected
before persistent PRAGMA changes.

The current owner lock uses Unix `flock`; this T02 adapter is therefore
supported on Unix-like systems only. Cross-platform locking is deferred until
the storage contract is hardened in T03.

T03 will add the full conformance/fault matrix, recovery paging, historical
control receipts, bounded encodings, and upgrade/backup policy. This module
does not claim power-loss or distributed ownership guarantees yet.
