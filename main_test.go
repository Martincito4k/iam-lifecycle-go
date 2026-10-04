package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func call(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	return res
}

func TestRoleChangeIsIdempotent(t *testing.T) {
	app := newApp()
	handler := app.routes()
	body := `{"eventId":"role-change-1","role":"finance"}`

	first := call(t, handler, http.MethodPost, "/identities/ana/role", body)
	if first.Code != http.StatusOK {
		t.Fatalf("first change status = %d, body=%s", first.Code, first.Body)
	}
	second := call(t, handler, http.MethodPost, "/identities/ana/role", body)
	if second.Code != http.StatusOK {
		t.Fatalf("duplicate change status = %d", second.Code)
	}
	var duplicate struct {
		Duplicate bool `json:"duplicate"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &duplicate); err != nil {
		t.Fatal(err)
	}
	if !duplicate.Duplicate {
		t.Fatal("second request was not identified as a duplicate")
	}
	if got := app.connectors["tickets"].GrantsByID["ana"]; len(got) != 0 {
		t.Fatalf("tickets grants should have been removed, got %v", got)
	}
	if got := app.connectors["finance"].GrantsByID["ana"]; len(got) != 2 {
		t.Fatalf("finance grants should have been added, got %v", got)
	}
}

func TestFailedConnectorCanBeRetried(t *testing.T) {
	app := newApp()
	app.connectors["tickets"].Fail = true
	handler := app.routes()
	change := call(t, handler, http.MethodPost, "/identities/ana/role", `{"eventId":"role-change-2","role":"finance"}`)
	if change.Code != http.StatusAccepted {
		t.Fatalf("partial change status = %d, body=%s", change.Code, change.Body)
	}
	if got := app.connectors["tickets"].GrantsByID["ana"]; len(got) == 0 {
		t.Fatal("tickets connector failure should leave previous grants in place")
	}
	app.connectors["tickets"].Fail = false
	retry := call(t, handler, http.MethodPost, "/events/role-change-2/retry", "")
	if retry.Code != http.StatusOK {
		t.Fatalf("retry status = %d, body=%s", retry.Code, retry.Body)
	}
	if got := app.connectors["tickets"].GrantsByID["ana"]; len(got) != 0 {
		t.Fatalf("retry should revoke tickets grants, got %v", got)
	}
	if app.identities["ana"].Role != "finance" {
		t.Fatalf("role = %q, want finance", app.identities["ana"].Role)
	}
}

func TestRoleChangeRevokesAIAgentDelegation(t *testing.T) {
	app := newApp()
	handler := app.routes()
	created := call(t, handler, http.MethodPost, "/delegations", `{"identityId":"ana","agentId":"support-helper","scopes":["tickets.read"],"expiresInSeconds":300}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("delegation status = %d, body=%s", created.Code, created.Body)
	}
	var delegation Delegation
	if err := json.Unmarshal(created.Body.Bytes(), &delegation); err != nil {
		t.Fatal(err)
	}
	change := call(t, handler, http.MethodPost, "/identities/ana/role", `{"eventId":"role-change-3","role":"finance"}`)
	if change.Code != http.StatusOK {
		t.Fatalf("role change status = %d", change.Code)
	}
	authorized := call(t, handler, http.MethodGet, "/delegations/"+delegation.ID+"/authorize?scope=tickets.read", "")
	if authorized.Code != http.StatusForbidden {
		t.Fatalf("revoked delegation status = %d, want 403", authorized.Code)
	}
}

func TestOlderPendingChangeCannotOverrideNewerRole(t *testing.T) {
	app := newApp()
	app.connectors["tickets"].Fail = true
	handler := app.routes()
	first := call(t, handler, http.MethodPost, "/identities/ana/role", `{"eventId":"old-change","role":"finance"}`)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first change status = %d", first.Code)
	}
	app.connectors["tickets"].Fail = false
	second := call(t, handler, http.MethodPost, "/identities/ana/role", `{"eventId":"new-change","role":"support"}`)
	if second.Code != http.StatusOK {
		t.Fatalf("second change status = %d, body=%s", second.Code, second.Body)
	}
	call(t, handler, http.MethodPost, "/events/old-change/retry", "")
	if got := app.connectors["tickets"].GrantsByID["ana"]; len(got) != 2 {
		t.Fatalf("stale retry overwrote current tickets grants: %v", got)
	}
	if got := app.connectors["finance"].GrantsByID["ana"]; len(got) != 0 {
		t.Fatalf("stale retry left finance grants behind: %v", got)
	}
}
