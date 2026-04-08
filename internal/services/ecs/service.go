package ecs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store"
)

const (
	clustersBucket      = "ecs-clusters"
	taskDefsBucket      = "ecs-task-definitions"
	servicesBucket      = "ecs-services"
	tasksBucket         = "ecs-tasks"
	accountID           = "000000000000"
	region              = "us-east-1"
)

type Service struct {
	metadata store.Store
	now      func() time.Time
	mu       sync.Mutex
}

type clusterRecord struct {
	Arn       string    `json:"arn"`
	CreatedAt time.Time `json:"created_at"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
}

type taskDefinitionRecord struct {
	Arn                  string            `json:"arn"`
	Compatibilities      []string          `json:"compatibilities"`
	ContainerDefinitions []map[string]any  `json:"container_definitions"`
	Cpu                  string            `json:"cpu,omitempty"`
	ExecutionRoleArn     string            `json:"execution_role_arn,omitempty"`
	Family               string            `json:"family"`
	Memory               string            `json:"memory,omitempty"`
	NetworkMode          string            `json:"network_mode,omitempty"`
	RequiresCompat       []string          `json:"requires_compatibilities,omitempty"`
	Revision             int               `json:"revision"`
	TaskRoleArn          string            `json:"task_role_arn,omitempty"`
}

type serviceRecord struct {
	Arn            string           `json:"arn"`
	ClusterArn     string           `json:"cluster_arn"`
	CreatedAt      time.Time        `json:"created_at"`
	DesiredCount   int              `json:"desired_count"`
	LoadBalancers  []map[string]any `json:"load_balancers,omitempty"`
	Name           string           `json:"name"`
	Status         string           `json:"status"`
	TaskDefinition string           `json:"task_definition"`
}

type taskRecord struct {
	Arn            string    `json:"arn"`
	ClusterArn     string    `json:"cluster_arn"`
	CreatedAt      time.Time `json:"created_at"`
	LastStatus     string    `json:"last_status"`
	LaunchType     string    `json:"launch_type"`
	TaskDefinition string    `json:"task_definition"`
}

func NewService(metadata store.Store) *Service {
	return &Service{metadata: metadata, now: time.Now}
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch operation {
	case "CreateCluster":
		return s.createCluster(w, r)
	case "ListClusters":
		return s.listClusters(w)
	case "RegisterTaskDefinition":
		return s.registerTaskDefinition(w, r)
	case "DescribeTaskDefinition":
		return s.describeTaskDefinition(w, r)
	case "CreateService":
		return s.createService(w, r)
	case "DescribeServices":
		return s.describeServices(w, r)
	case "ListServices":
		return s.listServices(w, r)
	case "RunTask":
		return s.runTask(w, r)
	case "DeleteService":
		return s.deleteService(w, r)
	default:
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplementedException", Message: "ecs operation is not implemented"}
	}
}

func (s *Service) createCluster(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		ClusterName string `json:"clusterName"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.ClusterName == "" {
		input.ClusterName = "default"
	}
	if cluster, err := s.loadCluster(input.ClusterName); err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"cluster": clusterResponse(cluster)})
		return nil
	}
	record := clusterRecord{
		Arn:       clusterARN(input.ClusterName),
		CreatedAt: s.now().UTC(),
		Name:      input.ClusterName,
		Status:    "ACTIVE",
	}
	if err := s.putRecord(clustersBucket, input.ClusterName, record); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"cluster": clusterResponse(record)})
	return nil
}

func (s *Service) listClusters(w http.ResponseWriter) error {
	arns := make([]string, 0)
	if err := s.metadata.Scan(clustersBucket, "", func(_, v []byte) error {
		var record clusterRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		arns = append(arns, record.Arn)
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Strings(arns)
	writeJSON(w, http.StatusOK, map[string]any{"clusterArns": arns})
	return nil
}

func (s *Service) registerTaskDefinition(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		ContainerDefinitions []map[string]any `json:"containerDefinitions"`
		Cpu                  string           `json:"cpu"`
		ExecutionRoleArn     string           `json:"executionRoleArn"`
		Family               string           `json:"family"`
		Memory               string           `json:"memory"`
		NetworkMode          string           `json:"networkMode"`
		RequiresCompat       []string         `json:"requiresCompatibilities"`
		TaskRoleArn          string           `json:"taskRoleArn"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	if input.Family == "" || len(input.ContainerDefinitions) == 0 {
		return validation("family and containerDefinitions are required")
	}
	revision, err := s.nextRevision(input.Family)
	if err != nil {
		return err
	}
	record := taskDefinitionRecord{
		Arn:                  taskDefinitionARN(input.Family, revision),
		Compatibilities:      []string{"EC2", "FARGATE"},
		ContainerDefinitions: input.ContainerDefinitions,
		Cpu:                  input.Cpu,
		ExecutionRoleArn:     input.ExecutionRoleArn,
		Family:               input.Family,
		Memory:               input.Memory,
		NetworkMode:          input.NetworkMode,
		RequiresCompat:       append([]string(nil), input.RequiresCompat...),
		Revision:             revision,
		TaskRoleArn:          input.TaskRoleArn,
	}
	if err := s.putRecord(taskDefsBucket, taskDefKey(input.Family, revision), record); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"taskDefinition": taskDefinitionResponse(record)})
	return nil
}

func (s *Service) describeTaskDefinition(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		TaskDefinition string `json:"taskDefinition"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	record, err := s.resolveTaskDefinition(input.TaskDefinition)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"taskDefinition": taskDefinitionResponse(record)})
	return nil
}

