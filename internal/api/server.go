package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tai-core/tai-talea/internal/config"
	"github.com/tai-core/tai-talea/internal/controller"
	"github.com/tai-core/tai-talea/internal/domain"
	"github.com/tai-core/tai-talea/internal/obs"
	"github.com/tai-core/tai-talea/internal/planner"
	"github.com/tai-core/tai-talea/internal/store"
)

// Options configures the HTTP surface.
type Options struct {
	Config     config.Config
	Store      store.Store
	Controller *controller.Controller
	Metrics    *obs.Registry
	Logger     *slog.Logger
	// Ready reports whether the process finished recovery.
	Ready func() bool
	Now   func() time.Time
}

// Server serves the Push API and the operator admin API.
type Server struct {
	opts Options
	auth *pushAuthenticator
	now  func() time.Time
}

// New builds the HTTP server.
func New(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("api server requires a store")
	}
	if opts.Controller == nil {
		return nil, errors.New("api server requires a controller")
	}
	if err := opts.Config.Validate(); err != nil {
		return nil, err
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Server{opts: opts, auth: newPushAuthenticator(opts.Config), now: now}, nil
}

// Handler returns the fully wired HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Partner Push API (§6.1).
	mux.HandleFunc("POST /v1/capacity/events", s.handlePushEvent)

	// Operator admin API.
	mux.HandleFunc("GET /v1/capacity/instances", s.admin(s.handleListInstances))
	mux.HandleFunc("GET /v1/capacity/instances/{id}", s.admin(s.handleGetInstance))
	mux.HandleFunc("POST /v1/capacity/instances/{id}/drain", s.admin(s.handleDrainInstance))
	mux.HandleFunc("POST /v1/capacity/instances/{id}/release", s.admin(s.handleReleaseInstance))
	mux.HandleFunc("GET /v1/capacity/instances/{id}/audit", s.admin(s.handleInstanceAudit))
	mux.HandleFunc("GET /v1/capacity/events", s.admin(s.handleListEvents))
	mux.HandleFunc("GET /v1/capacity/planner", s.admin(s.handleGetPlanner))
	mux.HandleFunc("PUT /v1/capacity/planner", s.admin(s.handleSetPlannerGear))
	mux.HandleFunc("POST /v1/capacity/reconcile", s.admin(s.handleReconcile))

	// Operational endpoints.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	return s.recoverPanics(s.logRequests(mux))
}

// ---------------------------------------------------------------- middleware

func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if !requireAdmin(request, s.opts.Config.Server.AdminToken) {
			writeJSON(writer, http.StatusUnauthorized, map[string]string{
				"error":   "unauthorized",
				"message": "a valid admin bearer token is required",
			})
			return
		}
		next(writer, request)
	}
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := s.now()
		recorder := &statusRecorder{ResponseWriter: writer, status: http.StatusOK}
		next.ServeHTTP(recorder, request)
		if s.opts.Logger == nil {
			return
		}
		s.opts.Logger.Info("http request",
			"method", request.Method,
			"path", request.URL.Path,
			"status", recorder.status,
			"bytes", recorder.written,
			"duration_ms", s.now().Sub(started).Milliseconds(),
			"remote", request.RemoteAddr)
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				if s.opts.Logger != nil {
					s.opts.Logger.Error("panic while serving request",
						"path", request.URL.Path, "panic", fmt.Sprint(recovered))
				}
				writeJSON(writer, http.StatusInternalServerError, map[string]string{
					"error":   "internal_error",
					"message": "the control plane failed to handle the request",
				})
			}
		}()
		next.ServeHTTP(writer, request)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(payload []byte) (int, error) {
	written, err := r.ResponseWriter.Write(payload)
	r.written += written
	return written, err
}

// ------------------------------------------------------------------- push

func (s *Server) handlePushEvent(writer http.ResponseWriter, request *http.Request) {
	limit := s.opts.Config.Server.MaxPushBodyBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, limit))
	if err != nil {
		writeJSON(writer, http.StatusRequestEntityTooLarge, map[string]string{
			"error":   "payload_too_large",
			"message": fmt.Sprintf("request body must not exceed %d bytes", limit),
		})
		return
	}

	now := s.now()
	partnerID, signature, err := s.auth.Verify(request, body, now)
	if err != nil {
		s.recordPushFailure(err)
		status := http.StatusUnauthorized
		if errors.Is(err, ErrMissingCredentials) || errors.Is(err, ErrStaleTimestamp) {
			status = http.StatusBadRequest
		}
		writeJSON(writer, status, map[string]string{
			"error":   errorCode(err),
			"message": err.Error(),
		})
		return
	}
	if s.auth.Seen(partnerID, signature, now) && s.opts.Metrics != nil {
		// A verbatim redelivery is still answered idempotently; the counter only
		// tells operators that a partner is retrying.
		s.opts.Metrics.IncCounter("capacity_push_replays_total",
			"Push requests that repeated a signed request already seen inside the replay window.",
			[]string{"partner"}, "partner", partnerID)
	}

	event, err := decodeEvent(body)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{
			"error":   "invalid_payload",
			"message": err.Error(),
		})
		return
	}
	if event.PartnerID == "" {
		event.PartnerID = partnerID
	}
	if event.PartnerID != partnerID {
		writeJSON(writer, http.StatusForbidden, map[string]string{
			"error":   "partner_mismatch",
			"message": "event partner_id does not match the authenticated credential",
		})
		return
	}
	event.Source = domain.SourcePush
	event.ReceivedAt = now
	event.Raw = body

	outcome, handleErr := s.opts.Controller.HandleCapacityEvent(request.Context(), event)
	if handleErr != nil && !outcome.Accepted {
		writeJSON(writer, http.StatusUnprocessableEntity, outcome)
		return
	}

	status := http.StatusAccepted
	if outcome.Duplicate {
		status = http.StatusOK
	}
	if !outcome.Accepted {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(writer, status, outcome)
}

