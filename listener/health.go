package listener

import "net/http"

// HealthRoutes registers the liveness and readiness probes on mux, for a Kubernetes
// Deployment's httpGet probes or a Docker HEALTHCHECK.
//
// Both answer from local state alone. Nothing remote is contacted: sessions are opened on
// demand and each reaches a target the caller chose, so there is no single upstream whose
// health would mean anything, and an unreachable bastion must not pull the pod out of its
// Service while every other target still works. An administratively disabled server
// (ssh.disable_flag_file) also stays ready on purpose — it is up and answering every call
// with the reason, which is exactly what a caller needs to reach.
//
// These routes sit outside the auth middleware, which wraps /mcp alone: a probe carries no
// token. That is why the payloads say nothing about the environment.
//
// The method patterns make a non-GET a 405 rather than a 200; GET also matches HEAD.
func HealthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", respondJSON(`{"status":"healthy"}`))
	mux.HandleFunc("GET /readyz", respondJSON(`{"status":"ready"}`))
}

func respondJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}
