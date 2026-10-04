package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// The demo keeps all data in memory. A production service would persist identity
// state, idempotency records, audit events, and connector jobs transactionally.
type App struct {
	mu          sync.Mutex
	identities  map[string]*Identity
	connectors  map[string]*Connector
	events      map[string]*LifecycleEvent
	delegations map[string]*Delegation
	sequence    int
}

type Identity struct {
	ID                string              `json:"id"`
	Name              string              `json:"name"`
	Role              string              `json:"role"`
	DesiredRole       string              `json:"desiredRole"`
	Version           int                 `json:"version"`
	EffectiveBySystem map[string][]string `json:"effectiveBySystem"`
}

type Connector struct {
	Name       string              `json:"name"`
	GrantsByID map[string][]string `json:"grantsByIdentity"`
	Fail       bool                `json:"simulatedFailure"`
}

type LifecycleEvent struct {
	ID          string            `json:"id"`
	IdentityID  string            `json:"identityId"`
	FromRole    string            `json:"fromRole"`
	ToRole      string            `json:"toRole"`
	Version     int               `json:"version"`
	Status      string            `json:"status"`
	Pending     []string          `json:"pendingSystems"`
	Results     map[string]string `json:"results"`
	CreatedAt   time.Time         `json:"createdAt"`
	CompletedAt *time.Time        `json:"completedAt,omitempty"`
}

