package httpapi

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
)

func WriteXML(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(payload)
}

func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
