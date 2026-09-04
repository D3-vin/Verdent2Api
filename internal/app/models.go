// models.go — Verdent model catalog: live upstream list
// (GET /config/model_list, see verdentmeta.go) with a static fallback.
// Lineup precedence: dashboard custom list (config.json) > live free models
// (is_limit_free) > VERDENT_MODELS env > built-in pair.
package app

import (
	"log"
	"os"
	"strings"
	"sync"
)

type ContextWindow struct {
	Display string
	Tokens  int
}

type ModelInfo struct {
	ID                 string
	Name               string
	Description        string
	Provider           string
	ContextWindows     []ContextWindow
	CostMultiplier     float64
	IsLimitFree        bool
	CreditsDescription string
	SupportsImages     bool
	SupportsThinking   bool
	MaxOutputTokens    int
	EffortLevels       []string
}

var defaultVerdentModels = []ModelInfo{
	{ID: "glm-5.3-flash-free", Name: "GLM-5.3-Flash",
		Description: "GLM-5.3 flash — free Verdent tier, coding and chat",
		IsLimitFree: true, MaxOutputTokens: 64000, SupportsThinking: true,
		EffortLevels:   []string{"low", "high", "max"},
		ContextWindows: []ContextWindow{{"300K", 300000}, {"1M", 1000000}}},
	{ID: "deepseek-v4-flash-free", Name: "DeepSeek-V4-Flash",
		Description: "DeepSeek V4 flash — free Verdent tier, coding and chat",
		IsLimitFree: true, MaxOutputTokens: 64000, SupportsThinking: true,
		EffortLevels:   []string{"low", "high", "max"},
		ContextWindows: []ContextWindow{{"300K", 300000}, {"1M", 1000000}}},
}

var (
	modelsMu    sync.RWMutex
	liveCatalog []ModelInfo // full upstream catalog (nil until fetched)
	lineup      []ModelInfo // models we actually serve
	customIDs   []string    // dashboard lineup (nil = auto from live/free)
)

// rebuildLineupLocked derives the served lineup. Caller holds modelsMu.
func rebuildLineupLocked() {
	var ids []string
	switch {
	case len(customIDs) > 0:
		ids = customIDs
	case len(liveCatalog) > 0:
		ids = freeModelIDs(liveCatalog)
	default:
		ids = initialIDs()
	}
	lineup = nil
	for _, id := range ids {
		lineup = append(lineup, modelInfoFor(id))
	}
}

// initialIDs is the offline fallback: VERDENT_MODELS env or the free pair.
func initialIDs() []string {
	env := strings.TrimSpace(os.Getenv("VERDENT_MODELS"))
	if env == "" {
		return []string{defaultVerdentModels[0].ID, defaultVerdentModels[1].ID}
	}
	var ids []string
	for _, id := range strings.Split(env, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return []string{defaultVerdentModels[0].ID, defaultVerdentModels[1].ID}
	}
	log.Printf("[Models] VERDENT_MODELS override: %d models", len(ids))
	return ids
}

// modelInfoFor resolves a model id against the live catalog first, then the
// built-in pair. Caller holds modelsMu (or accepts a racy read for tests).
func modelInfoFor(id string) ModelInfo {
	if len(liveCatalog) > 0 {
		for _, m := range liveCatalog {
			if strings.EqualFold(m.ID, id) {
				return m
			}
		}
	}
	for _, m := range defaultVerdentModels {
		if strings.EqualFold(m.ID, id) {
			return m
		}
	}
	return ModelInfo{ID: id, Name: id, Description: "Verdent model", MaxOutputTokens: 64000}
}

