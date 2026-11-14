# k6 load run against Stratus

`load.js` writes to a provisioned DynamoDB table at 200 requests a second
for 30 seconds. Half the writes share one partition key, so under a limits
profile that key throttles while the rest of the table keeps up.

## Run it

```sh
stratus serve &
stratus load run --name orders --limits strict -- k6 run load.js
```

`stratus load run` starts a load run, applies the `strict` limits profile,
runs k6, then stops the run, clears the profile and prints the summary:
requests per second, how many were throttled, latency percentiles and the
busiest operations. It exits with k6's exit code.

Add seeded chaos to the same run:

```sh
stratus load run --name orders-chaos --limits strict --chaos chaos.json -- k6 run load.js
```

`chaos.json` slows 10% of writes and fails 2% with a 500. The summary
counts those as `faults`, separate from throttles and from real `errors`,
so a failure the test did not plan for stands out.

## Look at a run afterwards

```sh
stratus load list                 # recent runs
stratus load status latest        # summary, per-second timeline, operations
stratus diagnose                  # why requests failed, with next steps
```

k6 gets `STRATUS_ENDPOINT` and `STRATUS_RUN_ID` from `stratus load run`, so
the script calls the right Stratus and can tag its own output with the run.

## Your own application

In a real test k6 calls your application, and your application calls
Stratus through its AWS SDK. Point the SDK at Stratus (`stratus env`
prints the variables), then run k6 against the application the same way:

```sh
stratus load run --name checkout --limits aws-default -- k6 run checkout.js
```
