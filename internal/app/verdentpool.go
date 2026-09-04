// verdentpool.go — Verdent JWT account pool: round-robin with failover,
// persisted to config.json next to the binary (port of the qoder2api pool
// pattern). config.json is the single source of truth; VERDENT_TOKEN /
// VERDENT_TOKENS_FILE only seed it on first run.
package app

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// VerdentAccount is one Verdent identity: a JWT plus the stable device
// fingerprint the upstream expects (X-Device-ID header, session_id payload).
type VerdentAccount struct {
	JWT        string
	DeviceID   string
	SessionID  string
	Label      string // derived from the JWT payload (email/name)
	Disabled   bool   // auth rejected upstream; re-enable manually
	FailStreak int
	ExpireAt   int64 // JWT exp (ms); 0 = unknown

	jar   *jar          // cookies for the usage endpoints (AWSALB affinity)
	Usage *AccountUsage // last quota snapshot (dashboard)
}

// poolConfig is the config.json schema.
type poolConfig struct {
	Accounts      []poolAccount           `json:"accounts"`
	ActiveJWT     string                  `json:"active_jwt"`
	DefaultModel  string                  `json:"default_model"`
	Models        []string                `json:"models,omitempty"`         // lineup override (dashboard)
	ModelSettings map[string]ModelSetting `json:"model_settings,omitempty"` // per-model context/thinking/effort
}

