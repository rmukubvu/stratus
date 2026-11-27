# Architecture decisions (stratus advise)

`stratus advise` helps a team pick AWS services for a workload and records
the choice as an architecture decision record (ADR). It asks a short set of
questions, rules out the options that cannot meet a hard requirement, checks
the rest against AWS's documented default quotas at the stated peak, ranks
them with an ops-overhead score, estimates what each costs a month from a
dated snapshot of AWS's price list, and writes the ADR in the
[MADR](https://adr.github.io/madr/) layout.

Every verdict carries its reasons, and every reason that rests on a quota
links to the AWS page the number came from. Stratus makes no LLM calls: the
rules are code, reviewed like the rest of Stratus. When an agent drives it
over MCP, the agent holds the conversation and Stratus supplies the facts.

The first decision family is **messaging and eventing**: SQS standard, SQS
FIFO, SNS fanning out to SQS, EventBridge, Kinesis Data Streams and MSK.

## Quick start

```sh
stratus serve &
stratus advise ask messaging --title "Order events"
```

`ask` walks the questions (press Enter to keep a default), then prints the
ranking:

```
1. kinesis        fits with caveats   95/100  ~$345.04/month
     caveat: 3 consumers reading every record exceed 2 MiB/s of shared reads per shard; use enhanced fan-out or more shards.
2. msk            fits with caveats   85/100  ~$612.43/month
     caveat: Nothing here needs Kafka; a managed queue or stream does the job with no cluster to run.
3. sqs-standard   ruled out            0/100  ~$946.08/month
     ruled out: A consumed message is deleted, so it cannot be replayed.
...
Costs: on-demand list prices published 2026-11-26, at the average rate; see the ADR for the breakdown.

Next: stratus advise decide adr-6cd89c5e69ce kinesis --why "..."
```

Then choose and write the ADR into the repository:

```sh
stratus advise decide adr-6cd89c5e69ce kinesis --why "consumers replay orders per customer"
stratus advise adr adr-6cd89c5e69ce --out docs/adr/0001-order-events.md
```

[examples/advise-messaging/0001-order-events.md](../examples/advise-messaging/0001-order-events.md)
is a complete ADR made this way.

## Step by step

| Command | What it does |
| --- | --- |
| `stratus advise families` | The families, with their questions and options (JSON) |
| `stratus advise start <family> [--title t] [id=value...]` | Start a decision, optionally with answers |
| `stratus advise answer <decision> id=value...` | Answer questions, e.g. `ordering=per_key peak_rate=2000` |
| `stratus advise assess <decision>` | Judge and rank the options against the answers |
| `stratus advise decide <decision> <option> [--why text]` | Choose an option that was not ruled out |
| `stratus advise adr <decision> [--out file]` | The ADR as Markdown |
| `stratus advise show <decision>` / `list` | A decision with its pending questions, or all of them |

Questions left unanswered take their defaults, and the ADR marks them as
"assumed (default)" so reviewers can see what nobody actually said.

The steps stay in order. Changing an answer drops the earlier assessment, so
a ranking always describes the current answers. Deciding needs an
assessment, and only an option that was not ruled out can be chosen:

```
stratus advise: OptionRuledOut: sqs-standard was ruled out: Standard queues make a best effort at order but do not guarantee it. (hint: choose one of: kinesis, msk)
```

## Verdicts and scores

Each reason is one of four kinds:

| Kind | Effect |
| --- | --- |
| Ruled out | Breaks a hard requirement; the option cannot be chosen |
| Caveat | Works, with a cost or condition worth recording; -15 |
| Strength | Something the option does well for this workload; +5 |
| Not checked | A quota Stratus does not model; never changes the verdict |

The fit score starts at 100. Each caveat also lowers the ceiling by 5, so
only an option without caveats scores 100. Options rank by verdict, then
score, then lower ops overhead.

**Ops overhead** scores each option 0 (none) to 3 (a team's job) on capacity
planning, patching and upgrades, scaling, failure modes and on-call, and the
skills needed, out of 15. Every family asks how much the team can run (none,
some, a platform team: up to 3, 7 or 15); an option above that gets a caveat
rather than being ruled out. The scores are judgement. They live in
`internal/advise/families/<family>/ops.go`, each with its reason, and the
ADR labels them as judgement.

## Cost

Each option gets a monthly estimate at the stated **average** rate, from
AWS on-demand list prices, before the free tier, taxes and data transfer.
The ADR shows every option's total in the ranking and the chosen option's
breakdown: each line with its quantity, and the assumptions of the usage
model (no batching, provisioned Kinesis sized for the peak, three MSK
brokers and so on), so a reviewer can redo the sum.

Prices come from a snapshot of [AWS's public price
list](https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/index.json)
shipped in the binary (`internal/pricing/snapshot.json`), so advice works
offline and gives the same answer in CI. The ADR states the date AWS
published the prices. A price missing for the region is listed as such and
the total becomes "at least", never a silent zero.

`scripts/pricing/snapshot.py` rebuilds the snapshot from the offer files
(no AWS account needed); maintainers rerun it when prices change.

| Option | What is priced |
| --- | --- |
| SQS standard / FIFO | Send, receive and delete per message, per 64 KiB chunk |
| SNS to SQS | Publishes (first million free) plus each consumer's queue requests; SNS-to-SQS delivery is free |
| EventBridge | Custom events per 64 KiB chunk; targets are billed by their own service |
| Kinesis | Provisioned shard-hours for the peak, PUT payload units, retention beyond a day and beyond 7 days, enhanced fan-out when shared reads would not keep up |
| MSK | Three kafka.m7g.large brokers and storage for the retained data, replicated three times |

## The messaging family

| Question | ID | Default |
| --- | --- | --- |
| Must consumers see messages in order? (`none`, `per_key`, `global`) | `ordering` | `none` |
| Peak rate, messages/s | `peak_rate` | 100 |
| Average rate, messages/s | `average_rate` | 10 |
| Typical message size, KiB | `message_size` | 4 |
| Independent consumers that need every message | `consumers` | 1 |
| Do consumers re-read past messages? | `replay` | false |
| How long unconsumed messages are kept, days | `retention_days` | 4 |
| Delivery (`at_least_once`, `exactly_once`) | `delivery` | `at_least_once` |
| Route messages by content? | `routing` | false |
| Need the Kafka API? | `kafka` | false |
| AWS region | `region` | `us-east-1` |
| How much the team can run (`none`, `some`, `platform_team`) | `ops_capacity` | `some` |

The rules use these documented quotas, kept once in `internal/limits`
(`limits.Documented()`), checked against AWS's pages in November 2026:

| Quota | Value | Source |
| --- | --- | --- |
| SQS maximum message size | 1 MiB | [SQS message quotas](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/quotas-messages.html) |
| SQS maximum retention | 14 days | same |
| SQS FIFO unbatched / batched | 300 / 3,000 per second | [SQS service quotas](https://docs.aws.amazon.com/general/latest/gr/sqs-service.html) |
| High-throughput FIFO, batched | 700,000 (us-east-1, us-west-2, eu-west-1) down to 24,000, by region | same |
| SNS published messages per second | 30,000 (us-east-1), 9,000, 1,500 or 300, by region | [SNS quotas](https://docs.aws.amazon.com/general/latest/gr/sns.html) |
| SNS maximum message size | 256 KiB | same |
| EventBridge PutEvents requests per second | 10,000, 2,400, 1,200, 600 or 400, by region | [EventBridge quotas](https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-quota.html) |
| EventBridge targets per rule | 5 | same |
| Kinesis per shard: write | 1,000 records/s and 1 MiB/s | [Kinesis quotas](https://docs.aws.amazon.com/streams/latest/dev/service-sizes-and-limits.html) |
| Kinesis per shard: shared reads | 2 MiB/s | same |
| Kinesis maximum record size / retention | 10 MiB / 365 days | same |

The SNS publish and EventBridge PutEvents rates are also enforced by the
`aws-default` limits profile (see [limits.md](limits.md)), from the same
values, so a load run against the chosen option hits the quota the ADR
cites.

A few things the family does not model yet: SNS FIFO topics, EventBridge's
per-event size limit (reported as not checked for events over 256 KiB), MSK
broker sizing beyond the three-broker baseline, Kinesis on-demand pricing
and Savings Plans or reserved pricing. The verdicts are a quota check at the stated peak, not a
load test.

## Agents and SDKs

Over MCP the tools are `stratus_advise_families`, `stratus_advise_start`,
`stratus_advise_answer`, `stratus_advise_assess`, `stratus_advise_decide`,
`stratus_advise_adr` and `stratus_advise_get`. Their descriptions tell the
agent to ask the person each pending question rather than guess, and to let
the person make the choice.

```ts
const d = await stratus.advise.start({ family: "messaging", title: "Order events", answers: { replay: true, consumers: 3 } });
const ranked = await stratus.advise.assess(d.id);
await stratus.advise.decide(d.id, ranked.recommended!, "we replay");
await fs.writeFile("docs/adr/0001-order-events.md", await stratus.advise.adr(d.id));
```

```python
d = stratus.advise.start("messaging", title="Order events", answers={"replay": True, "consumers": 3})
ranked = stratus.advise.assess(d["id"])
stratus.advise.decide(d["id"], ranked["recommended"], "we replay")
Path("docs/adr/0001-order-events.md").write_text(stratus.advise.adr(d["id"]))
```

The control API endpoints are listed in [control-api.md](control-api.md#architecture-decisions).

## State

Decisions live in Stratus's metadata store (bucket prefix `advise-`), so
they survive restarts and are covered by `stratus reset advise` and by
snapshots. Export the ADR to keep a decision with the code.

## Adding a family

A family is a package under `internal/advise/families/` that implements
`advise.Family`: its questions (each with a default and a one-line why), its
options with their ops profiles, and `Assess`, which turns the answers into
reasons per option. Quotas come through `advise.QuotaSource`, backed by
`internal/limits`, so add any new number to `limits.Documented()` with its
AWS source. Register the family in `internal/app/advise.go` and pin its
behaviour with golden scenarios like
`internal/advise/families/messaging/messaging_test.go`. To estimate cost,
implement `advise.Coster` as well, reading prices through `advise.PriceBook`
(backed by `internal/pricing`); add any new price to
`scripts/pricing/snapshot.py` and rebuild the snapshot.