func freeModelIDs(catalog []ModelInfo) []string {
	var ids []string
	for _, m := range catalog {
		if m.IsLimitFree {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 { // defensive: never serve an empty lineup
		for _, m := range defaultVerdentModels {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// setLiveCatalog installs a freshly fetched catalog and re-derives the
// lineup (auto mode tracks the live free list).
func setLiveCatalog(cats []ModelInfo) {
	modelsMu.Lock()
	defer modelsMu.Unlock()
	liveCatalog = cats
	rebuildLineupLocked()
}

// setCustomModelIDs is the dashboard settings path (config.json lineup).
func setCustomModelIDs(ids []string) {
	modelsMu.Lock()
	defer modelsMu.Unlock()
	customIDs = append([]string(nil), ids...)
	rebuildLineupLocked()
}

// modelMeta returns full metadata for a model id (live catalog or lineup).
func modelMeta(id string) (ModelInfo, bool) {
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	for _, m := range liveCatalog {
		if strings.EqualFold(m.ID, id) {
			return m, true
		}
	}
	for _, m := range lineup {
		if strings.EqualFold(m.ID, id) {
			return m, true
		}
	}
	return ModelInfo{}, false
}

// verdentModelIDs lists the lineup in fallback order.
func verdentModelIDs() []string {
	modelsMu.RLock()
	ids := make([]string, 0, len(lineup))
	for _, m := range lineup {
		ids = append(ids, m.ID)
	}
	modelsMu.RUnlock()
	if len(ids) == 0 {
		modelsMu.Lock()
		rebuildLineupLocked()
		modelsMu.Unlock()
		return verdentModelIDs()
	}
	return ids
}

// resolveModelAlias maps a client-requested model name to a lineup ID
// ("" when unknown — modelOrder then uses plain lineup order).
func resolveModelAlias(requested string) string {
	if requested == "" {
		return ""
	}
	lineupIDs := verdentModelIDs()
	req := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(requested, "[1m]")))
	// clients may send provider-prefixed ids ("verdent/glm-5.3-flash-free")
	if i := strings.IndexByte(req, '/'); i >= 0 {
		req = req[i+1:]
	}
	for _, id := range lineupIDs {
		if strings.EqualFold(id, req) {
			return id
		}
	}
	return ""
}

// findModelByID returns the model with a case-insensitive ID match.
func findModelByID(models []ModelInfo, id string) *ModelInfo {
	for i := range models {
		if strings.EqualFold(models[i].ID, id) {
			return &models[i]
		}
	}
	return nil
}

// modelIDs lists known model IDs (single source of truth for name checks).
func modelIDs() []string {
	return verdentModelIDs()
}

// modelCatalog returns the served lineup (for /v1/models).
func modelCatalog() []ModelInfo {
	verdentModelIDs() // ensure built
	modelsMu.RLock()
	defer modelsMu.RUnlock()
	return lineup
}

// modelMaxOutputTokens returns the model's upstream default (64000 floor).
func modelMaxOutputTokens(id string) int {
	if m, ok := modelMeta(id); ok && m.MaxOutputTokens > 0 {
		return m.MaxOutputTokens
	}
	return verdentDefaultMaxTokens
}

// modelContextTokens resolves the context_window_tokens payload value:
// the model's first (smallest) window by default, or the one matching the
// dashboard choice ("300K"/"1M"/raw tokens).
func modelContextTokens(modelID, choice string) int {
	m, ok := modelMeta(modelID)
	if !ok || len(m.ContextWindows) == 0 {
		return 300000
	}
	if choice != "" {
		for _, cw := range m.ContextWindows {
			if strings.EqualFold(cw.Display, choice) {
				return cw.Tokens
			}
		}
	}
	return m.ContextWindows[0].Tokens
}

// mapEffort maps a client reasoning_effort to a model-supported effort
// label; "" when nothing fits.
func mapEffort(modelID, requested string) string {
	if requested == "" {
		return ""
	}
	req := strings.ToLower(strings.TrimSpace(requested))
	m, ok := modelMeta(modelID)
	if !ok || len(m.EffortLevels) == 0 {
		return "" // no known levels — send nothing rather than a bad value
	}
	has := func(l string) bool {
		for _, v := range m.EffortLevels {
			if strings.EqualFold(v, l) {
				return true
			}
		}
		return false
	}
	if has(req) {
		return req
	}
	// nearest-fit mapping for OpenAI's five levels
	fallback := map[string]string{
		"minimal": "low", "medium": "high", "xhigh": "max",
	}
	if alt, ok := fallback[req]; ok && has(alt) {
		return alt
	}
	if has("high") {
		return "high"
	}
	return ""
}
