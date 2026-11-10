package route53

import (
	"net/http"

	"github.com/stratus/internal/control"
)

// Router is where the service registers its control endpoints;
// *control.Handler fits.
type Router interface {
	HandleFunc(pattern string, fn http.HandlerFunc)
}

// RegisterRoutes exposes DNS lookups on the control API:
//
//	GET /_stratus/control/v1/dns/resolve?name=api.example.com&type=A
//
// The answer comes from the emulated hosted zones, including the zones and
// records Cloud Map manages, so a test can check what a client would resolve.
func RegisterRoutes(r Router, svc *Service) {
	r.HandleFunc("GET "+control.PathPrefix+"/dns/resolve", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		if q.Get("name") == "" {
			control.WriteError(w, &control.Error{Kind: control.KindInvalid, Code: "InvalidRequest", Message: "name is required", Hint: "pass ?name=<dns name>&type=A"})
			return
		}
		res, err := svc.Resolve(q.Get("name"), q.Get("type"))
		if err != nil {
			control.WriteError(w, &control.Error{Kind: control.KindInvalid, Code: "InvalidRequest", Message: err.Error()})
			return
		}
		control.WriteJSON(w, http.StatusOK, res)
	})
}
