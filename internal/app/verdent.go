// verdent.go — Verdent upstream client: AES-GCM payload encryption, deck
// request building, Anthropic-style SSE parsing, account/model failover.
// Protocol ported from the reference router (spflw/verdent).
package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// verdentUpstreamURL is a var so tests can point it at an httptest server.
var verdentUpstreamURL = "https://llm-proxy.verdent.ai/llm/stream"

const (
	// Key derivation (crypto.js of the reference router):
	// key = first 32 bytes of utf8(base64(PROXY_SIGN)).
	verdentProxySign = "codeck502deck_25_09_15v7"
	verdentProxyBeta = "hybrid-stream@20250919"
	verdentVersion   = "2.13.1"

	verdentDefaultMaxTokens   = 64000
	verdentDefaultTemperature = 1.0

	// Retry policy (reference: need_retry/5xx → backoff, capped attempts).
	verdentMaxAttempts = 3
	verdentRetryBaseMs = 1000
	// Model cooldown after a rate-limit: window 12s × 5 (reference router).
	verdentModelCooldown = 60 * time.Second
	// Pause before trying the next fallback model.
	verdentModelSwitchDelay = time.Second
)

var (
	errVerdentAuth      = errors.New("verdent unauthorized")
	errVerdentRateLimit = errors.New("verdent rate limited")
)

// ── Payload encryption (proxyEncode) ──

var verdentAEADOnce sync.Once
var verdentAEAD cipher.AEAD

func verdentKey() cipher.AEAD {
	verdentAEADOnce.Do(func() {
		signB64 := base64.StdEncoding.EncodeToString([]byte(verdentProxySign))
		key := []byte(signB64)[:32] // first 32 bytes of the utf-8 base64 text
		block, err := aes.NewCipher(key)
		if err != nil {
			panic("verdent: bad AES key: " + err.Error())
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			panic("verdent: GCM: " + err.Error())
		}
		verdentAEAD = gcm
	})
	return verdentAEAD
}

// proxyEncode encrypts v with AES-256-GCM and returns
// base64(nonce[12] + ciphertext + tag[16]) — byte-compatible with the
// reference router's crypto.js proxyEncode.
func proxyEncode(v interface{}) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := verdentKey().Seal(nonce, nonce, plain, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// ── Device identity (headers mimic the Verdent desktop app) ──

func verdentOSName() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "macos"
	default:
		return "linux"
	}
}

func verdentHostEnv() map[string]interface{} {
	platform, osVersion, shell := "linux", "Linux x86_64", "bash"
	switch runtime.GOOS {
	case "windows":
		platform, osVersion, shell = "win32", "Windows_NT 10.0.26200", "cmd"
	case "darwin":
		platform, osVersion, shell = "darwin", "Darwin 24.0.0", "zsh"
	}
	return map[string]interface{}{
		"platform":   platform,
		"os_version": osVersion,
		"shell":      shell,
		"today_date": time.Now().Format("2006-01-02"),
	}
}

// ── Message translation (OpenAI messages → Verdent blocks) ──

// openaiToVerdent maps the client's OpenAI-shaped messages to Verdent's
// Anthropic-style [{role, content:[blocks]}] format: consecutive same-role
// messages merge naturally (each becomes its own turn — Verdent accepts
// that), system messages collect into the system blocks, tool results ride
// along as user tool_result blocks. Port of translate.js.
func openaiToVerdent(messages []json.RawMessage) (system []map[string]interface{}, out []map[string]interface{}) {
	var pendingToolResults []map[string]interface{}

	flushTools := func() {
		if len(pendingToolResults) > 0 {
			out = append(out, map[string]interface{}{
				"role":    "user",
				"content": pendingToolResults,
			})
			pendingToolResults = nil
		}
	}

	for _, raw := range messages {
		var msg struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}

		switch msg.Role {
		case "system":
			if text := getMessageContent(msg.Content); text != "" {
				system = append(system, map[string]interface{}{"type": "text", "text": text})
			}
		case "tool":
			pendingToolResults = append(pendingToolResults, map[string]interface{}{
				"type": "tool_result",
				"content": []map[string]interface{}{
					{"type": "text", "text": getMessageContent(msg.Content)},
				},
			})
		case "assistant":
			flushTools()
			var blocks []map[string]interface{}
			if text := getMessageContent(msg.Content); text != "" {
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": text})
			}
			for _, tc := range msg.ToolCalls {
				blocks = append(blocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": json.RawMessage(orEmptyJSON(tc.Function.Arguments)),
				})
			}
			if len(blocks) > 0 {
				out = append(out, map[string]interface{}{"role": "assistant", "content": blocks})
			}
		default: // user
			flushTools()
			blocks := userContentBlocks(msg.Content)
			if len(blocks) > 0 {
				out = append(out, map[string]interface{}{"role": "user", "content": blocks})
			}
		}
	}
	flushTools()

	// Verdent expects the conversation to open with a user turn.
	if len(out) == 0 || out[0]["role"] != "user" {
		out = append([]map[string]interface{}{{
			"role":    "user",
			"content": []map[string]interface{}{{"type": "text", "text": "Hello"}},
		}}, out...)
	}
	return system, out
}

