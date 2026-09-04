// verdentmeta.go — live Verdent metadata: the model catalog
// (GET llm-proxy.verdent.ai/config/model_list) and per-account usage
// (GET api.verdent.ai/verdent/usage_windows, agent.verdent.ai/user/center/info).
// The catalog is cached 5 min; usage endpoints are best-effort — some
// backends reject proxy traffic with 401 权限无效, in which case the
// dashboard shows the observed proxy stats instead.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	catalogUpstreamURL = "https://llm-proxy.verdent.ai/config/model_list"
	usageUpstreamURL   = "https://api.verdent.ai/verdent/usage_windows?team_id=0"
	userInfoURLBase    = "https://agent.verdent.ai/user/center/info"
)

const (
	catalogTTL         = 5 * time.Minute
	catalogFailBackoff = time.Minute
)

// ── Model catalog ──

type rawModelConfig struct {
	Key            string `json:"key"`
	Label          string `json:"label"`
	Icon           string `json:"icon"`
	Description    string `json:"description"`
	ContextWindow  string `json:"contextWindow"`
	ContextWindows []struct {
		Display string `json:"contextWindowDisplay"`
		Tokens  int    `json:"contextWindowTokens"`
	} `json:"contextWindows"`
	CostMultiplier     float64 `json:"costMultiplier"`
	CreditsDescription string  `json:"creditsDescription"`
	IsHidden           bool    `json:"is_hidden"`
	IsLimitFree        bool    `json:"is_limit_free"`
	SupportsImages     bool    `json:"supportsImages"`
	SupportsThinking   bool    `json:"supports_thinking"`
	MaxOutputTokens    int     `json:"default_max_output_tokens"`
	EffortLevels       []struct {
		Level int    `json:"level"`
		Label string `json:"label"`
	} `json:"effortLevels"`
	Provider []string `json:"provider"`
}

var (
	catalogMu       sync.Mutex
	catalogCache    []ModelInfo
	catalogAt       time.Time
	catalogLastFail time.Time
	catalogFetching bool

	catalogVersion atomic.Value // string, "model-catalog-<ms>" (app-style trace tag)
)

func currentCatalogVersion() string {
	if v, ok := catalogVersion.Load().(string); ok {
		return v
	}
	return ""
}

// modelCatalogLive returns the cached-or-fresh upstream catalog. The caller
// supplies an account (the endpoint needs a JWT); nil account → no fetch.
func modelCatalogLive(acc *VerdentAccount) []ModelInfo {
	catalogMu.Lock()
	defer catalogMu.Unlock()

	if len(catalogCache) > 0 && time.Since(catalogAt) < catalogTTL {
		return catalogCache
	}
	if acc == nil || catalogFetching ||
		(!catalogLastFail.IsZero() && time.Since(catalogLastFail) < catalogFailBackoff) {
		return catalogCache
	}
	catalogFetching = true
	defer func() { catalogFetching = false }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, catalogUpstreamURL, nil)
	if err != nil {
		catalogLastFail = time.Now()
		return catalogCache
	}
	h := req.Header
	h.Set("Accept", "*/*")
	h.Set("X-Verdent-Version", "2.13.1")
	h.Set("X-Version-Code", "2.13.1")
	h.Set("X-Verdent-Device-Id", acc.DeviceID)
	h.Set("X-Team-ID", "0")
	h.Set("X-Device-Type", "pc")
	h.Set("X-OS-Type", verdentOSName())
	h.Set("Authorization", "Bearer "+acc.JWT)

	resp, err := verdentHTTPClient.Do(req)
	if err != nil {
		catalogLastFail = time.Now()
		log.Printf("[Catalog] fetch failed: %v", err)
		return catalogCache
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		catalogLastFail = time.Now()
		log.Printf("[Catalog] HTTP %d", resp.StatusCode)
		return catalogCache
	}

	cats, err := parseModelList(body)
	if err != nil || len(cats) == 0 {
		catalogLastFail = time.Now()
		log.Printf("[Catalog] parse: %v", err)
		return catalogCache
	}
	catalogCache = cats
	catalogAt = time.Now()
	catalogVersion.Store("model-catalog-" + fmt.Sprintf("%d", catalogAt.UnixMilli()))
	catalogLastFail = time.Time{}
	setLiveCatalog(cats)
	log.Printf("[Catalog] live: %d models, %d free", len(cats), len(freeModelIDs(cats)))
	return catalogCache
}

