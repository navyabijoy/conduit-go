package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"conduit/sdk"
)

// schemaProbe is the request whose response shape is sampled for drift.
type schemaProbe struct {
	endpoint string
	req      sdk.Request
}

// probes picks the request sampled per connector; connectors without one get no
// baseline and are not scanned.
var probes = map[string]schemaProbe{
	// TODO: Probe Slack with a read-only endpoint before real providers are supported;
	// against real Slack this posts a message on every scan.
	"slack":  {endpoint: "send_message", req: sdk.Request{Body: []byte(`{"channel":"C_PROBE","text":"schema probe"}`)}},
	"github": {endpoint: "list_issues", req: sdk.Request{Params: map[string]string{"owner": "probe_owner", "repo": "probe_repo"}}},
	"stripe": {endpoint: "list_charges", req: sdk.Request{Params: map[string]string{"limit": "3"}}},
}

// runProbe returns the probe response body, or nil if conn has no such endpoint.
func runProbe(ctx context.Context, conn sdk.Connector, client *sdk.HTTPClient, p schemaProbe) ([]byte, error) {
	for _, ep := range conn.Endpoints() {
		if ep.Name == p.endpoint {
			resp, err := ep.Execute(ctx, client, p.req)
			if err != nil {
				return nil, err
			}
			return resp.Body, nil
		}
	}
	return nil, nil
}

// DriftDetector rescans instances and marks one "drifted" when a baseline field vanishes
// or changes type. It alerts CONDUIT_ALERT_WEBHOOK_URL if set.
type DriftDetector struct {
	db       *DB
	registry map[string]sdk.Connector
	mockHost string
	stopChan chan struct{}
}

// NewDriftDetector returns a detector that probes connectors through mockHost.
func NewDriftDetector(db *DB, registry map[string]sdk.Connector, mockHost string) *DriftDetector {
	return &DriftDetector{
		db:       db,
		registry: registry,
		mockHost: mockHost,
		stopChan: make(chan struct{}),
	}
}

// Start scans every interval in the background; the first scan runs after one interval.
func (d *DriftDetector) Start(interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		for {
			select {
			case <-ticker.C:
				d.checkDrift()
			case <-d.stopChan:
				ticker.Stop()
				return
			}
		}
	}()
}

// Stop ends scanning. Call it at most once.
func (d *DriftDetector) Stop() {
	close(d.stopChan)
}

// maxConcurrentScans keeps one slow provider from holding up the whole scan.
const maxConcurrentScans = 4

func (d *DriftDetector) checkDrift() {
	instances, err := d.db.ListInstances()
	if err != nil {
		slog.Error("drift scan: list instances failed", "component", "drift", "err", err)
		return
	}

	var wg sync.WaitGroup
	slots := make(chan struct{}, maxConcurrentScans)
	for _, inst := range instances {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			d.scanInstance(inst)
		}()
	}
	wg.Wait()
}

