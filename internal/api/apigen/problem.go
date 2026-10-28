package apigen

import (
	"encoding/json"
	"net/http"
)

func WriteProblem(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{Code: code, Message: message, RequestId: w.Header().Get("X-Request-ID")})
}

func NotImplemented(w http.ResponseWriter, _ *http.Request, operation string) {
	WriteProblem(w, http.StatusNotImplemented, "not_implemented", operation+" is not implemented yet")
}