// parseModelList converts the upstream /config/model_list payload.
func parseModelList(body []byte) ([]ModelInfo, error) {
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ModelConfig []rawModelConfig `json:"model_config"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(resp.Data.ModelConfig))
	for _, m := range resp.Data.ModelConfig {
		if m.Key == "" || m.IsHidden {
			continue
		}
		mi := ModelInfo{
			ID:                 m.Key,
			Name:               m.Label,
			Description:        m.Description,
			CreditsDescription: m.CreditsDescription,
			CostMultiplier:     m.CostMultiplier,
			IsLimitFree:        m.IsLimitFree,
			SupportsImages:     m.SupportsImages,
			SupportsThinking:   m.SupportsThinking,
			MaxOutputTokens:    m.MaxOutputTokens,
			Provider:           strings.Join(m.Provider, ","),
		}
		for _, cw := range m.ContextWindows {
			mi.ContextWindows = append(mi.ContextWindows, ContextWindow{Display: cw.Display, Tokens: cw.Tokens})
		}
		for _, el := range m.EffortLevels {
			mi.EffortLevels = append(mi.EffortLevels, el.Label)
		}
		out = append(out, mi)
	}
	return out, nil
}

// startCatalogRefresher keeps the live catalog warm; intended as a
// background goroutine. While no account exists it retries every 15s;
// after a fetch it sleeps the full TTL.
func (s *Server) startCatalogRefresher() {
	for {
		if acc := s.accounts.pick(0); acc != nil {
			modelCatalogLive(acc)
			time.Sleep(catalogTTL)
		} else {
			time.Sleep(15 * time.Second)
		}
	}
}

// primeAccountMetadata fetches catalog + usage right after an account is
// added, so the dashboard fills in without waiting for the next cycle.
func primeAccountMetadata(s *Server, acc *VerdentAccount) {
	modelCatalogLive(acc)
	if u := fetchAccountUsage(acc); u != nil {
		s.accounts.mu.Lock()
		acc.Usage = u
		if u.Email != "" && acc.Label == maskJWT(acc.JWT) {
			acc.Label = u.Email
		}
		s.accounts.mu.Unlock()
	}
}

// ── Per-account usage (best-effort) ──

type AccountUsage struct {
	Email         string  `json:"email,omitempty"`
	FreeCredits   int     `json:"free_credits,omitempty"`
	IsSubscribe   bool    `json:"is_subscribe,omitempty"`
	TokenFree     int     `json:"token_free,omitempty"`
	TokenConsumed int     `json:"token_consumed,omitempty"`
	Free5h        float64 `json:"free_5h_used"` // used fraction 0..1; -1 unknown
	Free7d        float64 `json:"free_7d_used"`
	Eco5h         float64 `json:"eco_5h_used"`
	Eco7d         float64 `json:"eco_7d_used"`
	EcoAvailable  bool    `json:"eco_available,omitempty"`
	Reset5h       int64   `json:"reset_5h_ms,omitempty"`
	Reset7d       int64   `json:"reset_7d_ms,omitempty"`
	FetchedAt     string  `json:"fetched_at,omitempty"`
	Err           string  `json:"error,omitempty"`
}

// jar is a minimal per-account cookie store (keeps AWSALB affinity).
type jar struct {
	mu      sync.Mutex
	cookies map[string]string
}

func newJar() *jar { return &jar{cookies: map[string]string{}} }

func (j *jar) header(host string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if c, ok := j.cookies[host]; ok {
		return c
	}
	return ""
}

func (j *jar) store(host, setCookies string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cookies[host] = setCookies
}

// getJSON does one GET with cookie continuity, returning status + body.
func getJSON(ctx context.Context, url, cookie string, headers map[string]string) (int, string, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", ""
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	req.Header.Set("User-Agent", "node-fetch")
	req.Header.Set("Accept", "*/*")

	resp, err := verdentHTTPClient.Do(req)
	if err != nil {
		return 0, "", ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	setCookie := resp.Header.Get("Set-Cookie")
	return resp.StatusCode, string(body), setCookie
}

// fetchAccountUsage fills usage for one account. Failures are recorded in
// Usage.Err — the proxy keeps working without quotas.
func fetchAccountUsage(acc *VerdentAccount) *AccountUsage {
	u := &AccountUsage{Free5h: -1, Free7d: -1, Eco5h: -1, Eco7d: -1} // -1 until fetched
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const apiHost = "api.verdent.ai"
	const agentHost = "agent.verdent.ai"

	authCookie := func(host string) string {
		c := "token=" + acc.JWT
		if extra := acc.jar.header(host); extra != "" {
			c += "; " + extra
		}
		return c
	}

	// usage windows (one retry with fresh ALB cookies on 401)
	for attempt := 0; attempt < 2; attempt++ {
		status, body, setC := getJSON(ctx, usageUpstreamURL, authCookie(apiHost), nil)
		if setC != "" {
			acc.jar.store(apiHost, mergeCookies(acc.jar.header(apiHost), setC))
		}
		log.Printf("[Usage] %s windows: HTTP %d (attempt %d)", maskJWT(acc.JWT), status, attempt)
		if status == 200 {
			var r struct {
				ErrCode int `json:"errCode"`
				Data    struct {
					FreeMode struct {
						Ratio5h   float64 `json:"ratio_5h"`
						Ratio7d   float64 `json:"ratio_7d"`
						ResetAt5h int64   `json:"reset_at_5h"`
						ResetAt7d int64   `json:"reset_at_7d"`
					} `json:"free_mode"`
					EcoMode struct {
						Ratio5h     float64 `json:"ratio_5h"`
						Ratio7d     float64 `json:"ratio_7d"`
						ResetAt5h   int64   `json:"reset_at_5h"`
						ResetAt7d   int64   `json:"reset_at_7d"`
						IsAvailable bool    `json:"is_available"`
					} `json:"eco_mode"`
				} `json:"data"`
			}
			if json.Unmarshal([]byte(body), &r) == nil && r.ErrCode == 0 {
				// free ratios are fractions (0.34); eco comes as percent (100)
				norm := func(v float64) float64 {
					if v > 1 {
						return v / 100
					}
					return v
				}
				u.Free5h, u.Free7d = norm(r.Data.FreeMode.Ratio5h), norm(r.Data.FreeMode.Ratio7d)
				u.Reset5h, u.Reset7d = r.Data.FreeMode.ResetAt5h, r.Data.FreeMode.ResetAt7d
				u.Eco5h, u.Eco7d = norm(r.Data.EcoMode.Ratio5h), norm(r.Data.EcoMode.Ratio7d)
				u.EcoAvailable = r.Data.EcoMode.IsAvailable
			}
			break
		}
		if status != 401 {
			break
		}
	}

	// user info (email, credits)
	status, body, _ := getJSON(ctx, userInfoURLBase+"?device_id="+acc.DeviceID,
		authCookie(agentHost), map[string]string{
			"x-device-type": "pc",
			"x-os-type":     verdentOSName(),
		})
	if status == 200 {
		var r struct {
			Data struct {
				Email       string `json:"email"`
				FreeCredits int    `json:"freeCredits"`
				IsSubscribe bool   `json:"isSubscribe"`
				TokenInfo   struct {
					TokenFree         int `json:"tokenFree"`
					TokenConsumed     int `json:"tokenConsumed"`
					TokenFlexibleLeft int `json:"tokenFlexibleLeft"`
				} `json:"tokenInfo"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(body), &r) == nil {
			u.Email = r.Data.Email
			u.FreeCredits = r.Data.FreeCredits
			u.IsSubscribe = r.Data.IsSubscribe
			u.TokenFree = r.Data.TokenInfo.TokenFree
			u.TokenConsumed = r.Data.TokenInfo.TokenConsumed
		}
	}

	if u.Free5h < 0 && u.Email == "" {
		u.Err = "upstream returned no usage (401?)"
	}
	u.FetchedAt = time.Now().Format(time.RFC3339)
	return u
}

// mergeCookies appends a Set-Cookie value (name=val) to an existing
// cookie header, replacing same-name entries. ponytail: parses nothing
// beyond the first ';' — Verdent only ever sets AWSALB/AWSALBCORS here.
func mergeCookies(cur, set string) string {
	name := set
	if i := strings.Index(name, "="); i > 0 {
		name = name[:i]
	} else {
		return cur
	}
	var kept []string
	for _, part := range strings.Split(cur, "; ") {
		if part != "" && !strings.HasPrefix(part, name+"=") {
			kept = append(kept, part)
		}
	}
	val := set
	if i := strings.Index(val, ";"); i > 0 {
		val = val[:i]
	}
	kept = append(kept, strings.TrimSpace(val))
	return strings.Join(kept, "; ")
}