func (d *DriftDetector) scanInstance(inst *ConnectorInstance) {
	if inst.BaselineSchema == "" {
		return
	}
	conn, ok := d.registry[inst.ConnectorID]
	if !ok {
		return
	}

	log := slog.With("component", "drift", "instance_id", inst.ID, "connector", inst.ConnectorID)
	log.Info("scanning for schema drift")

	var baselineSchema map[string]string
	if err := json.Unmarshal([]byte(inst.BaselineSchema), &baselineSchema); err != nil {
		log.Error("parse baseline schema failed", "err", err)
		return
	}

	creds, _, err := d.db.Credentials(inst.ID)
	if err != nil {
		log.Error("load credentials failed", "err", err)
		return
	}

	// Scans never refresh tokens: an expired token fails the probe until a user request
	// refreshes it.
	client := sdk.NewHTTPClient(
		"http://"+d.mockHost,
		*creds,
		conn.AuthConfig(),
		func(ctx context.Context, token *sdk.Token) (*sdk.Token, error) {
			return nil, fmt.Errorf("token refresh not supported during drift detection")
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	bodyBytes, err := runProbe(ctx, conn, client, probes[inst.ConnectorID])
	cancel()
	if err != nil {
		log.Warn("drift probe failed", "err", err)
	}

	// A failed probe is an availability or auth problem, not a shape change.
	if len(bodyBytes) == 0 {
		return
	}

	var liveObj interface{}
	if err := json.Unmarshal(bodyBytes, &liveObj); err != nil {
		log.Error("parse live response failed", "err", err)
		return
	}
	liveSchema := make(map[string]string)
	flattenJSONSchema("", liveObj, liveSchema)

	if driftDetails := schemaDiff(baselineSchema, liveSchema); len(driftDetails) > 0 {
		log.Warn("schema drift detected", "details", strings.Join(driftDetails, "; "))
		recordDriftDetection(inst.ConnectorID, "drifted")
		// Alert once when the instance first drifts, not on every scan.
		if inst.Status != "drifted" {
			inst.Status = "drifted"
			found, err := d.db.SetInstanceStatus(inst.ID, inst.Status)
			if err != nil {
				log.Error("save status failed", "err", err)
			}
			// No alert for an instance deleted while it was being probed.
			if found {
				d.triggerAlert(inst, driftDetails)
			}
		}
		return
	}

	if inst.Status == "drifted" {
		log.Info("instance returned to healthy status")
		inst.Status = "active"
		if _, err := d.db.SetInstanceStatus(inst.ID, inst.Status); err != nil {
			log.Error("save status failed", "err", err)
		}
	}
	recordDriftDetection(inst.ConnectorID, "healthy")
}

// schemaDiff lists baseline fields that vanished or changed type. New live fields are not
// drift, and an empty array says nothing about its items, so it never counts.
func schemaDiff(baseline, live map[string]string) []string {
	var emptyArrays []string
	for path, typ := range live {
		if typ == "array[empty]" {
			emptyArrays = append(emptyArrays, path)
		}
	}

	var details []string
	for key, expectedType := range baseline {
		actualType, exists := live[key]
		switch {
		case !exists:
			if hiddenByEmptyArray(key, emptyArrays) || (expectedType == "array[empty]" && hasItemFields(key, live)) {
				continue
			}
			details = append(details, fmt.Sprintf("Field %q removed", key))
		case actualType != expectedType:
			if expectedType == "array[empty]" || actualType == "array[empty]" {
				continue
			}
			details = append(details, fmt.Sprintf("Field %q type changed from %s to %s", key, expectedType, actualType))
		}
	}
	return details
}

// hiddenByEmptyArray reports whether key sits under a live empty array, so its absence
// just means there was nothing to sample.
func hiddenByEmptyArray(key string, emptyArrays []string) bool {
	for _, path := range emptyArrays {
		if strings.HasPrefix(key, path+".[]") {
			return true
		}
	}
	return false
}

// hasItemFields reports whether live has fields under the array at path; arrays of
// objects are recorded by their item fields, not by the array itself.
func hasItemFields(path string, live map[string]string) bool {
	for key := range live {
		if strings.HasPrefix(key, path+".[]") {
			return true
		}
	}
	return false
}

// alertClient has a timeout so a hung alert endpoint cannot hold a goroutine forever.
var alertClient = &http.Client{Timeout: 10 * time.Second}

// triggerAlert logs the drift and, if CONDUIT_ALERT_WEBHOOK_URL is set, POSTs an alert
// without waiting for the response.
func (d *DriftDetector) triggerAlert(inst *ConnectorInstance, details []string) {
	slog.Warn("schema drift alert", "component", "drift", "instance_id", inst.ID, "connector", inst.ConnectorID, "issues", details)

	alertURL := os.Getenv("CONDUIT_ALERT_WEBHOOK_URL")
	if alertURL == "" {
		return
	}

	payload, err := json.Marshal(map[string]any{
		"event":        "schema_drift",
		"connector_id": inst.ConnectorID,
		"instance_id":  inst.ID,
		"details":      details,
		"timestamp":    time.Now().Format(time.RFC3339),
	})
	if err != nil {
		slog.Error("encode alert failed", "component", "drift", "instance_id", inst.ID, "err", err)
		return
	}

	go func() {
		resp, err := alertClient.Post(alertURL, "application/json", bytes.NewReader(payload))
		if err != nil {
			slog.Error("send alert webhook failed", "component", "drift", "instance_id", inst.ID, "err", err)
			return
		}
		resp.Body.Close()
	}()
}

// flattenJSONSchema records the JSON type of every leaf under its dot path. Arrays are
// sampled by their first element.
func flattenJSONSchema(path string, val interface{}, schema map[string]string) {
	prefix := ""
	if path != "" {
		prefix = path + "."
	}

	switch v := val.(type) {
	case map[string]interface{}:
		for k, item := range v {
			flattenJSONSchema(prefix+k, item, schema)
		}
	case []interface{}:
		if len(v) == 0 {
			schema[path] = "array[empty]"
			return
		}
		switch f := v[0].(type) {
		case map[string]interface{}:
			flattenJSONSchema(path+".[]", f, schema)
		default:
			schema[path] = "array[" + jsonTypeName(f) + "]"
		}
	default:
		schema[path] = jsonTypeName(v)
	}
}

func jsonTypeName(v interface{}) string {
	if v == nil {
		return "null"
	}
	switch v.(type) {
	case string:
		return "string"
	case float64, int, int64, float32:
		return "number"
	case bool:
		return "boolean"
	default:
		return "unknown"
	}
}
