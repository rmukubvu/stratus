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
     containers (`http://host.docker.internal:<port>` on a VM or bare metal,
     Stratus's own address when it runs in a container; see
     [Runners in containers](#runners-in-containers-and-kubernetes))
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

Or start a background instance for later steps. It listens on a free port
unless you set `port`, and `AWS_ENDPOINT_URL`, `STRATUS_ENDPOINT` and default
credentials are exported for every following step. When the job ends, a post
step stops it so it removes its containers before the runner moves on:

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
| `port` | free port | Port for the background instance |
| `log-level` | `warn` | Stratus log level |

The action works on Linux, macOS and Windows runners. Lambda execution needs
Docker with Linux containers, which GitHub's Windows runners don't provide;
everything else works without it.

## Self-hosted runners

Both modes are safe on persistent runners and on hosts running several
runners at once: each instance gets its own port, data directory and
container names, and its containers are removed when it stops. If a job is
killed outright, the next Stratus start on that host reaps what it left.

The runner needs:

- Linux or macOS (x64 or arm64), or Windows with Docker Desktop using Linux
  containers, and a runner version that supports `node24` actions.
- `bash` and `curl` on the `PATH` (Git Bash on Windows).
- For Lambda, RDS, ElastiCache and MSK: a Docker daemon Stratus can reach.
  A VM or bare-metal runner with local Docker, a microVM runner, a
  Docker-in-Docker sidecar and a runner container that mounts the host's
  `docker.sock` all work; see
  [Runners in containers and Kubernetes](#runners-in-containers-and-kubernetes).
- Permission to use Docker (the `docker` group, or `DOCKER_HOST` for rootless
  Docker).

Images such as `public.ecr.aws/lambda/python:3.11` and `postgres:16-alpine`
are pulled on first use; a persistent runner keeps them cached.

```yaml
jobs:
  integration:
    runs-on: [self-hosted, linux, x64]
    steps:
      - uses: actions/checkout@v4
      - uses: rmukubvu/stratus@main
        with:
          seed: ./fixtures/snapshots/seeded   # optional
          run: go test ./...
```

## MicroVM runners

Firecracker-based runner fleets (Actuated, or your own on Firecracker or
Cloud Hypervisor) give each job a fresh microVM with its own kernel, its own
Docker daemon and a clean disk, destroyed when the job ends. To Stratus that
is the same as a VM runner with local Docker:

- the action and `stratus ci` need no settings; Stratus publishes engine and
  Lambda ports on the VM's own `127.0.0.1` and bind-mounts Lambda code from
  the VM's disk;
- parallel jobs never meet, because each has its own VM and daemon;
- cleanup is covered twice: Stratus removes its containers when it stops,
  and the VM, daemon and disk are discarded anyway.

What to plan for:

- **The Docker daemon must be running** before Stratus needs it. Lambda, RDS,
  ElastiCache and MSK report the daemon as unavailable until it answers;
  everything else works without it.
- **Images.** A fresh VM has an empty image store, so every job would pull
  `public.ecr.aws/lambda/...`, `postgres`, `valkey` or `redpanda` again. Bake
  them into the VM's root filesystem image, or point Docker at a pull-through
  registry mirror. Stratus never pulls images itself, so it fails fast with
  the `docker pull` command to run rather than stalling the job.
- **Boot time.** Stratus serves a restored snapshot about 30 ms after it
  starts, and in under a second when the snapshot holds a PostgreSQL instance
  (see the [cold boot benchmark](benchmark.md#cold-boot-benchmark)). The rest
  of a job's start-up is the VM and the Docker daemon.
- **Memory.** Size the VM for the engines your suite uses: Redpanda asks for
  1 GiB, PostgreSQL and MySQL a few hundred MB each.

We have not run Stratus on a Firecracker runner ourselves yet, because our
test machines have no KVM. The points above follow from the setup being a
Linux VM with local Docker, which is the configuration CI tests on every
push. If you run Stratus on a microVM fleet, we'd like to hear how it goes.

## Runners in containers and Kubernetes

Stratus starts Lambda, RDS, ElastiCache and MSK as Docker containers, and
has to reach them and hand them their files. How it does that depends on
where it runs relative to the Docker daemon. It works this out at startup
and logs the result (`container reach` at `info` level) when it differs from
the plain VM setup.

| Where Stratus runs | What Stratus does | Settings |
| --- | --- | --- |
| VM, bare metal, Docker Desktop, microVM | Publishes ports on `127.0.0.1`, bind-mounts Lambda code | none |
| Pod with a Docker-in-Docker sidecar (ARC `dind` mode) | Same loopback ports (the pod shares them), copies Lambda code in | none |
| Container with the host's `docker.sock` mounted: GitHub `container:` jobs, Compose, a runner container | Joins a Docker network with its containers and reaches them by name; copies Lambda code in | none |
| Kubernetes pod with the node's `docker.sock` mounted | Publishes ports on the node and reaches them at the node's address; copies Lambda code in | `STRATUS_DOCKER_HOST_ADDRESS` |

### Containers that share the host's Docker socket

When Stratus runs in a container that the daemon knows, it finds that
container and puts everything it starts on a user-defined network with it:

- the network the container is already on, if it is on one (GitHub's
  `container:` jobs and Compose projects always are), or
- a network named `stratus-<instance>` that Stratus creates, joins, and
  removes again when it stops.

Nothing is published on the host, so parallel jobs on one daemon never
compete for ports. Endpoints returned by the API are container names on that
network, such as `stratus-1a2b3c4d5e-rds-orders:5432`, and they resolve for
your tests in the job container and for Lambda functions alike.
`STRATUS_DOCKER_ENDPOINT` is Stratus's own address on the network.

```yaml
jobs:
  integration:
    runs-on: ubuntu-latest
    container: node:22          # GitHub mounts docker.sock into job containers
    steps:
      - uses: actions/checkout@v4
      - uses: rmukubvu/stratus@main
        with:
          run: npm test
```

The job image needs `bash` and `curl` for the action. Pre-pull engine and
Lambda images on the host as usual; they are shared with the host daemon.

### Kubernetes runners on the node's Docker socket

A runner pod that mounts the node's `/var/run/docker.sock` is not a
container the daemon knows, so Stratus cannot join a network with its
containers. Give it the node's address instead. Stratus then publishes ports
on all of the node's interfaces and reaches containers at that address, and
containers call Stratus back on the pod's address:

```yaml
env:
  - name: STRATUS_DOCKER_HOST_ADDRESS
    valueFrom:
      fieldRef:
        fieldPath: status.hostIP
```

Pod-to-node traffic on published ports must be allowed by your network
policy. CI runs exactly this setup on every push, on a minikube node running
Docker: [test/kubernetes](../test/kubernetes) has the pod manifest and the
checks (Lambda calling Stratus back, RDS and MSK from the pod, snapshot
restore, and no containers left after the pod is deleted). Prefer the Docker-in-Docker sidecar where you can: it isolates each
job and needs no settings.

### Lambda code without a shared filesystem

Outside a container Stratus bind-mounts each function's code and layers. In
a container, the daemon would look for those paths on its own host, so
Stratus copies them into each function container before it starts instead.
It takes a few milliseconds for a typical function, once per warm container.

### Overriding detection

| Variable | Values | Meaning |
| --- | --- | --- |
| `STRATUS_DOCKER_REACH` | `auto` (default), `loopback`, `network`, `host` | How to reach containers |
| `STRATUS_DOCKER_NETWORK` | network name | Join this network (implies `network`). It must be user-defined so names resolve |
| `STRATUS_DOCKER_HOST_ADDRESS` | address | The daemon host's address as Stratus sees it (implies `host`) |
| `STRATUS_DOCKER_FILES` | `auto` (default), `bind`, `copy` | How Lambda code reaches the container |

A Stratus container running with `--network host` keeps the loopback
behaviour, because it shares the host's network.

Orphan cleanup also works in containers, with one limit: Stratus recognises
leftovers by host and process, and every container has its own host name, so
the containers of a runner container that was killed outright are removed
only by the next Stratus that uses the same data directory. Ephemeral
runners with their own daemon (Docker-in-Docker, microVMs) don't have this
problem, because the daemon goes away with the job.
