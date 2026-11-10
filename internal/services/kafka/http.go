package kafka

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// route is one MSK REST operation. Its pattern is the path split on "/",
// with "{arn}" standing for one percent-encoded cluster ARN.
type route struct {
	method    string
	pattern   []string
	operation string
}

var routes = []route{
	{http.MethodPost, []string{"v1", "clusters"}, "CreateCluster"},
	{http.MethodGet, []string{"v1", "clusters"}, "ListClusters"},
	{http.MethodGet, []string{"v1", "clusters", "{arn}"}, "DescribeCluster"},
	{http.MethodDelete, []string{"v1", "clusters", "{arn}"}, "DeleteCluster"},
	{http.MethodGet, []string{"v1", "clusters", "{arn}", "bootstrap-brokers"}, "GetBootstrapBrokers"},
	{http.MethodGet, []string{"v1", "clusters", "{arn}", "nodes"}, "ListNodes"},
	{http.MethodPost, []string{"api", "v2", "clusters"}, "CreateClusterV2"},
	{http.MethodGet, []string{"api", "v2", "clusters"}, "ListClustersV2"},
	{http.MethodGet, []string{"api", "v2", "clusters", "{arn}"}, "DescribeClusterV2"},
	{http.MethodGet, []string{"v1", "tags", "{arn}"}, "ListTagsForResource"},
	{http.MethodPost, []string{"v1", "tags", "{arn}"}, "TagResource"},
	{http.MethodDelete, []string{"v1", "tags", "{arn}"}, "UntagResource"},
}

// match returns the operation for a request and the ARN in its path, if any.
// escapedPath must be the raw path: ARNs contain "/", which clients send as
// %2F.
func match(method, escapedPath string) (operation, arn string) {
	segments := strings.Split(strings.Trim(escapedPath, "/"), "/")
	for _, r := range routes {
		if r.method != method || len(r.pattern) != len(segments) {
			continue
		}
		arn, ok := "", true
		for i, part := range r.pattern {
			if part == "{arn}" {
				decoded, err := url.PathUnescape(segments[i])
				if err != nil || decoded == "" {
					ok = false
					break
				}
				arn = decoded
				continue
			}
			if part != segments[i] {
				ok = false
				break
			}
		}
		if ok {
			return r.operation, arn
		}
	}
	return "", ""
}

// Operation names the MSK operation for a request, or "" when the request is
// not one this service serves.
func Operation(method, escapedPath string) string {
	op, _ := match(method, escapedPath)
	return op
}

type createClusterRequest struct {
	provisionedConfig
	ClusterName string            `json:"clusterName"`
	Tags        map[string]string `json:"tags"`
}

type createClusterV2Request struct {
	ClusterName string             `json:"clusterName"`
	Tags        map[string]string  `json:"tags"`
	Provisioned *provisionedConfig `json:"provisioned"`
	Serverless  json.RawMessage    `json:"serverless"`
}

