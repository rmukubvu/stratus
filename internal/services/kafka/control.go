package kafka

import (
	"net/http"

	"github.com/stratus/internal/control"
)

// Router is where the service registers its control endpoints;
// *control.Handler fits.
type Router interface {
	HandleFunc(pattern string, fn http.HandlerFunc)
}

// ClusterView is a cluster as the control API shows it: everything a test or
// an agent needs to connect, without looking up ARNs first.
type ClusterView struct {
	Name             string `json:"name"`
	ARN              string `json:"arn"`
	Type             string `json:"type"`
	State            string `json:"state"`
	KafkaVersion     string `json:"kafka_version"`
	BootstrapBrokers string `json:"bootstrap_brokers"`
}

// Clusters lists every cluster with its bootstrap address, sorted by name.
func (s *Service) Clusters() ([]ClusterView, error) {
	clusters, err := s.List("", "")
	if err != nil {
		return nil, err
	}
	out := make([]ClusterView, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, ClusterView{
			Name:             c.Name,
			ARN:              c.ARN,
			Type:             c.Type,
			State:            c.State,
			KafkaVersion:     c.KafkaVersion(),
			BootstrapBrokers: c.BootstrapBrokers(),
		})
	}
	return out, nil
}

// RegisterRoutes exposes the clusters on the control API:
//
//	GET /_stratus/control/v1/kafka/clusters   every cluster with its bootstrap brokers
func RegisterRoutes(r Router, svc *Service) {
	r.HandleFunc("GET "+control.PathPrefix+"/kafka/clusters", func(w http.ResponseWriter, _ *http.Request) {
		items, err := svc.Clusters()
		if err != nil {
			control.WriteError(w, err)
			return
		}
		control.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
	})
}
