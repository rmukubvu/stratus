# CloudWatch metrics and alarms

Stratus serves CloudWatch over both protocols the SDKs use: the query
protocol (older SDKs, the CLI) and AWS JSON (current boto3 and other SDKs
that target `GraniteServiceVersion20100801`). Errors come back in the shape
the protocol expects.

## Metrics

`PutMetricData` stores datapoints, including `Values`/`Counts` arrays, which
are expanded so each counted sample feeds statistics. `ListMetrics` and
`GetMetricStatistics` read them back.

## Alarms

| Operation | Notes |
| --- | --- |
| `PutMetricAlarm` | Single-metric alarms are evaluated. Metric-math alarms (`Metrics`) are stored and described but not evaluated. |
| `PutCompositeAlarm` | `AlarmRule` supports `ALARM()`, `OK()`, `INSUFFICIENT_DATA()`, `AND`, `OR`, `NOT`, `TRUE`, `FALSE` and parentheses; children may be named or given by ARN and must exist. `ActionsSuppressor` must name an existing alarm. |
| `DescribeAlarms` | Filters by names, prefix, type, state, action prefix, `ChildrenOfAlarmName`, `ParentsOfAlarmName`; paginates. Without `AlarmTypes`, only metric alarms are returned, as in AWS. |
| `DescribeAlarmsForMetric` | In creation order. |
| `DescribeAlarmHistory` | `ConfigurationUpdate`, `StateUpdate` and `Action` items, with AWS's `HistoryData` JSON. |
| `SetAlarmState` | The forced state holds until the alarm is next evaluated. |
| `DeleteAlarms` | Refused while a composite alarm depends on the alarm. |
| `EnableAlarmActions`, `DisableAlarmActions`, `TagResource`, `UntagResource`, `ListTagsForResource` | |

### How alarms are evaluated

CloudWatch evaluates each alarm once per period, over the most recent
`EvaluationPeriods` periods that have completed. Stratus does the same, but
lazily: the first CloudWatch call in a new period (publishing data or reading
alarms) runs the evaluation that AWS would have run at the period boundary.
The outcome is the same as AWS's as long as something calls CloudWatch, which
a test that waits for an alarm always does.

For each period the alarm's statistic (`Average`, `Sum`, `Minimum`,
`Maximum`, `SampleCount` or a `pNN` extended statistic) is computed over the
datapoints with the alarm's namespace, metric name, dimensions and (if set)
unit. The alarm goes to `ALARM` when at least `DatapointsToAlarm` (default
`EvaluationPeriods`) periods breach the threshold, and to `OK` otherwise.
Periods without data follow `TreatMissingData`:

| `TreatMissingData` | No data in the window | Some data |
| --- | --- | --- |
| `missing` (default) | `INSUFFICIENT_DATA` | only periods with data count |
| `ignore` | the state is kept | only periods with data count |
| `breaching` | `ALARM` | missing periods count as breaching |
| `notBreaching` | `OK` | missing periods count as not breaching |

With `EvaluateLowSampleCountPercentile=ignore`, a percentile over too few
samples to be significant counts as missing.

Each state change gets AWS's reason text, for example
`Threshold Crossed: 3 out of the last 3 datapoints [87.0 (12/11/26 20:53:20), ...]
were greater than the threshold (50.0) (minimum 3 datapoints for OK -> ALARM
transition).`, the matching `StateReasonData` JSON, a `StateUpdate` history
item, and the alarm's actions for the new state. SNS topic actions publish
the notification JSON AWS sends; other action types are accepted and
recorded but not run.

Composite alarms are re-derived whenever a child changes state. Their
`StateReason` names the child that triggered the change
(`<child ARN> transitioned to ALARM at ...`) and `StateReasonData` lists it
under `triggeringAlarms`.
