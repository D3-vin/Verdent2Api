// dashboard.go — /api/* JSON endpoints for the embedded dashboard SPA.
// Port of the qoder2api dashboard API; quota endpoints dropped (Verdent
// exposes none), model cooldowns added.
package app

import (
	"encoding/json"
	"net/http"
)

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{"error": err.Error()})
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	count, _, activeJWT, activeUser := s.accounts.StatusSnapshot()
	writeJSON(w, map[string]interface{}{
		"running":        true,
		"port":           s.cfg.Port,
		"accounts":       count,
		"active_jwt":     activeJWT,
		"active_user":    activeUser,
		"default_model":  s.accounts.DefaultModel(),
		"cooldowns":      cooldownSnapshot(),
		"model_settings": s.accounts.modelSettingsSnapshot(),
	})
}

func (s *Server) apiAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]interface{}{"accounts": s.accounts.Snapshot()})
}

func (s *Server) apiAccountsAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		JWT string `json:"jwt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.accounts.AddJWT(body.JWT); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

func (s *Server) apiAccountsRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Index int `json:"index"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.accounts.RemoveIndex(body.Index); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

func (s *Server) apiAccountsSelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Index int `json:"index"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.accounts.SelectIndex(body.Index); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

func (s *Server) apiSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		DefaultModel string   `json:"default_model"`
		Models       []string `json:"models"`
		Model        string   `json:"model"`    // per-model runtime target
		Context      string   `json:"context"`  // "300K" | "1M" | ""
		Thinking     string   `json:"thinking"` // "off" | "on" | ""
		Effort       string   `json:"effort"`   // low/high/max… | ""
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	if len(body.Models) > 0 {
		s.accounts.SetModels(body.Models)
	}
	if body.DefaultModel != "" {
		resolved := resolveModelAlias(body.DefaultModel)
		if resolved == "" {
			writeAPIError(w, http.StatusBadRequest, stringErr("unknown model: "+body.DefaultModel))
			return
		}
		s.accounts.SetDefaultModel(resolved)
	}
	if body.Model != "" {
		resolved := resolveModelAlias(body.Model)
		if resolved == "" {
			writeAPIError(w, http.StatusBadRequest, stringErr("unknown model: "+body.Model))
			return
		}
		set := ModelSetting{Context: body.Context, Thinking: body.Thinking, Effort: body.Effort}
		if set.Thinking == "off" {
			set.Thinking = "disabled"
		} else if set.Thinking == "on" {
			set.Thinking = "enabled"
		}
		if set.Context == "default" || set.Context == "model" {
			set.Context = ""
		}
		s.accounts.SetModelSetting(resolved, set)
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

// apiQuotaRefresh re-fetches usage windows + user info for all accounts.
func (s *Server) apiQuotaRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.accounts.RefreshUsage()
	writeJSON(w, map[string]interface{}{"ok": true, "accounts": s.accounts.Snapshot()})
}

type stringErr string

func (e stringErr) Error() string { return string(e) }
