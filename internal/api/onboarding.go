package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/tai-core/tai-talea/internal/onboarding"
)

type consoleOwnerKey struct{}

func consolePartner(r *http.Request) string {
	owner, _ := r.Context().Value(consoleOwnerKey{}).(string)
	return owner
}
func (s *Server) consoleAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if requireAdmin(r, s.opts.Config.Server.AdminToken) {
			next(w, r)
			return
		}
		token := bearerToken(r.Header.Get("Authorization"))
		for _, partner := range s.opts.Config.Partners {
			if token != "" && partner.ConsoleToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(partner.ConsoleToken)) == 1 {
				next(w, r.WithContext(context.WithValue(r.Context(), consoleOwnerKey{}, partner.ID)))
				return
			}
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized", "message": "请输入有效的管理令牌或合作商页面令牌"})
	}
}
func (s *Server) ownConsoleInstance(next http.HandlerFunc) http.HandlerFunc {
	return s.consoleAuth(func(w http.ResponseWriter, r *http.Request) {
		if owner := consolePartner(r); owner != "" {
			instance, err := s.opts.Store.GetInstance(r.Context(), r.PathValue("id"))
			if err != nil || instance.PartnerID != owner {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
				return
			}
		}
		next(w, r)
	})
}
func (s *Server) registerOnboarding(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/capacity/console/instances", s.consoleAuth(s.handleListInstances))
	mux.HandleFunc("GET /v1/capacity/console/instances/{id}", s.ownConsoleInstance(s.handleGetInstance))
	mux.HandleFunc("GET /v1/capacity/console/instances/{id}/audit", s.ownConsoleInstance(s.handleInstanceAudit))
	mux.HandleFunc("POST /v1/capacity/console/instances/{id}/reclaim", s.ownConsoleInstance(s.handleReclaimInstance))
	mux.HandleFunc("GET /v1/capacity/console/onboarding", s.consoleAuth(func(w http.ResponseWriter, r *http.Request) {
		jobs := []onboarding.Job{}
		if s.opts.Onboarding != nil {
			jobs = s.opts.Onboarding.List(consolePartner(r))
		}
		writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
	}))
	mux.HandleFunc("POST /v1/capacity/console/onboarding", s.consoleAuth(s.handleOnboard))
}
func (s *Server) handleOnboard(w http.ResponseWriter, r *http.Request) {
	if s.opts.Onboarding == nil {
		writeJSON(w, 503, map[string]string{"message": "控制节点尚未配置自动安装制品"})
		return
	}
	var payload onboarding.Request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil {
		writeJSON(w, 400, map[string]string{"message": "上线参数格式不正确"})
		return
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		writeJSON(w, 400, map[string]string{"message": "只允许一份上线参数"})
		return
	}
	if owner := consolePartner(r); owner != "" {
		if payload.PartnerID != "" && payload.PartnerID != owner {
			writeJSON(w, 403, map[string]string{"message": "不能替其他合作商提交节点"})
			return
		}
		payload.PartnerID = owner
	}
	known := false
	for _, p := range s.opts.Config.Partners {
		if p.ID == payload.PartnerID {
			known = true
		}
	}
	if !known {
		writeJSON(w, 400, map[string]string{"message": "请选择已配置的合作商"})
		return
	}
	job, err := s.opts.Onboarding.Submit(r.Context(), payload)
	if err != nil {
		code := 400
		if errors.Is(err, onboarding.ErrConflict) {
			code = 409
		}
		writeJSON(w, code, map[string]string{"message": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "job": job})
}