// userContentBlocks converts an OpenAI content field (string or parts array)
// to Verdent blocks; non-text parts (images) are skipped — free-model text
// only. ponytail: upgrade path = map image_url parts when Verdent documents
// vision blocks.
func userContentBlocks(raw json.RawMessage) []map[string]interface{} {
	text := getMessageContent(raw)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return []map[string]interface{}{{"type": "text", "text": text}}
}

func orEmptyJSON(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

// verdentDefaultSystem is used when the client sends no system prompt.
// ponytail: the reference router ships the full Verdent app prompt
// (verdent-system.json, ~40k chars heavy on docs for tools we don't send);
// this trimmed identity avoids tool hallucination.
var verdentDefaultSystem = []map[string]interface{}{{
	"type": "text",
	"text": "You are Verdent, an interactive agent designed by the Verdent team. You help users explore ideas, solve problems, and carry work through to completion. Be direct, accurate, and concise.",
}}

// ── Request building ──

type verdentPayload struct {
	Channel         string      `json:"channel"`
	Model           string      `json:"model"`
	ContextWindowTk int         `json:"context_window_tokens"`
	AgentName       string      `json:"agent_name"`
	ReactType       string      `json:"react_type"`
	Stream          bool        `json:"stream"`
	SessionID       string      `json:"session_id"`
	ConvID          string      `json:"conv_id"`
	ReactID         string      `json:"react_id"`
	MaxTokens       int         `json:"max_tokens"`
	Temperature     float64     `json:"temperature"`
	Thinking        interface{} `json:"thinking,omitempty"`
	Effort          string      `json:"effort,omitempty"`
	System          string      `json:"system"`
	Messages        string      `json:"messages"`
	Tools           string      `json:"tools,omitempty"`
	ToolChoice      interface{} `json:"tool_choice,omitempty"`
	Encrypt         bool        `json:"encrypt"`
	Env             interface{} `json:"env"`
	CustomTraceTags []string    `json:"custom_trace_tags_tmp"`
	Trace           interface{} `json:"custom_trace_metadata_tmp"`
	ModelCatalogVer string      `json:"model_catalog_version,omitempty"`
	IsEco           bool        `json:"is_eco"`
	IsAuto          bool        `json:"is_auto"`
	IsFree          bool        `json:"is_free"`
	IsLimitFree     bool        `json:"is_limit_free"`
	NativeAPI       bool        `json:"native_api"`
}

func buildVerdentPayload(model, effort, thinking, contextChoice, userQuery string, system, messages []map[string]interface{}) ([]byte, error) {
	sysBlocks := system
	if len(sysBlocks) == 0 {
		sysBlocks = verdentDefaultSystem
	}
	sysEnc, err := proxyEncode(sysBlocks)
	if err != nil {
		return nil, err
	}
	msgEnc, err := proxyEncode(messages)
	if err != nil {
		return nil, err
	}
	p := verdentPayload{
		Channel:         "deck",
		Model:           model,
		ContextWindowTk: modelContextTokens(model, contextChoice),
		AgentName:       "VerdentDeck",
		ReactType:       "Main Agent",
		Stream:          true,
		SessionID:       "session_" + uuidV4(),
		ConvID:          "conv_" + uuidV4(),
		ReactID:         "model_agent_" + uuidV4(),
		MaxTokens:       modelMaxOutputTokens(model),
		Temperature:     verdentDefaultTemperature,
		Thinking:        thinkingJSON(thinking),
		Effort:          effort,
		System:          sysEnc,
		Messages:        msgEnc,
		Encrypt:         true,
		Env:             verdentHostEnv(),
		CustomTraceTags: []string{},
		Trace: map[string]interface{}{
			"selected_model_id":                  model,
			"effective_model_id":                 model,
			"image_route_selected_model_is_byok": false,
			"conversation_scene":                 "worker",
			"conversation_prompt_source":         "user",
			"action_type":                        "text",
			"device_type":                        "pc",
			"os_type":                            verdentOSName(),
			"user_query":                         truncate(userQuery, 200),
		},
		ModelCatalogVer: currentCatalogVersion(),
		// is_free/is_limit_free stay false — the app sends false even for
		// -free models; server-side "true" routes into the exhausted
		// free pool and 30001s ("running out of credits").
		IsFree:      false,
		IsLimitFree: false,
	}
	return json.Marshal(p)
}

// thinkingJSON converts a thinking choice into the app's object shape;
// "" sends nothing (model default).
func thinkingJSON(choice string) interface{} {
	switch choice {
	case "enabled":
		return map[string]interface{}{"type": "enabled", "budget_tokens": 20000}
	case "disabled":
		return map[string]string{"type": "disabled"}
	}
	return nil
}

// ── Model fallback (cooldowns) ──

var (
	modelCooldownsMu sync.Mutex
	modelCooldowns   = map[string]time.Time{}
)

func markModelRateLimited(model string) {
	modelCooldownsMu.Lock()
	defer modelCooldownsMu.Unlock()
	modelCooldowns[model] = time.Now().Add(verdentModelCooldown)
	log.Printf("[Verdent] model %s rate-limited — cooldown %s", model, verdentModelCooldown)
}

func noteModelOK(model string) {
	modelCooldownsMu.Lock()
	defer modelCooldownsMu.Unlock()
	delete(modelCooldowns, model)
}

func modelBlocked(model string) bool {
	modelCooldownsMu.Lock()
	defer modelCooldownsMu.Unlock()
	until, ok := modelCooldowns[model]
	return ok && time.Now().Before(until)
}

// cooldownSnapshot powers the dashboard Models card.
func cooldownSnapshot() map[string]int64 {
	modelCooldownsMu.Lock()
	defer modelCooldownsMu.Unlock()
	out := make(map[string]int64, len(modelCooldowns))
	for m, until := range modelCooldowns {
		if ms := time.Until(until).Milliseconds(); ms > 0 {
			out[m] = ms
		}
	}
	return out
}

// modelOrder returns the fallback chain for a request: the requested model
// (alias-resolved) first, then the rest of the free lineup.
func modelOrder(requested string) []string {
	lineup := verdentModelIDs()
	resolved := resolveModelAlias(requested)
	if resolved == "" || findString(lineup, resolved) < 0 {
		return lineup // unknown or empty model → plain lineup order
	}
	out := []string{resolved}
	for _, m := range lineup {
		if m != resolved {
			out = append(out, m)
		}
	}
	return out
}

func findString(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// ── sendToVerdent — the upstream seam ──

// sendToVerdent sends a chat request to Verdent, calling onContent for each
// text delta and onReasoning (may be nil) for each thinking delta. Failover:
// auth errors rotate the account, rate limits cool the model down and move
// to the next one. All of it happens before the first forwarded delta, so
// the client never sees a half-started attempt. Stall protection comes from
// the request context (handlers pass cfg.Timeout).
func (s *Server) sendToVerdent(ctx context.Context, prompt string, opts sendOpts, onContent, onReasoning func(string)) (string, string, error) {
	system, messages := verdentMessagesFor(opts, prompt)
	if s.cfg.LogLevel == "debug" {
		plain, _ := json.Marshal(messages)
		log.Printf("[TRACE][payload] plaintext valid_utf8=%v bytes=%d preview=%q",
			utf8.Valid(plain), len(plain), truncate(string(plain), 120))
	}

	var lastErr error
	for _, model := range modelOrder(opts.model) {
		if modelBlocked(model) {
			continue
		}
		effort := mapEffort(model, opts.reasoningEffort)
		if effort == "" {
			effort = mapEffort(model, s.accounts.ModelSettingFor(model).Effort)
		}
		thinking, contextChoice := opts.thinking, opts.context
		if thinking == "" {
			thinking = s.accounts.ModelSettingFor(model).Thinking
		}
		if contextChoice == "" {
			contextChoice = s.accounts.ModelSettingFor(model).Context
		}
		modelDone := false // rate-limit hit → move on to the next model
		for attempt := 0; attempt < verdentMaxAttempts && !modelDone; attempt++ {
			acc := s.accounts.pick(attempt)
			if acc == nil {
				if lastErr == nil {
					lastErr = fmt.Errorf("no active Verdent accounts — add a JWT via the dashboard or VERDENT_TOKEN")
				}
				break
			}

			emitted := &emittedFlag{}
			content, reasoning, err := s.doVerdentRequest(ctx, acc, model, effort, opts.thinking, opts.context, prompt, system, messages,
				emitted.wrap(onContent), emitted.wrap(onReasoning))

			switch {
			case err == nil:
				noteModelOK(model)
				s.accounts.noteOK(acc)
				return content, reasoning, nil
			case emitted.flag:
				// Client already saw deltas from this attempt — no invisible retry.
				return "", "", err
			case errors.Is(err, errVerdentAuth):
				lastErr = err
				log.Printf("[Verdent] account %s rejected — rotating", maskJWT(acc.JWT))
				s.accounts.disableAccount(acc)
				// next attempt picks the next account
			case errors.Is(err, errVerdentRateLimit):
				lastErr = err
				markModelRateLimited(model)
				modelDone = true
			case attempt+1 < verdentMaxAttempts:
				lastErr = err
				select {
				case <-ctx.Done():
					return "", "", ctx.Err()
				case <-time.After(time.Duration(verdentRetryBaseMs*(attempt+1)) * time.Millisecond):
				}
			default:
				lastErr = err
			}
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(verdentModelSwitchDelay):
		}
	}
	return "", "", lastErr
}

// verdentMessagesFor builds the Verdent message list: structured client
// messages when available, otherwise session history + the prompt as a
// single user turn.
func verdentMessagesFor(opts sendOpts, prompt string) (system []map[string]interface{}, messages []map[string]interface{}) {
	if len(opts.clientMessages) > 0 {
		return openaiToVerdent(opts.clientMessages)
	}
	msgs := []map[string]interface{}{}
	for _, m := range opts.messages {
		raw, _ := json.Marshal(m)
		var probe struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &probe) == nil && (probe.Role == "user" || probe.Role == "assistant") {
			blocks := userContentBlocks(probe.Content)
			if len(blocks) > 0 {
				msgs = append(msgs, map[string]interface{}{"role": probe.Role, "content": blocks})
			}
		}
	}
	msgs = append(msgs, map[string]interface{}{
		"role":    "user",
		"content": []map[string]interface{}{{"type": "text", "text": prompt}},
	})
	return nil, msgs
}

// emittedFlag marks whether any delta was forwarded to the client. Once set,
// a failure can no longer be retried invisibly.
type emittedFlag struct{ flag bool }

func (e *emittedFlag) wrap(f func(string)) func(string) {
	if f == nil {
		return nil
	}
	return func(chunk string) {
		if chunk != "" {
			e.flag = true
		}
		f(chunk)
	}
}

// ── HTTP + SSE ──

// verdentProxyTransport returns the outbound transport: direct by default
// (ignores inherited HTTP(S)_PROXY env — a local mitm/proxy in the launch
// shell must not silently break upstream TLS), or an explicit VERDENT_PROXY.
func verdentProxyTransport() http.RoundTripper {
	t := &http.Transport{
		MaxIdleConns:        10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if p := strings.TrimSpace(os.Getenv("VERDENT_PROXY")); p != "" {
		if u, err := url.Parse(p); err == nil && u.Host != "" {
			t.Proxy = http.ProxyURL(u)
			log.Printf("[Verdent] outbound via proxy %s", u.Redacted())
		}
	}
	return t
}

var verdentHTTPClient = &http.Client{Transport: verdentProxyTransport()}

func (s *Server) doVerdentRequest(ctx context.Context, acc *VerdentAccount, model, effort, thinking, contextChoice, userQuery string,
	system, messages []map[string]interface{}, onContent, onReasoning func(string)) (string, string, error) {

	body, err := buildVerdentPayload(model, effort, thinking, contextChoice, userQuery, system, messages)
	if err != nil {
		return "", "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", verdentUpstreamURL, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+acc.JWT)
	h.Set("Cookie", "token="+acc.JWT)
	h.Set("verdent-proxy-beta", verdentProxyBeta)
	h.Set("agent_type", "ts_agent")
	h.Set("X-Version-Code", verdentVersion)
	h.Set("X-Verdent-Version", verdentVersion)
	h.Set("X-Verdent-Device-Id", acc.DeviceID)
	h.Set("OS", "Windows 11 Pro")
	h.Set("CPU-Arch", runtime.GOARCH)
	h.Set("Device-Model", "AMD64")
	h.Set("X-Device-ID", acc.DeviceID)
	h.Set("X-Team-ID", "0")
	h.Set("X-Device-Type", "pc")
	h.Set("X-OS-Type", verdentOSName())

	if s.cfg.LogLevel == "debug" {
		log.Printf("[DEBUG] Verdent request: model=%s account=%s bytes=%d", model, maskJWT(acc.JWT), len(body))
	}

	resp, err := verdentHTTPClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("verdent connection error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		errText, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		if cls := classifyVerdentError(resp.StatusCode, errText); cls != nil {
			return "", "", cls
		}
		return "", "", fmt.Errorf("verdent error %d: %s", resp.StatusCode, truncate(string(errText), 200))
	}

	return parseVerdentStream(resp.Body, s.cfg.LogLevel, onContent, onReasoning)
}

// classifyVerdentError maps the upstream JSON error body ({error_code,
// need_retry, msg}) to sentinel errors; nil when nothing special matches.
func classifyVerdentError(status int, body []byte) error {
	var e struct {
		ErrorCode int    `json:"error_code"`
		Code      int    `json:"code"`
		NeedRetry bool   `json:"need_retry"`
		Msg       string `json:"msg"`
	}
	if json.Unmarshal(body, &e) != nil {
		return nil
	}
	code := e.ErrorCode
	if code == 0 {
		code = e.Code
	}
	if status == 401 || code == 10006 {
		return errVerdentAuth
	}
	if status == 429 {
		return fmt.Errorf("%w: %s", errVerdentRateLimit, orDefault(e.Msg, "rate limited"))
	}
	// 30001: free window / credits exhausted — same failover path as 429
	// (cool the model down, try the next one).
	if code == 30001 || strings.Contains(strings.ToLower(e.Msg), "credits") {
		return fmt.Errorf("%w: %s", errVerdentRateLimit, orDefault(e.Msg, "out of credits"))
	}
	return nil
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// parseVerdentStream reads the SSE body: `data:` lines carry JSON events in
// Anthropic's shape (message_start, content_block_*, message_delta,
// message_stop) plus a bare `data: [DONE]` terminator.
func parseVerdentStream(body io.Reader, logLevel string, onContent, onReasoning func(string)) (string, string, error) {
	reader := bufio.NewReaderSize(body, 64*1024)
	var contentBuf, reasoningBuf strings.Builder
	var buffer string

	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			buffer += line
			lines := strings.Split(buffer, "\n")
			buffer = lines[len(lines)-1]
			for _, l := range lines[:len(lines)-1] {
				trimmed := strings.TrimSpace(l)
				if trimmed == "" || !strings.HasPrefix(trimmed, "data:") {
					continue
				}
				data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if data == "[DONE]" {
					content, reasoning := contentBuf.String(), reasoningBuf.String()
					if !utf8.ValidString(content) {
						log.Printf("[TRACE][upstream-final] INVALID UTF-8 in assembled content: %q", truncate(content, 100))
					}
					if n := strings.Count(content, string(rune(0xFFFD))); n > 0 {
						log.Printf("[TRACE][egress] %d U+FFFD in final content (upstream sent replacement chars)", n)
					}
					return content, reasoning, nil
				}
				if logLevel == "debug" {
					log.Printf("[DEBUG] Verdent SSE: %s", truncate(data, 300))
				}
				textDelta, thinkDelta, streamErr := feedVerdentEvent(data, onContent, onReasoning)
				if textDelta != "" {
					contentBuf.WriteString(textDelta)
				}
				if thinkDelta != "" {
					reasoningBuf.WriteString(thinkDelta)
				}
				if streamErr != nil {
					return contentBuf.String(), reasoningBuf.String(), streamErr
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				// Stream ended without [DONE] — process the tail, accept what we have.
				if data := strings.TrimSpace(buffer); data != "" {
					data = strings.TrimSpace(strings.TrimPrefix(data, "data:"))
					if data != "" && data != "[DONE]" {
						feedVerdentEvent(data, onContent, onReasoning)
					}
				}
				return contentBuf.String(), reasoningBuf.String(), nil
			}
			return "", "", fmt.Errorf("verdent stream read error: %w", err)
		}
	}
}

// feedVerdentEvent dispatches one SSE data payload. Returns the text/thinking
// deltas it produced (for buffering) and a sentinel error when the stream
// reports an upstream failure.
func feedVerdentEvent(data string, onContent, onReasoning func(string)) (textDelta, thinkDelta string, err error) {
	var ev struct {
		Type  string `json:"type"`
		Delta *struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
		// Error payloads (HTTP-style body delivered inside the stream).
		ErrorCode int    `json:"error_code"`
		Code      int    `json:"code"`
		NeedRetry bool   `json:"need_retry"`
		Msg       string `json:"msg"`
	}
	if json.Unmarshal([]byte(data), &ev) != nil {
		return "", "", nil // not JSON — ignore
	}

	switch {
	case ev.Type == "content_block_delta" && ev.Delta != nil:
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != "" {
				if !utf8.ValidString(ev.Delta.Text) {
					log.Printf("[TRACE][upstream-delta] INVALID UTF-8 text_delta: %q", truncate(ev.Delta.Text, 80))
				}
				if strings.ContainsRune(ev.Delta.Text, 0xFFFD) {
					log.Printf("[TRACE][upstream-delta] U+FFFD inside text_delta: %q", truncate(ev.Delta.Text, 80))
				}
				if onContent != nil {
					onContent(ev.Delta.Text)
				}
				return ev.Delta.Text, "", nil
			}
		case "thinking_delta":
			if ev.Delta.Thinking != "" {
				if !utf8.ValidString(ev.Delta.Thinking) {
					log.Printf("[TRACE][upstream-delta] INVALID UTF-8 thinking_delta: %q", truncate(ev.Delta.Thinking, 80))
				}
				if onReasoning != nil {
					onReasoning(ev.Delta.Thinking)
				}
				return "", ev.Delta.Thinking, nil
			}
		}
	case ev.ErrorCode != 0 || ev.Code != 0 || ev.Type == "error":
		code := ev.ErrorCode
		if code == 0 {
			code = ev.Code
		}
		if code == 10006 {
			return "", "", errVerdentAuth
		}
		msg := ev.Msg
		if msg == "" {
			msg = fmt.Sprintf("verdent stream error code %d", code)
		}
		if code == 30001 {
			return "", "", fmt.Errorf("%w: %s", errVerdentRateLimit, msg)
		}
		return "", "", fmt.Errorf("verdent stream error: %s", msg)
	}
	return "", "", nil
}

// maskJWT shortens a JWT for logs/dashboard: first 8 + … + last 4.
func maskJWT(t string) string {
	if len(t) <= 14 {
		return "***"
	}
	return t[:8] + "…" + t[len(t)-4:]
}

// jwtLabel extracts a human-readable account label from the JWT payload
// (email / name / sub), falling back to the masked token.
func jwtLabel(token string) string {
	if payload, err := jwtPayload(token); err == nil {
		for _, key := range []string{"email", "name", "sub", "user_name"} {
			if v, ok := payload[key].(string); ok && v != "" {
				return v
			}
		}
	}
	return maskJWT(token)
}
