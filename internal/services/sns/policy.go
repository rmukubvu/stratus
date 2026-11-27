package sns

import "encoding/json"

// defaultTopicPolicy is the access policy AWS gives a topic created without
// one, and returns from GetTopicAttributes until a Policy is set: the
// owning account may manage, subscribe to and publish to the topic. Tools
// such as Terraform parse it, so a topic must always have one.
func defaultTopicPolicy(topicARN, account string) string {
	policy := map[string]any{
		"Version": "2008-10-17",
		"Id":      "__default_policy_ID",
		"Statement": []any{map[string]any{
			"Sid":       "__default_statement_ID",
			"Effect":    "Allow",
			"Principal": map[string]any{"AWS": "*"},
			"Action": []string{
				"SNS:GetTopicAttributes", "SNS:SetTopicAttributes", "SNS:AddPermission", "SNS:RemovePermission",
				"SNS:DeleteTopic", "SNS:Subscribe", "SNS:ListSubscriptionsByTopic", "SNS:Publish",
			},
			"Resource":  topicARN,
			"Condition": map[string]any{"StringEquals": map[string]any{"AWS:SourceOwner": account}},
		}},
	}
	raw, _ := json.Marshal(policy)
	return string(raw)
}

// subscriptionAttributeNames are the attributes Subscribe and
// SetSubscriptionAttributes accept for standard topics.
var subscriptionAttributeNames = map[string]bool{
	"DeliveryPolicy": true, "FilterPolicy": true, "FilterPolicyScope": true,
	"RawMessageDelivery": true, "RedrivePolicy": true, "SubscriptionRoleArn": true,
}

// subscriptionAttributes checks the attributes given to Subscribe and adds
// AWS's default of RawMessageDelivery=false.
func subscriptionAttributes(given map[string]string) (map[string]string, error) {
	attrs := map[string]string{"RawMessageDelivery": "false"}
	for name, value := range given {
		if !subscriptionAttributeNames[name] {
			return nil, badRequest("InvalidParameter", "Invalid parameter: Attributes Reason: Unknown attribute "+name)
		}
		if value == "" {
			continue
		}
		attrs[name] = value
	}
	return attrs, nil
}
