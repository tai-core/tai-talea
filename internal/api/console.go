package api

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"time"

	"github.com/tai-core/tai-talea/internal/store"
)

//go:embed ui/*
var consoleFiles embed.FS

func (s *Server) registerConsole(mux *http.ServeMux) {
	files, _ := fs.Sub(consoleFiles, "ui")
	assets := http.FileServer(http.FS(files))
	secure := func(next http.Handler) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
			next.ServeHTTP(w, r)
		}
	}
	mux.Handle("GET /{$}", secure(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := consoleFiles.ReadFile("ui/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})))
	mux.Handle("GET /ui/", secure(http.StripPrefix("/ui/", assets)))
	s.registerOnboarding(mux)
	mux.HandleFunc("GET /v1/capacity/console/telemetry", s.consoleAuth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"telemetry": s.opts.Controller.Telemetry(consolePartner(r)), "planner_mode": "fixed", "gear": s.opts.Controller.Planner().Config().Gear})
	}))
	mux.HandleFunc("GET /v1/capacity/console", s.consoleAuth(func(w http.ResponseWriter, r *http.Request) {
		modes := map[string]string{}
		for _, partner := range s.opts.Config.Partners {
			if owner := consolePartner(r); owner != "" && owner != partner.ID {
				continue
			}
			modes[partner.ID] = partner.Adapter
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"model": s.opts.Config.Controller.ModelID, "release_modes": modes,
			"partner_id": consolePartner(r), "onboarding_enabled": s.opts.Onboarding != nil,
			"drain_grace_seconds": int(s.opts.Controller.DefaultDrainGrace().Seconds()),
			"reconcile_seconds":   int(s.opts.Config.Controller.ReconcileInterval.Duration().Seconds()),
		})
	}))
}

func (s *Server) handleReclaimInstance(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		GraceSeconds *int `json:"grace_seconds"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&payload)
	if err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_payload"})
		return
	}
	if err == nil {
		var extra any
		if !errors.Is(decoder.Decode(&extra), io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_payload"})
			return
		}
	}
	grace := s.opts.Controller.DefaultDrainGrace()
	if payload.GraceSeconds != nil {
		if *payload.GraceSeconds < 0 || *payload.GraceSeconds > 86400 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "grace_seconds must be between 0 and 86400"})
			return
		}
		grace = time.Duration(*payload.GraceSeconds) * time.Second
	}
	id := r.PathValue("id")
	if err := s.opts.Controller.RequestReclaim(r.Context(), id, "operator requested reclaim", grace); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeStoreError(w, err)
		} else {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "reclaim_failed", "message": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"instance_id": id, "accepted": true,
		"result": "reclaim requested; Talea will drain, stop and release the instance",
	})
}
