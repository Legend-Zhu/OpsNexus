package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/audit"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/monitor"
)

// API exposes the orchestrator over HTTP. Routes are registered with a stdlib
// *http.ServeMux (Go 1.22+ method+pattern matching) in Routes().
type API struct {
	orch   *Orchestrator
	events func(service string, typ monitor.EventType, limit int) []monitor.Event
	audit  *audit.Store
}

// NewAPI wraps an Orchestrator with HTTP handlers.
func NewAPI(orch *Orchestrator) *API {
	return &API{orch: orch}
}

// SetEvents wires the P2 event query function (typically monitor.Manager.Events).
func (a *API) SetEvents(fn func(service string, typ monitor.EventType, limit int) []monitor.Event) {
	a.events = fn
}

// SetAudit wires the audit store (nil disables the endpoint's data).
func (a *API) SetAudit(s *audit.Store) {
	a.audit = s
}

// Routes returns the HTTP routes served by this API. The caller registers them
// on its own *http.ServeMux (so main can coexist with other handlers).
func (a *API) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"POST /api/v1/services":               a.deploy,
		"GET /api/v1/services":                a.list,
		"GET /api/v1/services/{name}":         a.inspect,
		"POST /api/v1/services/{name}":        a.update,
		"DELETE /api/v1/services/{name}":      a.remove,
		"POST /api/v1/services/{name}/scale":  a.scale,
		"POST /api/v1/services/{name}/restart": a.restart,
		"GET /api/v1/operations":              a.listOps,
		"GET /api/v1/operations/{id}":         a.getOp,
		"GET /api/v1/events":                  a.listEvents,
		"GET /api/v1/self":                    a.self,
		"GET /api/v1/audit":                   a.listAudit,
		"GET /api/v1/nodes":                   a.nodes,
		"GET /api/v1/nodes/{id}/processes":    a.nodeProcesses,
	}
}

// ---- handlers ----

// maybeProxyWrite forwards a write request to the leader when this instance
// is a non-leader manager (the leader owns control-plane writes). Returns
// (proxied, status, body) — when proxied, the caller must not continue.
func (a *API) maybeProxyWrite(w http.ResponseWriter, r *http.Request, path string) bool {
	leader, err := a.orch.AmILeader(r.Context())
	if err != nil || leader {
		return false // we are the leader (or unknown): handle locally
	}
	body, _ := io.ReadAll(r.Body)
	code, resp, err := a.orch.ProxyWriteToLeader(r.Context(), r.Method, path, body)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return true
	}
	w.WriteHeader(code)
	_, _ = w.Write(resp)
	return true
}

func (a *API) deploy(w http.ResponseWriter, r *http.Request) {
	if a.maybeProxyWrite(w, r, "/api/v1/services") {
		return
	}
	cfg, ok := decodeConfig(w, r)
	if !ok {
		return
	}
	op, err := a.orch.Deploy(r.Context(), cfg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, op.snapshot())
}

func (a *API) update(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if a.maybeProxyWrite(w, r, "/api/v1/services/"+name) {
		return
	}
	cfg, ok := decodeConfig(w, r)
	if !ok {
		return
	}
	op, err := a.orch.Update(r.Context(), name, cfg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, op.snapshot())
}

func (a *API) scale(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if a.maybeProxyWrite(w, r, "/api/v1/services/"+name+"/scale") {
		return
	}
	var body struct {
		Replicas uint64 `json:"replicas"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	op, err := a.orch.Scale(r.Context(), name, body.Replicas)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, op.snapshot())
}

func (a *API) restart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if a.maybeProxyWrite(w, r, "/api/v1/services/"+name+"/restart") {
		return
	}
	op, err := a.orch.Restart(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, op.snapshot())
}

func (a *API) remove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if a.maybeProxyWrite(w, r, "/api/v1/services/"+name) {
		return
	}
	op, err := a.orch.Remove(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, op.snapshot())
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	label := r.URL.Query().Get("label")
	svcs, err := a.orch.ListServices(r.Context(), label)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, svcs)
}

func (a *API) inspect(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	d, err := a.orch.Inspect(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (a *API) listOps(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.orch.store.List())
}

func (a *API) getOp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	op, ok := a.orch.GetOperation(id)
	if !ok {
		writeErr(w, http.StatusNotFound, errNotFound("operation", id))
		return
	}
	writeJSON(w, http.StatusOK, op)
}

// self reports the local node's swarm role (HA awareness).
func (a *API) self(w http.ResponseWriter, r *http.Request) {
	si, err := a.orch.Self(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, si)
}

// listAudit returns audit entries (newest-first), optionally filtered by action.
func (a *API) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if a.audit == nil {
		writeJSON(w, http.StatusOK, []audit.Entry{})
		return
	}
	writeJSON(w, http.StatusOK, a.audit.List(audit.Action(q.Get("action")), limit))
}

// listEvents returns monitoring events, optionally filtered by service and
// type, newest-first. Example: /api/v1/events?service=e2e&type=log_match&limit=50
func (a *API) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	typ := monitor.EventType(q.Get("type"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if a.events == nil {
		writeJSON(w, http.StatusOK, []monitor.Event{})
		return
	}
	writeJSON(w, http.StatusOK, a.events(q.Get("service"), typ, limit))
}

// ---- helpers ----

// decodeConfig reads a YAML/JSON config body, applies defaults + validation.
func decodeConfig(w http.ResponseWriter, r *http.Request) (*config.Config, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return nil, false
	}
	if len(body) == 0 {
		writeErr(w, http.StatusBadRequest, errEmpty("config body"))
		return nil, false
	}
	// Hint format from Content-Type; default to YAML.
	name := "deploy.yaml"
	if strings.Contains(r.Header.Get("Content-Type"), "json") {
		name = "deploy.json"
	}
	cfg, err := config.LoadBytes(name, body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return nil, false
	}
	return cfg, true
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// errNotFound / errEmpty are sentinel builders; wrap so messages are friendly.
type simpleErr struct{ msg string }

func (e simpleErr) Error() string { return e.msg }

func errNotFound(what, id string) error { return simpleErr{what + " " + id + " not found"} }
func errEmpty(what string) error        { return simpleErr{what + " is empty"} }