// Handle serves one MSK REST request.
func (s *Service) Handle(w http.ResponseWriter, r *http.Request) error {
	op, arn := match(r.Method, r.URL.EscapedPath())
	switch op {
	case "CreateCluster":
		var in createClusterRequest
		if err := decode(r, &in); err != nil {
			return err
		}
		c, err := s.CreateProvisioned(r.Context(), in.ClusterName, in.provisionedConfig, in.Tags)
		if err != nil {
			return err
		}
		return respond(w, http.StatusOK, map[string]any{"clusterArn": c.ARN, "clusterName": c.Name, "state": c.State})
	case "CreateClusterV2":
		var in createClusterV2Request
		if err := decode(r, &in); err != nil {
			return err
		}
		c, err := s.createV2(r, in)
		if err != nil {
			return err
		}
		return respond(w, http.StatusOK, map[string]any{"clusterArn": c.ARN, "clusterName": c.Name, "clusterType": c.Type, "state": c.State})
	case "ListClusters":
		clusters, err := s.List(r.URL.Query().Get("clusterNameFilter"), typeProvisioned)
		if err != nil {
			return err
		}
		return respond(w, http.StatusOK, map[string]any{"clusterInfoList": mapClusters(clusters, clusterInfoV1)})
	case "ListClustersV2":
		q := r.URL.Query()
		clusters, err := s.List(q.Get("clusterNameFilter"), strings.ToUpper(q.Get("clusterTypeFilter")))
		if err != nil {
			return err
		}
		return respond(w, http.StatusOK, map[string]any{"clusterInfoList": mapClusters(clusters, clusterInfoV2)})
	case "DescribeCluster":
		c, err := s.Get(arn)
		if err != nil {
			return err
		}
		return respond(w, http.StatusOK, map[string]any{"clusterInfo": clusterInfoV1(c)})
	case "DescribeClusterV2":
		c, err := s.Get(arn)
		if err != nil {
			return err
		}
		return respond(w, http.StatusOK, map[string]any{"clusterInfo": clusterInfoV2(c)})
	case "DeleteCluster":
		c, err := s.Delete(r.Context(), arn)
		if err != nil {
			return err
		}
		return respond(w, http.StatusOK, map[string]any{"clusterArn": c.ARN, "state": c.State})
	case "GetBootstrapBrokers":
		c, err := s.Get(arn)
		if err != nil {
			return err
		}
		return respond(w, http.StatusOK, map[string]any{"bootstrapBrokerString": c.BootstrapBrokers()})
	case "ListNodes":
		c, err := s.Get(arn)
		if err != nil {
			return err
		}
		return respond(w, http.StatusOK, map[string]any{"nodeInfoList": nodeInfo(c)})
	case "ListTagsForResource":
		c, err := s.Get(arn)
		if err != nil {
			return err
		}
		return respond(w, http.StatusOK, map[string]any{"tags": c.Tags})
	case "TagResource":
		var in struct {
			Tags map[string]string `json:"tags"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		if err := s.Tag(arn, in.Tags); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	case "UntagResource":
		if err := s.Untag(arn, r.URL.Query()["tagKeys"]); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	default:
		return notFound("no MSK operation matches %s %s", r.Method, r.URL.Path)
	}
}

func (s *Service) createV2(r *http.Request, in createClusterV2Request) (cluster, error) {
	switch {
	case in.Provisioned != nil && in.Serverless != nil:
		return cluster{}, badRequest("specify either provisioned or serverless, not both")
	case in.Provisioned != nil:
		return s.CreateProvisioned(r.Context(), in.ClusterName, *in.Provisioned, in.Tags)
	case in.Serverless != nil:
		return s.CreateServerless(r.Context(), in.ClusterName, in.Serverless, in.Tags)
	default:
		return cluster{}, badRequest("provisioned or serverless is required")
	}
}

// timestamp renders times the way MSK does: ISO 8601 with milliseconds.
func timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// brokerSoftware is the BrokerSoftwareInfo of a cluster: its Kafka version
// and, when the caller attached one, its MSK configuration.
func brokerSoftware(c cluster) map[string]any {
	out := map[string]any{"kafkaVersion": c.KafkaVersion()}
	if c.Provisioned != nil && len(c.Provisioned.ConfigurationInfo) > 0 {
		var cfg struct {
			ARN      string `json:"arn"`
			Revision int64  `json:"revision"`
		}
		if json.Unmarshal(c.Provisioned.ConfigurationInfo, &cfg) == nil {
			out["configurationArn"] = cfg.ARN
			out["configurationRevision"] = cfg.Revision
		}
	}
	return out
}

// clusterInfoV1 is the ClusterInfo shape of DescribeCluster and ListClusters.
func clusterInfoV1(c cluster) map[string]any {
	out := map[string]any{
		"clusterArn":                c.ARN,
		"clusterName":               c.Name,
		"creationTime":              timestamp(c.Created),
		"currentBrokerSoftwareInfo": brokerSoftware(c),
		"currentVersion":            c.CurrentVersion,
		"state":                     c.State,
		"tags":                      c.Tags,
	}
	if p := c.Provisioned; p != nil {
		putProvisioned(out, p)
	}
	return out
}

// clusterInfoV2 is the Cluster shape of DescribeClusterV2 and ListClustersV2.
func clusterInfoV2(c cluster) map[string]any {
	out := map[string]any{
		"clusterArn":     c.ARN,
		"clusterName":    c.Name,
		"clusterType":    c.Type,
		"creationTime":   timestamp(c.Created),
		"currentVersion": c.CurrentVersion,
		"state":          c.State,
		"tags":           c.Tags,
	}
	if p := c.Provisioned; p != nil {
		provisioned := map[string]any{"currentBrokerSoftwareInfo": brokerSoftware(c)}
		putProvisioned(provisioned, p)
		out["provisioned"] = provisioned
	}
	if c.Serverless != nil {
		out["serverless"] = c.Serverless
	}
	return out
}

// putProvisioned copies the broker settings a caller supplied into out.
func putProvisioned(out map[string]any, p *provisionedConfig) {
	out["numberOfBrokerNodes"] = p.NumberOfBrokerNodes
	out["enhancedMonitoring"] = p.EnhancedMonitoring
	raw := map[string]json.RawMessage{
		"brokerNodeGroupInfo":  p.BrokerNodeGroupInfo,
		"clientAuthentication": p.ClientAuthentication,
		"encryptionInfo":       p.EncryptionInfo,
		"loggingInfo":          p.LoggingInfo,
		"openMonitoring":       p.OpenMonitoring,
	}
	for key, value := range raw {
		if len(value) > 0 {
			out[key] = value
		}
	}
	if p.StorageMode != "" {
		out["storageMode"] = p.StorageMode
	}
}

// nodeInfo describes the cluster's single broker.
func nodeInfo(c cluster) []map[string]any {
	if c.Port == 0 {
		return []map[string]any{}
	}
	instanceType := "kafka.serverless"
	if c.Provisioned != nil {
		var group struct {
			InstanceType string `json:"instanceType"`
		}
		_ = json.Unmarshal(c.Provisioned.BrokerNodeGroupInfo, &group)
		instanceType = group.InstanceType
	}
	return []map[string]any{{
		"addedToClusterTime": timestamp(c.Created),
		"instanceType":       instanceType,
		"nodeARN":            strings.Replace(c.ARN, ":cluster/", ":broker/", 1) + "/1",
		"nodeType":           "BROKER",
		"brokerNodeInfo": map[string]any{
			"brokerId":                  1,
			"clientVpcIpAddress":        c.Address,
			"currentBrokerSoftwareInfo": brokerSoftware(c),
			"endpoints":                 []string{c.Address},
		},
	}}
}

func mapClusters(clusters []cluster, shape func(cluster) map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, shape(c))
	}
	return out
}

func decode(r *http.Request, into any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return badRequest("read request body: %v", err)
	}
	if len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			return badRequest("request body is not valid JSON: %v", err)
		}
		return badRequest("invalid request: %v", err)
	}
	return nil
}

func respond(w http.ResponseWriter, status int, payload any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(payload)
}
