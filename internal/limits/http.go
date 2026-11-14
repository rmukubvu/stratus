package limits

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/stratus/internal/control"
)

// Router is where limits registers its endpoints; *control.Handler fits.
type Router interface {
	HandleFunc(pattern string, fn http.HandlerFunc)
}

// RegisterRoutes exposes the governor on the control API:
//
//	PUT    /_stratus/control/v1/limits   apply a profile (an empty body applies aws-default)
//	GET    /_stratus/control/v1/limits   the active profile, per-rule counts and recent breaches
//	DELETE /_stratus/control/v1/limits   turn limits off and return the final status
func RegisterRoutes(r Router, g *Governor) {
	path := control.PathPrefix + "/limits"
	r.HandleFunc("PUT "+path, func(w http.ResponseWriter, req *http.Request) {
		var spec Spec
		if err := control.DecodeOptionalJSON(req, &spec); err != nil {
			control.WriteError(w, err)
			return
		}
		status, err := g.Apply(spec)
		if err != nil {
			control.WriteError(w, err)
			return
		}
		control.WriteJSON(w, http.StatusOK, status)
	})
	r.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
		status, err := g.Status()
		if err != nil {
			control.WriteError(w, err)
			return
		}
		control.WriteJSON(w, http.StatusOK, status)
	})
	r.HandleFunc("DELETE "+path, func(w http.ResponseWriter, _ *http.Request) {
		status, err := g.Clear()
		if err != nil {
			control.WriteError(w, err)
			return
		}
		control.WriteJSON(w, http.StatusOK, status)
	})
}

// ParseSetting reads the STRATUS_LIMITS or --limits setting: empty means
// off, a base profile name applies that base, anything else is a path to a
// JSON profile. ok is false when limits stay off.
func ParseSetting(value string, readFile func(string) ([]byte, error)) (spec Spec, ok bool, err error) {
	value = strings.TrimSpace(value)
	switch strings.ToLower(value) {
	case "", "off":
		return Spec{}, false, nil
	case BaseAWSDefault, BaseStrict:
		return Spec{Profile: strings.ToLower(value)}, true, nil
	}
	raw, err := readFile(value)
	if err != nil {
		return Spec{}, false, fmt.Errorf("read limits profile: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return Spec{}, false, fmt.Errorf("limits profile %s is not valid: %w", value, err)
	}
	return spec, true, nil
}
