package advise

import "fmt"

// OpsScore is one dimension of operational overhead, 0 (none) to 3 (a
// team's job), with the reason for the score.
type OpsScore struct {
	Score  int    `json:"score"`
	Reason string `json:"reason"`
}

// OpsProfile scores what running an option costs a team beyond its bill.
// The scores are judgement, reviewed in Stratus's source, and ADRs label
// them as such.
type OpsProfile struct {
	Capacity     OpsScore `json:"capacity_planning"`
	Patching     OpsScore `json:"patching_and_upgrades"`
	Scaling      OpsScore `json:"scaling"`
	FailureModes OpsScore `json:"failure_modes_and_on_call"`
	Skills       OpsScore `json:"skills_needed"`
}

// Total sums the five scores, 0 to 15.
func (p OpsProfile) Total() int {
	return p.Capacity.Score + p.Patching.Score + p.Scaling.Score + p.FailureModes.Score + p.Skills.Score
}

// Dimensions lists the scores with their names, in a fixed order.
func (p OpsProfile) Dimensions() []NamedOpsScore {
	return []NamedOpsScore{
		{"Capacity planning", p.Capacity},
		{"Patching and upgrades", p.Patching},
		{"Scaling", p.Scaling},
		{"Failure modes and on-call", p.FailureModes},
		{"Skills needed", p.Skills},
	}
}

// NamedOpsScore is an OpsScore with its dimension's name.
type NamedOpsScore struct {
	Name string
	OpsScore
}

// OpsCapacityID is the question every family asks about the team.
const OpsCapacityID = "ops_capacity"

// The team's capacity, and the most ops overhead it carries comfortably.
const (
	CapacityNone     = "none"
	CapacitySome     = "some"
	CapacityPlatform = "platform_team"
	budgetNone       = 3
	budgetSome       = 7
	budgetPlatform   = 15
)

// OpsCapacityQuestion asks how much running of infrastructure the team
// can take on. The core asks it for every family and checks every option
// against it.
func OpsCapacityQuestion() Question {
	return Question{
		ID:     OpsCapacityID,
		Prompt: "How much infrastructure can the team run and be on call for?",
		Why:    "A service the team cannot operate fails at 3 a.m. Options heavier than this get a caveat.",
		Kind:   KindChoice,
		Choices: []Choice{
			{CapacityNone, "None: managed services only"},
			{CapacitySome, "Some: can tune and watch a service"},
			{CapacityPlatform, "A platform team that runs clusters"},
		},
		Default: CapacitySome,
	}
}

func opsBudget(capacity string) int {
	switch capacity {
	case CapacityNone:
		return budgetNone
	case CapacityPlatform:
		return budgetPlatform
	default:
		return budgetSome
	}
}

// opsReason is the caveat an option gets when its overhead exceeds what
// the team said it can carry, or nil.
func opsReason(o Option, capacity string) *Reason {
	total, budget := o.Ops.Total(), opsBudget(capacity)
	if total <= budget {
		return nil
	}
	return &Reason{
		Kind: ReasonCaveat,
		Text: fmt.Sprintf("Ops overhead %d/15 is above what the team said it can carry (%d/15 for %q).", total, budget, capacity),
	}
}
