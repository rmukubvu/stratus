# Running Stratus in CI

Stratus is built to make CI runs fast, isolated and clean.

## `stratus ci`

```sh
stratus ci -- go test ./...
```

`stratus ci` starts a private Stratus inside the same process, then:

1. picks a free port and creates a fresh data directory,
2. optionally seeds it from a snapshot (`--seed`),
3. waits until Stratus is ready,
4. runs your command with these variables set:
   - `AWS_ENDPOINT_URL` and `STRATUS_ENDPOINT`: the private instance
   - `STRATUS_DOCKER_ENDPOINT`: the same instance as seen from Lambda
     containers (`http://host.docker.internal:<port>`)
   - `STRATUS_PORT`
   - `AWS_DISABLE_HOST_PREFIX_INJECTION=true`, so operations that prefix the
     host (Cloud Map's `DiscoverInstances` calls `data-<host>`) still reach
     Stratus
   - `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION` and
     `AWS_DEFAULT_REGION`, set to harmless defaults only if you haven't set them
5. shuts Stratus down, stops its Lambda, database and cache containers and
   deletes the data directory, whether the command passed, failed or was
   interrupted.

The exit code is your command's. If Stratus itself fails (bad seed, port
problem, never became ready) the exit code is `125`, so you can tell an
infrastructure failure from a test failure.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--seed <dir>` | none | Snapshot directory to start from |
| `--keep-data` | false | Keep the data directory for debugging |
| `--log-level` | `warn` | Stratus log level |
| `--ready-timeout` | `60s` | How long to wait for Stratus |

## Seed once, fork many

Seeding (creating tables, queues, buckets and Lambdas) is usually the slow
part of an integration suite. Do it once, snapshot it, then start every job
from the snapshot:

```sh
# once
stratus serve --data-dir ./fixtures &
stratus wait
./scripts/seed.sh
stratus snapshot save seeded
kill %1

# every job, in parallel if you like
stratus ci --seed ./fixtures/snapshots/seeded -- go test ./orders/...
stratus ci --seed ./fixtures/snapshots/seeded -- go test ./payments/...
```

Each job gets its own copy. Blobs are hard-linked, so seeding is fast and
cheap even with large S3 objects, and writes in one job are never seen by
another job or by the snapshot. RDS databases and ElastiCache clusters come
up with the rows and keys they held when the snapshot was taken, each job on
its own ports (see [engines.md](engines.md#seeded-jobs)). You can also seed a long-running server
with `stratus serve --seed <dir> --data-dir <empty-dir>`.

## No orphaned containers

Every Lambda container Stratus starts is labelled with the instance that
owns it (derived from the data directory), the owning process ID and the
host. On startup Stratus removes:

- containers left by an earlier process that used the same data directory,
  which are always orphans because only one process can hold a data
  directory at a time;
- containers whose owning process ran on this host and no longer exists,
  such as a run killed with `SIGKILL` or an out-of-memory error.

Containers owned by a live process, or by a process on another host
sharing the same Docker daemon, are left alone. Container names include
the instance, so parallel runs never collide.

## GitHub Action

Run a command against a throwaway instance:

```yaml
- uses: actions/setup-go@v5        # or whatever your tests need
- uses: rmukubvu/stratus@main
  with:
    seed: ./fixtures/snapshots/seeded   # optional
    run: go test ./...
```

Or start a background instance for later steps. `AWS_ENDPOINT_URL`,
`STRATUS_ENDPOINT` and default credentials are exported for every
following step:

```yaml
- uses: rmukubvu/stratus@main
  id: stratus
- run: npm test
- run: stratus snapshot save after-tests   # the control CLI works too
```

| Input | Default | Meaning |
| --- | --- | --- |
| `version` | `latest` | Release tag, `latest`, or `source` to build from the action's checkout |
| `run` | empty | Command for `stratus ci`; empty starts a background instance |
| `seed` | empty | Snapshot directory to start from |
| `port` | `4566` | Port for the background instance |
| `log-level` | `warn` | Stratus log level |

The action works on Linux, macOS and Windows runners. Lambda execution needs
Docker with Linux containers, which GitHub's Windows runners don't provide;
everything else works without it.