func (s *Service) createService(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Cluster        string           `json:"cluster"`
		DesiredCount   int              `json:"desiredCount"`
		LoadBalancers  []map[string]any `json:"loadBalancers"`
		ServiceName    string           `json:"serviceName"`
		TaskDefinition string           `json:"taskDefinition"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	cluster, err := s.resolveCluster(input.Cluster)
	if err != nil {
		return err
	}
	taskDef, err := s.resolveTaskDefinition(input.TaskDefinition)
	if err != nil {
		return err
	}
	if input.ServiceName == "" {
		return validation("serviceName is required")
	}
	record := serviceRecord{
		Arn:            serviceARN(cluster.Name, input.ServiceName),
		ClusterArn:     cluster.Arn,
		CreatedAt:      s.now().UTC(),
		DesiredCount:   input.DesiredCount,
		LoadBalancers:  input.LoadBalancers,
		Name:           input.ServiceName,
		Status:         "ACTIVE",
		TaskDefinition: taskDef.Arn,
	}
	if err := s.putRecord(servicesBucket, serviceKey(cluster.Name, input.ServiceName), record); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"service": serviceResponse(record)})
	return nil
}

func (s *Service) describeServices(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Cluster  string   `json:"cluster"`
		Services []string `json:"services"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	cluster, err := s.resolveCluster(input.Cluster)
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0, len(input.Services))
	for _, name := range input.Services {
		record, err := s.loadService(cluster.Name, name)
		if err != nil {
			return err
		}
		items = append(items, serviceResponse(record))
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": items, "failures": []any{}})
	return nil
}

func (s *Service) listServices(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Cluster string `json:"cluster"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	cluster, err := s.resolveCluster(input.Cluster)
	if err != nil {
		return err
	}
	arns := make([]string, 0)
	if err := s.metadata.Scan(servicesBucket, cluster.Name+"|", func(_, v []byte) error {
		var record serviceRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		arns = append(arns, record.Arn)
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Strings(arns)
	writeJSON(w, http.StatusOK, map[string]any{"serviceArns": arns})
	return nil
}

func (s *Service) runTask(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Cluster        string `json:"cluster"`
		LaunchType     string `json:"launchType"`
		TaskDefinition string `json:"taskDefinition"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	cluster, err := s.resolveCluster(input.Cluster)
	if err != nil {
		return err
	}
	taskDef, err := s.resolveTaskDefinition(input.TaskDefinition)
	if err != nil {
		return err
	}
	launchType := input.LaunchType
	if launchType == "" {
		launchType = "FARGATE"
	}
	record := taskRecord{
		Arn:            taskARN(cluster.Name),
		ClusterArn:     cluster.Arn,
		CreatedAt:      s.now().UTC(),
		LastStatus:     "RUNNING",
		LaunchType:     launchType,
		TaskDefinition: taskDef.Arn,
	}
	if err := s.putRecord(tasksBucket, record.Arn, record); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": []map[string]any{taskResponse(record)}, "failures": []any{}})
	return nil
}

func (s *Service) deleteService(w http.ResponseWriter, r *http.Request) error {
	var input struct {
		Cluster string `json:"cluster"`
		Service string `json:"service"`
	}
	if err := decodeJSON(r, &input); err != nil {
		return err
	}
	cluster, err := s.resolveCluster(input.Cluster)
	if err != nil {
		return err
	}
	record, err := s.loadService(cluster.Name, input.Service)
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(servicesBucket, serviceKey(cluster.Name, input.Service)); err != nil {
		return internal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"service": serviceResponse(record)})
	return nil
}

