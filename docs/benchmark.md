# Test Isolation Benchmark

Most integration suites spend their time getting back to a known state
between tests, not running them. This benchmark measures that cost for each
way of isolating tests against Stratus.

```sh
go run ./cmd/stratus-bench -tests 20 -suite 200
go run ./cmd/stratus-bench -scale 4      # a 4x larger fixture
```

## What it measures

Each strategy starts from the same seeded fixture: S3 buckets with objects,
DynamoDB tables with items and SQS queues with messages, all created through
the AWS APIs as a real setup step would. Then, for every simulated test, it:

1. Mutates state like a test does: deletes an object, writes items, enqueues
   messages and creates a bucket of its own.
2. Isolates using the strategy. **Only this step is timed.**
3. Verifies that the state is exactly the seeded fixture again, with the
   right bucket, object, item and message counts. A strategy that leaks state
   fails the run instead of looking fast.

| Strategy | Between tests |
| --- | --- |
| fresh emulator + setup | Stop the emulator, start a new one, run the setup calls again. This is the usual pattern for an emulator without a control API. |
| reset + setup | `POST /reset`, then run the setup calls again. |
| fresh emulator from snapshot (`--seed`) | Stop the emulator and start a new one seeded from a snapshot, with no setup calls. This is what `stratus ci --seed` gives each parallel CI job. |
| snapshot restore | `POST /snapshots/seeded/restore` |

## Results

Measured on a 4 vCPU Linux cloud container (2026-11-05). CI runs the
benchmark on every push and writes the table to the job summary, so these
numbers can be rechecked on any commit.

Fixture: 4 buckets x 100 objects (16 KiB each), 3 tables x 500 items, 3 queues x 50 messages. 20 tests per strategy; times are per test.

| Strategy | Median | p90 | Speed-up | Overhead for 200 tests |
| --- | ---: | ---: | ---: | ---: |
| fresh emulator + setup | 982.1 ms | 1066.5 ms | 1.0x | 196.4 s |
| reset + setup | 1028.2 ms | 1106.8 ms | 1.0x | 205.6 s |
| fresh emulator from snapshot (`--seed`) | 12.5 ms | 15.2 ms | 78.4x | 2.5 s |
| snapshot restore | 11.1 ms | 14.3 ms | 88.9x | 2.2 s |

## Reading the numbers

- **Setup calls are the cost.** Restarting Stratus or resetting it is cheap.
  Replaying about 2,000 API calls is not. Both strategies that rerun setup
  land near one second per test, and that grows with the fixture.
- **Snapshots remove setup from the loop.** Restore swaps in a copy of the
  metadata database and hard-links blobs, so its cost depends on the size of
  the state, not on how many calls built it. Two hundred tests go from about
  three minutes of isolation overhead to about two seconds.
- **`--seed` is just as fast and also parallel.** Each CI job or test worker
  gets its own emulator with its own copy of the fixture, so jobs can run
  concurrently without sharing state.
- **The baseline is generous.** "Fresh emulator" restarts an in-process
  Stratus, which takes milliseconds. An emulator in a container takes seconds
  to start, so the real gap is wider than this table shows.
