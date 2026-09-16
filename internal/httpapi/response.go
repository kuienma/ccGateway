package httpapi

import (
	"encoding/json"
	"net/http"
)

const contentTypeJSON = "application/json"

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
