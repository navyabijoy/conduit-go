package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"conduit/sdk"
)

// handleExecuteEndpoint passes the provider's status and body straight through to the caller.
func (s *Server) handleExecuteEndpoint(w http.ResponseWriter, r *http.Request, instanceID, endpointName string) {
	inst, err := s.db.Instance(instanceID)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "Connector instance not found")
		return
	}

	conn, ok := s.registry[inst.ConnectorID]
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "Connector registry entry missing")
		return
	}

	var targetEP *sdk.EndpointDefinition
	for _, ep := range conn.Endpoints() {
		if ep.Name == endpointName {
			targetEP = &ep
			break
		}
	}
	if targetEP == nil {
		writeJSONError(w, http.StatusNotFound, "Endpoint not found: "+endpointName)
		return
	}

	creds, _, err := s.db.Credentials(instanceID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Failed to retrieve credentials: "+err.Error())
		return
	}

	bodyBytes, status, err := readBody(w, r)
	if err != nil {
		writeJSONError(w, status, "Failed to read request body")
		return
	}

	params := make(map[string]string)
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			params[k] = v[0]
		}
	}

	client := sdk.NewHTTPClient(
		"http://"+s.mockHost,
		*creds,
		conn.AuthConfig(),
		s.tokenRefreshHook(instanceID, conn),
	)

	req := sdk.Request{
		Body:   bodyBytes,
		Params: params,
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	slog.Info("executing endpoint", "component", "gateway", "instance_id", instanceID, "connector", inst.ConnectorID, "endpoint", endpointName)

	start := time.Now()
	resp, err := targetEP.Execute(ctx, client, req)
	duration := time.Since(start)

	if err != nil {
		errType := "permanent"
		if sdk.IsTransient(err) {
			errType = "transient"
		}
		recordRequestError(inst.ConnectorID, endpointName, errType)
		recordRequestDuration(inst.ConnectorID, endpointName, "error", duration.Seconds())

		slog.Error("endpoint execution failed", "component", "gateway", "instance_id", instanceID, "connector", inst.ConnectorID, "endpoint", endpointName, "err", err)
		writeJSONError(w, http.StatusBadGateway, "Execution failed: "+err.Error())
		return
	}

	recordRequestDuration(inst.ConnectorID, endpointName, "success", duration.Seconds())

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(resp.Body)
}