// ModelSetting is the per-model dashboard choice (qoder2api pattern):
// Context "300K"/"1M", Thinking default/off/on, Effort low/high/max…
type ModelSetting struct {
	Context  string `json:"context,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	Effort   string `json:"effort,omitempty"`
}

type poolAccount struct {
	JWT       string `json:"jwt"`
	DeviceID  string `json:"device_id"`
	SessionID string `json:"session_id"`
	Disabled  bool   `json:"disabled,omitempty"`
}

// AccountPool manages the account list. pick() walks round-robin from the
// active account skipping disabled ones — the same policy as qoder2api.
type AccountPool struct {
	mu       sync.Mutex
	accounts []*VerdentAccount
	active   int
	path     string

	defaultModel  string
	models        []string // dashboard-managed lineup override (nil = default/env)
	modelSettings map[string]ModelSetting

	onAdd func(*VerdentAccount) // set by Main: prime catalog+usage
}

// configPath prefers the working directory, then the executable directory.
func poolConfigPath() string {
	if _, err := os.Stat("config.json"); err == nil {
		return "config.json"
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "config.json")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "config.json"
}

// NewAccountPool loads config.json; when it has no accounts it seeds from
// VERDENT_TOKEN / VERDENT_TOKENS_FILE and persists the result.
func NewAccountPool(envToken, tokensFile string) *AccountPool {
	p := &AccountPool{path: poolConfigPath()}
	raw, err := os.ReadFile(p.path)
	if err == nil {
		var cfg poolConfig
		if json.Unmarshal(raw, &cfg) == nil {
			for _, a := range cfg.Accounts {
				if a.JWT == "" {
					continue
				}
				acc := &VerdentAccount{
					JWT: a.JWT, DeviceID: a.DeviceID, SessionID: a.SessionID,
					Label: jwtLabel(a.JWT), Disabled: a.Disabled,
					jar: newJar(),
				}
				p.accounts = append(p.accounts, acc)
			}
			p.defaultModel = cfg.DefaultModel
			p.modelSettings = cfg.ModelSettings
			p.setActiveByJWT(cfg.ActiveJWT)
			if len(cfg.Models) > 0 {
				p.models = append([]string(nil), cfg.Models...)
				setCustomModelIDs(cfg.Models) // dashboard lineup beats env/default
			}
		}
	}

	if len(p.accounts) == 0 {
		for _, jwt := range seedTokens(envToken, tokensFile) {
			p.accounts = append(p.accounts, newVerdentAccount(jwt))
		}
		if len(p.accounts) > 0 {
			log.Printf("[Pool] seeded %d account(s) from env — persisted to %s", len(p.accounts), p.path)
			p.save()
		}
	}
	return p
}

// verdentUserID extracts the user_id claim (accounts are deduped by it,
// like the reference router: a re-login refreshes the same account).
func verdentUserID(jwt string) string {
	payload, err := jwtPayload(jwt)
	if err != nil {
		return ""
	}
	switch v := payload["user_id"].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatInt(int64(v), 10)
	}
	return ""
}

func verdentExpireAt(jwt string) int64 {
	payload, err := jwtPayload(jwt)
	if err != nil {
		return 0
	}
	if v, ok := payload["exp"].(float64); ok {
		return int64(v) * 1000
	}
	return 0
}

func newVerdentAccount(jwt string) *VerdentAccount {
	return &VerdentAccount{
		JWT:       jwt,
		DeviceID:  uuidV4(),
		SessionID: uuidV4(),
		Label:     jwtLabel(jwt),
		ExpireAt:  verdentExpireAt(jwt),
		jar:       newJar(),
	}
}

// seedTokens collects JWTs from a tokens file (one per line, # comments)
// with the single-token env fallback.
func seedTokens(envToken, tokensFile string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(t string) {
		t = strings.TrimSpace(t)
		if t != "" && !strings.HasPrefix(t, "#") && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	if tokensFile != "" {
		if data, err := os.ReadFile(tokensFile); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				add(line)
			}
		}
	}
	add(envToken)
	return out
}

// pick returns the (attempt+1)-th non-disabled account starting from active.
// nil when every account is disabled.
func (p *AccountPool) pick(attempt int) *VerdentAccount {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.accounts)
	if n == 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	for step := 0; step < n; step++ {
		acc := p.accounts[(p.active+step)%n]
		if acc.Disabled {
			continue
		}
		if acc.ExpireAt > 0 && now > acc.ExpireAt-60_000 {
			continue // expired JWT (reference: 60s safety margin)
		}
		if attempt == 0 {
			return acc
		}
		attempt--
	}
	return nil
}

// disableAccount marks an account dead after an upstream auth rejection and
// switches the active slot to the next live one (dashboard-visible).
func (p *AccountPool) disableAccount(acc *VerdentAccount) {
	p.mu.Lock()
	defer p.mu.Unlock()
	acc.Disabled = true
	if p.accounts[p.active].Disabled {
		p.active = p.nextActiveLocked()
	}
	p.save()
}

func (p *AccountPool) nextActiveLocked() int {
	n := len(p.accounts)
	for step := 1; step <= n; step++ {
		i := (p.active + step) % n
		if !p.accounts[i].Disabled {
			return i
		}
	}
	return p.active
}

func (p *AccountPool) noteOK(acc *VerdentAccount) {
	p.mu.Lock()
	defer p.mu.Unlock()
	acc.FailStreak = 0
}

func (p *AccountPool) setActiveByJWT(jwt string) {
	if jwt == "" {
		return
	}
	for i, a := range p.accounts {
		if a.JWT == jwt {
			p.active = i
			return
		}
	}
}

// ── Dashboard mutations ──

func (p *AccountPool) AddJWT(jwt string) error {
	jwt = strings.TrimSpace(jwt)
	if jwt == "" {
		return fmt.Errorf("empty JWT")
	}
	if !strings.Contains(jwt, ".") {
		return fmt.Errorf("not a JWT (expected header.payload.signature)")
	}
	p.mu.Lock()
	if acc := p.upsertLocked(jwt, ""); acc != nil {
		hook := p.onAdd
		p.save()
		p.mu.Unlock()
		log.Printf("[Pool] added account %s (%d total)", maskJWT(jwt), len(p.accounts))
		if hook != nil {
			go hook(acc)
		}
		return nil
	}
	p.mu.Unlock()
	return fmt.Errorf("invalid JWT")
}

// upsertLocked adds the JWT or, when the same user_id is already pooled,
// refreshes that account in place (re-login flow). Caller holds p.mu.
func (p *AccountPool) upsertLocked(jwt, deviceID string) *VerdentAccount {
	for _, a := range p.accounts {
		if a.JWT == jwt {
			return nil // exact duplicate
		}
	}
	uid := verdentUserID(jwt)
	if uid != "" {
		for _, a := range p.accounts {
			if verdentUserID(a.JWT) == uid {
				a.JWT = jwt
				a.ExpireAt = verdentExpireAt(jwt)
				a.Disabled = false
				a.FailStreak = 0
				if deviceID != "" {
					a.DeviceID = deviceID
				}
				log.Printf("[Pool] account refreshed for user %s (token updated)", uid)
				return a
			}
		}
	}
	acc := newVerdentAccount(jwt)
	if deviceID != "" {
		acc.DeviceID = deviceID
	}
	p.accounts = append(p.accounts, acc)
	return acc
}

// AddAccountWithDevice installs a JWT with an externally-bound device id
// (OAuth flow: the login URL carried id=<deviceId>).
func (p *AccountPool) AddAccountWithDevice(jwt, deviceID string) error {
	jwt = strings.TrimSpace(jwt)
	if jwt == "" || !strings.Contains(jwt, ".") {
		return fmt.Errorf("empty/invalid JWT from OAuth exchange")
	}
	p.mu.Lock()
	acc := p.upsertLocked(jwt, deviceID)
	if acc == nil {
		p.mu.Unlock()
		return fmt.Errorf("JWT already in the pool")
	}
	hook := p.onAdd
	p.save()
	p.mu.Unlock()
	log.Printf("[Pool] OAuth login: account %s (%d total)", maskJWT(jwt), len(p.accounts))
	if hook != nil {
		go hook(acc)
	}
	return nil
}

func (p *AccountPool) RemoveIndex(i int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i < 0 || i >= len(p.accounts) {
		return fmt.Errorf("bad index")
	}
	if len(p.accounts) == 1 {
		return fmt.Errorf("refusing to remove the last account")
	}
	p.accounts = append(p.accounts[:i], p.accounts[i+1:]...)
	if p.active >= len(p.accounts) {
		p.active = 0
	}
	p.save()
	return nil
}

func (p *AccountPool) SelectIndex(i int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i < 0 || i >= len(p.accounts) {
		return fmt.Errorf("bad index")
	}
	if p.accounts[i].Disabled {
		p.accounts[i].Disabled = false // manual "Use" re-enables a dead account
	}
	p.active = i
	p.save()
	return nil
}

// RefreshUsage re-fetches usage windows + user info for every account.
func (p *AccountPool) RefreshUsage() {
	p.mu.Lock()
	accounts := append([]*VerdentAccount(nil), p.accounts...)
	p.mu.Unlock()
	for _, acc := range accounts {
		u := fetchAccountUsage(acc)
		p.mu.Lock()
		acc.Usage = u
		if u.Email != "" && acc.Label == maskJWT(acc.JWT) {
			acc.Label = u.Email
		}
		p.mu.Unlock()
	}
}

// DefaultModel returns the dashboard-configured default (first free model
// when unset).
func (p *AccountPool) DefaultModel() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.defaultModel != "" {
		return p.defaultModel
	}
	if ids := verdentModelIDs(); len(ids) > 0 {
		return ids[0]
	}
	return "glm-5.3-flash-free"
}

func (p *AccountPool) SetDefaultModel(model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.defaultModel = model
	p.save()
}

// SetModels replaces the model lineup (persisted + applied live).
func (p *AccountPool) SetModels(ids []string) {
	if len(ids) == 0 {
		return
	}
	p.mu.Lock()
	p.models = append(p.models[:0], ids...)
	p.save()
	p.mu.Unlock()
	setCustomModelIDs(ids)
}

// ModelLineup returns the persisted lineup (nil when never customized).
func (p *AccountPool) ModelLineup() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.models...)
}

// modelSettingsSnapshot feeds the dashboard settings selects.
func (p *AccountPool) modelSettingsSnapshot() map[string]ModelSetting {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]ModelSetting, len(p.modelSettings))
	for k, v := range p.modelSettings {
		out[k] = v
	}
	return out
}

// ModelSettingFor returns the stored per-model setting (zero value when unset).
func (p *AccountPool) ModelSettingFor(model string) ModelSetting {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.modelSettings[model]
}

// SetModelSetting merges one setting field for a model and persists.
func (p *AccountPool) SetModelSetting(model string, set ModelSetting) {
	p.mu.Lock()
	if p.modelSettings == nil {
		p.modelSettings = map[string]ModelSetting{}
	}
	cur := p.modelSettings[model]
	apply := func(curVal, in string) string {
		switch in {
		case "":
			return curVal // untouched
		case "default":
			return "" // reset to model default
		}
		return in
	}
	cur.Context = apply(cur.Context, set.Context)
	cur.Thinking = apply(cur.Thinking, set.Thinking)
	cur.Effort = apply(cur.Effort, set.Effort)
	p.modelSettings[model] = cur
	p.save()
	p.mu.Unlock()
}

// Snapshot renders the Accounts dashboard card.
func (p *AccountPool) Snapshot() []map[string]interface{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]interface{}, 0, len(p.accounts))
	for i, a := range p.accounts {
		entry := map[string]interface{}{
			"index":    i,
			"jwt":      maskJWT(a.JWT),
			"active":   i == p.active,
			"user":     a.Label,
			"disabled": a.Disabled,
		}
		if a.Usage != nil {
			entry["usage"] = a.Usage
		}
		out = append(out, entry)
	}
	return out
}

// StatusSnapshot renders the Status dashboard card.
func (p *AccountPool) StatusSnapshot() (count, active int, activeJWT, activeUser string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.accounts) == 0 {
		return 0, -1, "", ""
	}
	acc := p.accounts[p.active]
	return len(p.accounts), p.active, maskJWT(acc.JWT), acc.Label
}

// save persists the pool; call with p.mu held.
func (p *AccountPool) save() {
	cfg := poolConfig{ActiveJWT: "", DefaultModel: p.defaultModel, Models: p.models, ModelSettings: p.modelSettings}
	if len(p.accounts) > 0 {
		cfg.ActiveJWT = p.accounts[p.active].JWT
	}
	for _, a := range p.accounts {
		cfg.Accounts = append(cfg.Accounts, poolAccount{
			JWT: a.JWT, DeviceID: a.DeviceID, SessionID: a.SessionID, Disabled: a.Disabled,
		})
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(p.path, raw, 0o600); err != nil {
		log.Printf("[Pool] config save failed: %v", err)
	}
}