func (s *Service) loadCluster(name string) (clusterRecord, error) {
	raw, err := s.metadata.Get(clustersBucket, name)
	if err != nil {
		return clusterRecord{}, internal(err)
	}
	if raw == nil {
		return clusterRecord{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ClusterNotFoundException", Message: "cluster not found"}
	}
	var record clusterRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return clusterRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) resolveCluster(name string) (clusterRecord, error) {
	if name == "" {
		name = "default"
	}
	if strings.HasPrefix(name, "arn:aws:ecs:") {
		parts := strings.Split(name, "/")
		name = parts[len(parts)-1]
	}
	return s.loadCluster(name)
}

func (s *Service) nextRevision(family string) (int, error) {
	maxRevision := 0
	if err := s.metadata.Scan(taskDefsBucket, family+"|", func(k, _ []byte) error {
		 parts := strings.Split(string(k), "|")
		 if len(parts) == 2 {
		 	if rev := atoi(parts[1]); rev > maxRevision {
		 		maxRevision = rev
		 	}
		 }
		return nil
	}); err != nil {
		return 0, internal(err)
	}
	return maxRevision + 1, nil
}

func (s *Service) resolveTaskDefinition(name string) (taskDefinitionRecord, error) {
	if name == "" {
		return taskDefinitionRecord{}, validation("taskDefinition is required")
	}
	if strings.HasPrefix(name, "arn:aws:ecs:") {
		parts := strings.Split(name, "/")
		name = parts[len(parts)-1]
	}
	family, revision, found := strings.Cut(name, ":")
	if !found {
		latest, err := s.nextRevision(family)
		if err != nil {
			return taskDefinitionRecord{}, err
		}
		revision = fmt.Sprintf("%d", latest-1)
	}
	raw, err := s.metadata.Get(taskDefsBucket, taskDefKey(family, atoi(revision)))
	if err != nil {
		return taskDefinitionRecord{}, internal(err)
	}
	if raw == nil {
		return taskDefinitionRecord{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ClientException", Message: "task definition not found"}
	}
	var record taskDefinitionRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return taskDefinitionRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) loadService(clusterName, serviceName string) (serviceRecord, error) {
	raw, err := s.metadata.Get(servicesBucket, serviceKey(clusterName, serviceName))
	if err != nil {
		return serviceRecord{}, internal(err)
	}
	if raw == nil {
		return serviceRecord{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ServiceNotFoundException", Message: "service not found"}
	}
	var record serviceRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return serviceRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) putRecord(bucket, key string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(bucket, key, raw); err != nil {
		return internal(err)
	}
	return nil
}

func clusterResponse(record clusterRecord) map[string]any {
	return map[string]any{
		"clusterArn": record.Arn,
		"clusterName": record.Name,
		"registeredContainerInstancesCount": 0,
		"runningTasksCount": 0,
		"status": record.Status,
	}
}

func taskDefinitionResponse(record taskDefinitionRecord) map[string]any {
	return map[string]any{
		"compatibilities":       record.Compatibilities,
		"containerDefinitions":  record.ContainerDefinitions,
		"cpu":                   record.Cpu,
		"executionRoleArn":      record.ExecutionRoleArn,
		"family":                record.Family,
		"memory":                record.Memory,
		"networkMode":           record.NetworkMode,
		"requiresCompatibilities": record.RequiresCompat,
		"revision":              record.Revision,
		"taskDefinitionArn":     record.Arn,
		"taskRoleArn":           record.TaskRoleArn,
	}
}

func serviceResponse(record serviceRecord) map[string]any {
	return map[string]any{
		"clusterArn":     record.ClusterArn,
		"createdAt":      record.CreatedAt.Format(time.RFC3339),
		"desiredCount":   record.DesiredCount,
		"loadBalancers":  record.LoadBalancers,
		"serviceArn":     record.Arn,
		"serviceName":    record.Name,
		"status":         record.Status,
		"taskDefinition": record.TaskDefinition,
	}
}

func taskResponse(record taskRecord) map[string]any {
	return map[string]any{
		"clusterArn":         record.ClusterArn,
		"containers":         []any{},
		"lastStatus":         record.LastStatus,
		"launchType":         record.LaunchType,
		"taskArn":            record.Arn,
		"taskDefinitionArn":  record.TaskDefinition,
	}
}

func clusterARN(name string) string {
	return "arn:aws:ecs:" + region + ":" + accountID + ":cluster/" + name
}

func taskDefinitionARN(family string, revision int) string {
	return "arn:aws:ecs:" + region + ":" + accountID + ":task-definition/" + family + ":" + fmt.Sprintf("%d", revision)
}

func serviceARN(clusterName, serviceName string) string {
	return "arn:aws:ecs:" + region + ":" + accountID + ":service/" + clusterName + "/" + serviceName
}

func taskARN(clusterName string) string {
	return "arn:aws:ecs:" + region + ":" + accountID + ":task/" + clusterName + "/" + uuid.NewString()
}

func taskDefKey(family string, revision int) string {
	return family + "|" + fmt.Sprintf("%d", revision)
}

func serviceKey(clusterName, serviceName string) string {
	return clusterName + "|" + serviceName
}

func atoi(s string) int {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

func validation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ClientException", Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "ServerException", Message: err.Error()}
}

func decodeJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return validation("request body is not valid JSON")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
