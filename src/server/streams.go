package server

import (
	"encoding/json"
	"net/http"
)

// maxBodyBytes caps every JSON request body this API accepts.
const maxBodyBytes = 64 << 10

// decodeBody reads a size-capped JSON body into DST, writing the 400 itself.
// ok=false means the response has already been sent.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
