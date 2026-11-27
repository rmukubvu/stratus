package messaging

import "github.com/stratus/internal/advise"

// Ops profiles: judgement, scored 0 (none) to 3 (a team's job). Change a
// score here with its reason; ADRs quote both.

var opsSQS = advise.OpsProfile{
	Capacity:     advise.OpsScore{Score: 0, Reason: "No capacity to provision; scales with traffic."},
	Patching:     advise.OpsScore{Score: 0, Reason: "Fully managed; nothing to patch."},
	Scaling:      advise.OpsScore{Score: 0, Reason: "No throughput quota per action to plan around."},
	FailureModes: advise.OpsScore{Score: 1, Reason: "Watch queue age and the dead-letter queue."},
	Skills:       advise.OpsScore{Score: 0, Reason: "Visibility timeouts and retries are the whole model."},
}

var opsFIFO = advise.OpsProfile{
	Capacity:     advise.OpsScore{Score: 0, Reason: "No capacity to provision."},
	Patching:     advise.OpsScore{Score: 0, Reason: "Fully managed; nothing to patch."},
	Scaling:      advise.OpsScore{Score: 1, Reason: "Throughput quotas; message group design decides parallelism."},
	FailureModes: advise.OpsScore{Score: 1, Reason: "A stuck message blocks its group; watch age and the dead-letter queue."},
	Skills:       advise.OpsScore{Score: 1, Reason: "Message groups and deduplication IDs need care."},
}

var opsSNS = advise.OpsProfile{
	Capacity:     advise.OpsScore{Score: 0, Reason: "No capacity to provision."},
	Patching:     advise.OpsScore{Score: 0, Reason: "Fully managed; nothing to patch."},
	Scaling:      advise.OpsScore{Score: 0, Reason: "Regional publish quota, adjustable on request."},
	FailureModes: advise.OpsScore{Score: 1, Reason: "A dead-letter queue per subscription and per queue to watch."},
	Skills:       advise.OpsScore{Score: 1, Reason: "Subscriptions, filter policies and queue policies to keep in step."},
}

var opsEventBridge = advise.OpsProfile{
	Capacity:     advise.OpsScore{Score: 0, Reason: "No capacity to provision."},
	Patching:     advise.OpsScore{Score: 0, Reason: "Fully managed; nothing to patch."},
	Scaling:      advise.OpsScore{Score: 0, Reason: "Regional PutEvents quota, adjustable on request."},
	FailureModes: advise.OpsScore{Score: 1, Reason: "Failed deliveries go to a dead-letter queue per target; a rule that matches nothing fails silently."},
	Skills:       advise.OpsScore{Score: 1, Reason: "Event patterns and schemas to agree on across teams."},
}

var opsKinesis = advise.OpsProfile{
	Capacity:     advise.OpsScore{Score: 2, Reason: "Shard count to size in provisioned mode, or on-demand cost to watch."},
	Patching:     advise.OpsScore{Score: 0, Reason: "Fully managed; nothing to patch."},
	Scaling:      advise.OpsScore{Score: 2, Reason: "Resharding, and hot shards from skewed partition keys."},
	FailureModes: advise.OpsScore{Score: 2, Reason: "Iterator age, a poison record stalling a shard, checkpointing bugs."},
	Skills:       advise.OpsScore{Score: 1, Reason: "Partition keys, checkpoints and consumer libraries."},
}

var opsMSK = advise.OpsProfile{
	Capacity:     advise.OpsScore{Score: 3, Reason: "Broker count, instance size and storage to size and keep sized."},
	Patching:     advise.OpsScore{Score: 2, Reason: "Kafka version upgrades to plan and roll."},
	Scaling:      advise.OpsScore{Score: 3, Reason: "Adding brokers means moving partitions; partitions per topic are hard to change."},
	FailureModes: advise.OpsScore{Score: 2, Reason: "Consumer lag, under-replicated partitions, rebalancing storms."},
	Skills:       advise.OpsScore{Score: 3, Reason: "Kafka expertise across producers, consumers and the cluster."},
}