type Delegation struct {
	ID         string    `json:"id"`
	IdentityID string    `json:"identityId"`
	AgentID    string    `json:"agentId"`
	Scopes     []string  `json:"scopes"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Revoked    bool      `json:"revoked"`
	Reason     string    `json:"revocationReason,omitempty"`
}

type changeRoleRequest struct {
	EventID string `json:"eventId"`
	Role    string `json:"role"`
}

type delegationRequest struct {
	IdentityID string   `json:"identityId"`
	AgentID    string   `json:"agentId"`
	Scopes     []string `json:"scopes"`
	ExpiresIn  int      `json:"expiresInSeconds"`
}

type failureRequest struct {
	Fail bool `json:"fail"`
}

type response struct {
	Error string `json:"error,omitempty"`
	OK    bool   `json:"ok,omitempty"`
}

var roleGrants = map[string]map[string][]string{
	"support": {
		"tickets": {"tickets.read", "tickets.comment"},
		"finance": {},
	},
	"finance": {
		"tickets": {},
		"finance": {"finance.read", "finance.report"},
	},
}

func newApp() *App {
	return &App{
		identities: map[string]*Identity{
			"ana": {
				ID: "ana", Name: "Ana Pérez", Role: "support", DesiredRole: "support", Version: 1,
				EffectiveBySystem: map[string][]string{
					"tickets": {"tickets.read", "tickets.comment"},
					"finance": {},
				},
			},
		},
		connectors: map[string]*Connector{
			"tickets": {Name: "tickets", GrantsByID: map[string][]string{"ana": {"tickets.read", "tickets.comment"}}},
			"finance": {Name: "finance", GrantsByID: map[string][]string{"ana": {}}},
		},
		events:      make(map[string]*LifecycleEvent),
		delegations: make(map[string]*Delegation),
	}
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /identities/{id}", a.getIdentity)
	mux.HandleFunc("POST /identities/{id}/role", a.changeRole)
	mux.HandleFunc("GET /events/{id}", a.getEvent)
	mux.HandleFunc("POST /events/{id}/retry", a.retryEvent)
	mux.HandleFunc("GET /connectors", a.getConnectors)
	mux.HandleFunc("PUT /demo/connectors/{name}/failure", a.setConnectorFailure)
	mux.HandleFunc("POST /delegations", a.createDelegation)
	mux.HandleFunc("GET /delegations/{id}/authorize", a.authorizeDelegation)
	return requestLog(mux)
}

func (a *App) getIdentity(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id, ok := a.identities[r.PathValue("id")]
	if !ok {
		writeError(w, http.StatusNotFound, "identity not found")
		return
	}
	writeJSON(w, http.StatusOK, id)
}

func (a *App) changeRole(w http.ResponseWriter, r *http.Request) {
	var req changeRoleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.EventID) == "" {
		writeError(w, http.StatusBadRequest, "eventId is required for idempotency")
		return
	}
	if _, ok := roleGrants[req.Role]; !ok {
		writeError(w, http.StatusBadRequest, "role must be support or finance")
		return
	}
	identityID := r.PathValue("id")

	a.mu.Lock()
	if prior, exists := a.events[req.EventID]; exists {
		if prior.IdentityID != identityID || prior.ToRole != req.Role {
			a.mu.Unlock()
			writeError(w, http.StatusConflict, "eventId was already used for a different change")
			return
		}
		copy := cloneEvent(prior)
		a.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"duplicate": true, "event": copy})
		return
	}
	id, ok := a.identities[identityID]
	if !ok {
		a.mu.Unlock()
		writeError(w, http.StatusNotFound, "identity not found")
		return
	}
	if id.Role == req.Role && id.DesiredRole == req.Role {
		a.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"duplicate": false, "message": "identity already has this role"})
		return
	}
	id.DesiredRole = req.Role
	id.Version++
	for _, previous := range a.events {
		if previous.IdentityID == identityID && previous.Status != "completed" && previous.Status != "superseded" {
			previous.Status = "superseded"
			previous.Pending = nil
		}
	}
	// Delegated AI access is invalidated immediately when the subject's role changes.
	for _, d := range a.delegations {
		if d.IdentityID == identityID && !d.Revoked {
			d.Revoked = true
			d.Reason = "identity role changed"
		}
	}
	event := &LifecycleEvent{
		ID: req.EventID, IdentityID: identityID, FromRole: id.Role, ToRole: req.Role,
		Version: id.Version, Status: "pending", Results: make(map[string]string), CreatedAt: time.Now().UTC(),
	}
	a.events[req.EventID] = event
	a.mu.Unlock()

	a.applyPending(req.EventID)

	a.mu.Lock()
	result := cloneEvent(a.events[req.EventID])
	a.mu.Unlock()
	status := http.StatusOK
	if result.Status == "partial" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, result)
}

func (a *App) applyPending(eventID string) {
	a.mu.Lock()
	event, ok := a.events[eventID]
	if !ok {
		a.mu.Unlock()
		return
	}
	identity := a.identities[event.IdentityID]
	if identity == nil {
		a.mu.Unlock()
		return
	}
	if identity.Version != event.Version {
		event.Status = "superseded"
		event.Pending = nil
		a.mu.Unlock()
		return
	}
	// Copy values before releasing the lock; connector calls would be network I/O in production.
	targets := make(map[string][]string, len(a.connectors))
	for name := range a.connectors {
		if event.Results[name] != "applied" {
			targets[name] = append([]string(nil), roleGrants[event.ToRole][name]...)
		}
	}
	a.mu.Unlock()

	for name, desired := range targets {
		a.mu.Lock()
		connector := a.connectors[name]
		event = a.events[eventID]
		identity = a.identities[event.IdentityID]
		if event.Status == "superseded" || identity.Version != event.Version {
			event.Status = "superseded"
			event.Pending = nil
			a.mu.Unlock()
			return
		}
		if connector == nil || connector.Fail {
			event.Results[name] = "failed: connector unavailable"
			event.Pending = addUnique(event.Pending, name)
			a.mu.Unlock()
			continue
		}
		connector.GrantsByID[identity.ID] = desired
		identity.EffectiveBySystem[name] = append([]string(nil), desired...)
		event.Results[name] = "applied"
		event.Pending = remove(event.Pending, name)
		a.mu.Unlock()
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	event = a.events[eventID]
	identity = a.identities[event.IdentityID]
	if identity.Version != event.Version {
		event.Status = "superseded"
		event.Pending = nil
		return
	}
	if len(event.Pending) == 0 {
		event.Status = "completed"
		identity.Role = event.ToRole
		now := time.Now().UTC()
		event.CompletedAt = &now
	} else {
		event.Status = "partial"
	}
}

func (a *App) getEvent(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	event, ok := a.events[r.PathValue("id")]
	if !ok {
		writeError(w, http.StatusNotFound, "event not found")
		return
	}
	writeJSON(w, http.StatusOK, event)
}

func (a *App) retryEvent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.mu.Lock()
	event, ok := a.events[id]
	if !ok {
		a.mu.Unlock()
		writeError(w, http.StatusNotFound, "event not found")
		return
	}
	if event.Status == "completed" {
		copy := cloneEvent(event)
		a.mu.Unlock()
		writeJSON(w, http.StatusOK, copy)
		return
	}
	a.mu.Unlock()

	a.applyPending(id)
	a.mu.Lock()
	result := cloneEvent(a.events[id])
	a.mu.Unlock()
	status := http.StatusOK
	if result.Status != "completed" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, result)
}

func (a *App) getConnectors(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	writeJSON(w, http.StatusOK, a.connectors)
}

func (a *App) setConnectorFailure(w http.ResponseWriter, r *http.Request) {
	var req failureRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	connector, ok := a.connectors[r.PathValue("name")]
	if !ok {
		writeError(w, http.StatusNotFound, "connector not found")
		return
	}
	connector.Fail = req.Fail
	writeJSON(w, http.StatusOK, connector)
}

func (a *App) createDelegation(w http.ResponseWriter, r *http.Request) {
	var req delegationRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.AgentID == "" || len(req.Scopes) == 0 || req.ExpiresIn < 1 || req.ExpiresIn > 3600 {
		writeError(w, http.StatusBadRequest, "agentId, scopes, and expiresInSeconds (1..3600) are required")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	identity, ok := a.identities[req.IdentityID]
	if !ok {
		writeError(w, http.StatusNotFound, "identity not found")
		return
	}
	grants := flatten(identity.EffectiveBySystem)
	for _, scope := range req.Scopes {
		if !slices.Contains(grants, scope) {
			writeError(w, http.StatusForbidden, "delegation scope exceeds the identity's effective access")
			return
		}
	}
	a.sequence++
	id := fmt.Sprintf("delegation-%d", a.sequence)
	d := &Delegation{
		ID: id, IdentityID: req.IdentityID, AgentID: req.AgentID,
		Scopes: unique(req.Scopes), ExpiresAt: time.Now().UTC().Add(time.Duration(req.ExpiresIn) * time.Second),
	}
	a.delegations[id] = d
	writeJSON(w, http.StatusCreated, d)
}

func (a *App) authorizeDelegation(w http.ResponseWriter, r *http.Request) {
	delegationID := r.PathValue("id")
	scope := r.URL.Query().Get("scope")
	a.mu.Lock()
	defer a.mu.Unlock()
	d, ok := a.delegations[delegationID]
	if !ok {
		writeError(w, http.StatusNotFound, "delegation not found")
		return
	}
	identity := a.identities[d.IdentityID]
	if d.Revoked {
		writeError(w, http.StatusForbidden, "delegation revoked: "+d.Reason)
		return
	}
	if time.Now().After(d.ExpiresAt) {
		writeError(w, http.StatusForbidden, "delegation expired")
		return
	}
	if !slices.Contains(d.Scopes, scope) || !slices.Contains(flatten(identity.EffectiveBySystem), scope) {
		writeError(w, http.StatusForbidden, "scope is not currently authorized")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authorized": true, "identityId": d.IdentityID, "agentId": d.AgentID, "scope": scope})
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, response{Error: message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("method=%s path=%s duration_ms=%d", r.Method, r.URL.Path, time.Since(started).Milliseconds())
	})
}

func flatten(bySystem map[string][]string) []string {
	var out []string
	for _, grants := range bySystem {
		out = append(out, grants...)
	}
	return unique(out)
}

func unique(values []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	slices.Sort(out)
	return out
}

func addUnique(values []string, item string) []string {
	if slices.Contains(values, item) {
		return values
	}
	return append(values, item)
}

func remove(values []string, item string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != item {
			out = append(out, value)
		}
	}
	return out
}

func cloneEvent(event *LifecycleEvent) LifecycleEvent {
	copy := *event
	copy.Pending = append([]string(nil), event.Pending...)
	copy.Results = make(map[string]string, len(event.Results))
	for key, value := range event.Results {
		copy.Results[key] = value
	}
	return copy
}

func main() {
	app := newApp()
	server := &http.Server{
		Addr:              ":8080",
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("IAM lifecycle demo listening on http://localhost%s", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
