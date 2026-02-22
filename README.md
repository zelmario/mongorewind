# mongorewind

A terminal UI tool that watches a MongoDB cluster for changes and lets you rewind them — undoing inserts, updates, replaces, and deletes in reverse order.

## Why?

When preparing to release a new version of an application, developers often restore a copy of the production database into a test cluster to verify that everything works correctly. Depending on the size of the database, this restore process can take a significant amount of time.

If something goes wrong during testing and the data needs to be in its original state to run the test again, the only option — without mongorewind — is to wait through another full restore cycle.

With mongorewind, you can instantly rewind all the changes made during the test run and start over, without touching the backup or waiting for a restore.

## How it works

mongorewind opens a cluster-wide [change stream](https://www.mongodb.com/docs/manual/changeStreams/) and records every data-modifying event to a local log file. When you press `R` to rewind, it applies the inverse of each recorded operation in reverse chronological order:

| Recorded operation | Rewind action |
|--------------------|---------------|
| `insert` | `deleteOne` |
| `update` / `replace` | `replaceOne` with pre-image (upsert) |
| `delete` | `replaceOne` with pre-image (upsert) |

Pre-images (the document state *before* each change) are captured automatically using MongoDB's `changeStreamPreAndPostImages` collection option, which mongorewind enables on every collection it finds — and on any new collection the moment it is created.

## Requirements

- **Go 1.24+**
- **MongoDB 6.0+** running as a **replica set** or **sharded cluster**
  (change streams are not available on standalone instances)

## Installation

```bash
git clone https://github.com/zelmario/mongorewind.git
cd mongorewind
go build -o mongorewind .
```

Or install directly:

```bash
go install github.com/zelmario/mongorewind@latest
```

## Usage

```
mongorewind [flags]

Flags:
  -uri string   MongoDB connection URI (default "mongodb://localhost:27017")
  -log string   Path to the on-disk change log (default "rewind.log")
```

Run `mongorewind --help` to see all flags.

### Replica set URI

For replica sets, include all hosts and the `replicaSet` parameter so the driver can discover the primary automatically:

```bash
mongorewind --uri "mongodb://host1:27017,host2:27017,host3:27017/?replicaSet=rs0"
```

### Keyboard controls

| Key | Action |
|-----|--------|
| `R` | Rewind — invert all recorded changes, newest first |
| `C` | Clear — discard the recorded log without rewinding |
| `Q` / `Ctrl-C` | Quit |

### CI / non-interactive rewind

Run `mongorewind --rewind` from any shell or script to trigger a rewind in the already-running instance and wait for it to complete:

```bash
# start the watcher (e.g. as a background service or in another terminal)
mongorewind --uri "mongodb://..." &

# run your test suite
run_tests

# rewind all changes and run again
mongorewind --rewind
run_tests
```

`mongorewind --rewind` exits with code `0` on success and `1` on error, so it integrates naturally into CI pipelines.

If you use a custom `--log` path, pass the same value to `--rewind` so it finds the correct socket:

```bash
mongorewind --log /tmp/mytest.log --uri "mongodb://..." &
mongorewind --log /tmp/mytest.log --rewind
```

## Display

```
  mongorewind
  ────────────────────────────────────────────────────────────────
  ● watching   4 operation(s) pending rewind

  Database                       Inserts    Updates    Deletes
  ────────────────────────────────────────────────────────────────
  myapp                                1          2          1
  ────────────────────────────────────────────────────────────────
  Total                                1          2          1

  R=rewind  C=clear  Q=quit
```

The status indicator shows `● watching` (green) while the change stream is active, or `○ idle` (yellow) during a rewind operation.

## Development

`testdata.sh` is a load generator that hammers MongoDB with random inserts, updates, and deletes across multiple databases and collections. Useful for testing mongorewind:

```bash
# defaults: mongodb://localhost:27017, 0.1s delay between ops, 5 parallel workers
./testdata.sh

# custom URI, faster
./testdata.sh "mongodb://localhost:27017/?replicaSet=rs0" 0.05 10
```

Requires `mongosh` on `$PATH`.

## Notes

- **Replica set requirement** — MongoDB change streams require a replica set or sharded cluster. A standalone `mongod` will not work. You can start a single-node replica set for local development with `mongod --replSet rs0` followed by `rs.initiate()` in the mongo shell.
- **Pre-images** — mongorewind uses `fullDocumentBeforeChange` to capture the state of a document before every update or delete. It enables this automatically on all collections via `collMod`, and polls every 2 seconds to catch newly created collections.
- **Log file** — Changes are persisted as raw BSON to `rewind.log` (configurable with `--log`). The file is truncated on startup and after a successful rewind, so each session starts clean.
- **Scope** — The change stream watches the entire cluster. System databases (`admin`, `local`, `config`) and `system.*` collections are ignored.

## License

MIT