func (s *Server) recordPushFailure(err error) {
	if s.opts.Metrics == nil {
		return
	}
	s.opts.Metrics.IncCounter("capacity_push_auth_failures_total",
		"Push requests rejected before reaching the state machine.",
		[]string{"reason"}, "reason", errorCode(err))
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, ErrMissingCredentials):
		return "missing_credentials"
	case errors.Is(err, ErrUnknownPartner):
		return "unknown_partner"
	case errors.Is(err, ErrStaleTimestamp):
		return "stale_timestamp"
	case errors.Is(err, ErrBadSignature):
		return "bad_signature"
	default:
		return "unauthorized"
	}
}

// decodeEvent enforces a single JSON document so a partner cannot smuggle a
// second payload past the signature check. Unknown fields are tolerated so the
// partner protocol can grow without breaking existing senders.
func decodeEvent(body []byte) (domain.CapacityEvent, error) {
	var event domain.CapacityEvent
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if err := decoder.Decode(&event); err != nil {
		return domain.CapacityEvent{}, fmt.Errorf("decode capacity event: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return domain.CapacityEvent{}, errors.New("request body must contain exactly one json document")
	}
	return event, nil
}

// ------------------------------------------------------------------ admin

func (s *Server) handleListInstances(writer http.ResponseWriter, request *http.Request) {
	instances, err := s.opts.Store.ListInstances(request.Context())
	if err != nil {
		s.writeStoreError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"instances": instances, "count": len(instances)})
}

func (s *Server) handleGetInstance(writer http.ResponseWriter, request *http.Request) {
	instance, err := s.opts.Store.GetInstance(request.Context(), request.PathValue("id"))
	if err != nil {
		s.writeStoreError(writer, err)
		return
	}
	verdict := domain.CheckCombination(instance.InstanceState, instance.ServiceState, instance.Role)
	writeJSON(writer, http.StatusOK, map[string]any{
		"instance":     instance,
		"legality":     verdict,
		"planner_gear": s.opts.Controller.Planner().Config().Gear,
	})
}

func (s *Server) handleDrainInstance(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	grace := s.opts.Config.Controller.DrainGrace.Duration()
	if err := s.opts.Controller.BeginDrain(request.Context(), id, "operator requested drain", grace); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "drain_failed", "message": err.Error()})
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]string{
		"instance_id": id,
		"result":      "readiness closed; instance is draining",
	})
}

func (s *Server) handleReleaseInstance(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	if err := s.opts.Controller.BeginRelease(request.Context(), id, "operator requested release"); err != nil {
		writeJSON(writer, http.StatusConflict, map[string]string{"error": "release_failed", "message": err.Error()})
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]string{
		"instance_id": id,
		"result":      "container returned to the partner",
	})
}

func (s *Server) handleInstanceAudit(writer http.ResponseWriter, request *http.Request) {
	entries, err := s.opts.Store.ListAudit(request.Context(), request.PathValue("id"), 100)
	if err != nil {
		s.writeStoreError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"entries": entries, "count": len(entries)})
}

func (s *Server) handleListEvents(writer http.ResponseWriter, request *http.Request) {
	events, err := s.opts.Store.ListRecentEvents(request.Context(), 100)
	if err != nil {
		s.writeStoreError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"events": events, "count": len(events)})
}

func (s *Server) handleGetPlanner(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"gear":        s.opts.Controller.Planner().Config().Gear,
		"gears":       planner.GearNames(),
		"last_change": s.opts.Controller.Planner().LastChangeAt(),
	})
}

func (s *Server) handleSetPlannerGear(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, 8<<10))
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_payload", "message": err.Error()})
		return
	}
	var payload struct {
		Gear string `json:"gear"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_payload", "message": err.Error()})
		return
	}
	if err := s.opts.Controller.Planner().SetGear(payload.Gear); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_gear", "message": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"gear": payload.Gear, "gears": planner.GearNames()})
}

func (s *Server) handleReconcile(writer http.ResponseWriter, request *http.Request) {
	report, err := s.opts.Controller.ReconcileOnce(request.Context())
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "reconcile_failed", "message": err.Error()})
		return
	}
	decision, plannerErr := s.opts.Controller.Rebalance(request.Context())
	payload := map[string]any{"reconcile": report}
	if plannerErr != nil {
		payload["planner_error"] = plannerErr.Error()
	} else {
		payload["planner"] = decision
	}
	writeJSON(writer, http.StatusOK, payload)
}

// -------------------------------------------------------------- operational

func (s *Server) handleHealth(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(writer http.ResponseWriter, _ *http.Request) {
	if s.opts.Ready != nil && !s.opts.Ready() {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "recovering"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleMetrics(writer http.ResponseWriter, _ *http.Request) {
	if s.opts.Metrics == nil {
		writer.WriteHeader(http.StatusOK)
		return
	}
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte(s.opts.Metrics.Render()))
}

func (s *Server) writeStoreError(writer http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": "not_found", "message": err.Error()})
		return
	}
	writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "storage_error", "message": err.Error()})
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(payload)
}

// Shutdown is a helper for main: it drains the HTTP server.
func Shutdown(ctx context.Context, server *http.Server, timeout time.Duration) error {
	if server == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}
