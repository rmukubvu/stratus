package kafka

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	accountID = "000000000000"
	region    = "us-east-1"

	typeProvisioned = "PROVISIONED"
	typeServerless  = "SERVERLESS"

	stateActive   = "ACTIVE"
	stateDeleting = "DELETING"
	stateFailed   = "FAILED"

	// serverlessKafkaVersion is what a serverless cluster reports, since
	// callers do not choose one.
	serverlessKafkaVersion = "3.6.0"
)

var clusterNamePattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z-]{0,63}$`)

// provisionedConfig is the broker configuration of a provisioned cluster, as
// CreateCluster and CreateClusterV2 accept it. Settings Stratus does not act
// on (networking, encryption, monitoring) are kept verbatim and echoed back,
// so infrastructure-as-code tools see no drift.
type provisionedConfig struct {
	BrokerNodeGroupInfo  json.RawMessage `json:"brokerNodeGroupInfo,omitempty"`
	KafkaVersion         string          `json:"kafkaVersion"`
	NumberOfBrokerNodes  int             `json:"numberOfBrokerNodes"`
	ClientAuthentication json.RawMessage `json:"clientAuthentication,omitempty"`
	ConfigurationInfo    json.RawMessage `json:"configurationInfo,omitempty"`
	EncryptionInfo       json.RawMessage `json:"encryptionInfo,omitempty"`
	EnhancedMonitoring   string          `json:"enhancedMonitoring,omitempty"`
	LoggingInfo          json.RawMessage `json:"loggingInfo,omitempty"`
	OpenMonitoring       json.RawMessage `json:"openMonitoring,omitempty"`
	StorageMode          string          `json:"storageMode,omitempty"`
}

func (p *provisionedConfig) validate() error {
	if strings.TrimSpace(p.KafkaVersion) == "" {
		return badRequest("kafkaVersion is required")
	}
	if p.NumberOfBrokerNodes < 1 {
		return badRequest("numberOfBrokerNodes must be at least 1")
	}
	if p.EnhancedMonitoring == "" {
		p.EnhancedMonitoring = "DEFAULT"
	}
	return nil
}

// cluster is the persisted state of one MSK cluster. Whatever the requested
// broker count, Stratus runs one broker; the count is reported as requested.
type cluster struct {
	ARN            string             `json:"arn"`
	Name           string             `json:"name"`
	Type           string             `json:"type"`
	Provisioned    *provisionedConfig `json:"provisioned,omitempty"`
	Serverless     json.RawMessage    `json:"serverless,omitempty"`
	Tags           map[string]string  `json:"tags"`
	Created        time.Time          `json:"created"`
	CurrentVersion string             `json:"current_version"`
	State          string             `json:"state"`
	Address        string             `json:"address"`
	Port           int                `json:"port"`
}

// newCluster validates a create request's common fields and builds the
// cluster it describes, without an endpoint yet.
func newCluster(name string, tags map[string]string, now time.Time) (cluster, error) {
	if !clusterNamePattern.MatchString(name) {
		return cluster{}, badRequest("clusterName must be 1-64 letters, digits or hyphens, starting with a letter or digit")
	}
	if tags == nil {
		tags = map[string]string{}
	}
	return cluster{
		ARN:            fmt.Sprintf("arn:aws:kafka:%s:%s:cluster/%s/%s", region, accountID, name, randomID()),
		Name:           name,
		Tags:           tags,
		Created:        now.UTC(),
		CurrentVersion: "K3AEGXETSR30VB",
		State:          stateActive,
	}, nil
}

// KafkaVersion is the broker version the cluster reports.
func (c cluster) KafkaVersion() string {
	if c.Provisioned != nil {
		return c.Provisioned.KafkaVersion
	}
	return serverlessKafkaVersion
}

// BootstrapBrokers is the connection string clients pass to their Kafka
// library, or "" while the cluster has no running broker.
func (c cluster) BootstrapBrokers() string {
	if c.Port == 0 {
		return ""
	}
	return fmt.Sprintf("%s:%d", c.Address, c.Port)
}

// nameFromARN extracts the cluster name from an ARN such as
// arn:aws:kafka:us-east-1:000000000000:cluster/orders/1b2c...
func nameFromARN(arn string) (string, bool) {
	_, resource, found := strings.Cut(arn, ":cluster/")
	if !found || !strings.HasPrefix(arn, "arn:aws:kafka:") {
		return "", false
	}
	name, _, _ := strings.Cut(resource, "/")
	return name, name != ""
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s-1", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}
