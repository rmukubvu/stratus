package operator

import (
	"encoding/json"
	"html/template"
)

func templateJSON(value any) template.JS {
	raw, err := json.Marshal(value)
	if err != nil {
		return template.JS("null")
	}
	return template.JS(raw)
}
