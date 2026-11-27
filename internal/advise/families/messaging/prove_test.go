package messaging

import (
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/advise"
)

func proofOf(t *testing.T, option string, answers map[string]any) advise.ProofScript {
	t.Helper()
	f := New(documented, listPrices)
	r := advise.NewRequirements(f.Questions(), nil)
	if answers != nil {
		d := assess(t, answers)
		r = d.Requirements(f)
	}
	return f.Proof(option, r, advise.ProofTarget{Account: "912345678901", Region: "eu-west-1", Rate: 300, Duration: 20 * time.Second, Name: "abc-" + option})
}

func TestEachQueueStreamAndBusOptionHasAProducerProof(t *testing.T) {
	cases := map[string][]string{
		SQSStandard: {"AmazonSQS.CreateQueue", "AmazonSQS.SendMessage"},
		SNSFanout:   {`Action: "CreateTopic"`, `Action: "Subscribe"`, `Action: "Publish"`},
		EventBridge: {"AWSEvents.PutRule", "AWSEvents.PutTargets", "AWSEvents.PutEvents"},
		Kinesis:     {"Kinesis_20131202.CreateStream", "Kinesis_20131202.PutRecord"},
	}
	for option, calls := range cases {
		p := proofOf(t, option, nil)
		if p.Unprovable != "" || len(p.Operations) == 0 {
			t.Errorf("%s: %+v", option, p)
			continue
		}
		for _, want := range append(calls, `rate: 300,`, `duration: "20s"`, `const account = "912345678901";`, `const region = "eu-west-1";`, `"proof-abc-`+option+`"`) {
			if !strings.Contains(p.Script, want) {
				t.Errorf("%s script lacks %q", option, want)
			}
		}
	}
}

func TestFIFOAndMSKSayWhyTheyCannotBeProved(t *testing.T) {
	for option, want := range map[string]string{SQSFIFO: "FIFO queues", MSK: "Kafka client"} {
		if p := proofOf(t, option, nil); !strings.Contains(p.Unprovable, want) || p.Script != "" {
			t.Errorf("%s: %+v", option, p)
		}
	}
}

func TestProofsSubscribeEveryConsumerAndCapPayloads(t *testing.T) {
	p := proofOf(t, SNSFanout, map[string]any{"consumers": 4.0, "message_size": 512.0})
	if !strings.Contains(p.Script, "const consumers = 4;") || !strings.Contains(p.Script, `"x".repeat(65536)`) {
		t.Errorf("script:\n%s", p.Script)
	}
	p = proofOf(t, EventBridge, map[string]any{"consumers": 9.0})
	if !strings.Contains(p.Script, "const targets = 5;") {
		t.Errorf("EventBridge allows five targets per rule:\n%s", p.Script)
	}
}

func TestAnswersAreEncodedSafelyIntoTheScript(t *testing.T) {
	f := New(documented, listPrices)
	r := advise.NewRequirements(f.Questions(), nil)
	p := f.Proof(SQSStandard, r, advise.ProofTarget{Account: "912345678901", Region: `x"; evil(); "`, Rate: 1, Duration: time.Second, Name: "n"})
	if strings.Contains(p.Script, `x"; evil()`) || !strings.Contains(p.Script, `"x\"; evil(); \""`) {
		t.Errorf("region not escaped:\n%s", p.Script)
	}
}

func TestTheProofLoadIsThePeakInTheStatedRegion(t *testing.T) {
	d := assess(t, map[string]any{"peak_rate": 2500.0, "region": "ap-south-1"})
	peak, region := New(documented, listPrices).ProofLoad(d.Requirements(New(documented, listPrices)))
	if peak != 2500 || region != "ap-south-1" {
		t.Errorf("got %v %s", peak, region)
	}
}
