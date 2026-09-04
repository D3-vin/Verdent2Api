// main.go — HTTP server, config, OpenAI-compatible routes, dashboard.
// Replaces main.js (Express) entirely. Captcha logic is in-process (no pipe IPC).
package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ── Config ──

type Config struct {
	Host              string
	Port              string
	AuthEnabled       bool
	AuthToken         string
	VerdentToken      string
	VerdentTokensFile string
	LogLevel          string
	LogFormat         string
	Timeout           time.Duration
}

func loadConfig() *Config {
	cfg := &Config{
		Host:              envOr("HOST", "0.0.0.0"),
		Port:              envOr("PORT", "5084"),
		AuthEnabled:       true,
		AuthToken:         envOr("AUTH_TOKEN", "d3vin"),
		VerdentToken:      envOr("VERDENT_TOKEN", ""),
		VerdentTokensFile: envOr("VERDENT_TOKENS_FILE", "verdent_tokens.txt"),
		LogLevel:          envOr("LOG_LEVEL", "info"),
		LogFormat:         envOr("LOG_FORMAT", "text"),
		Timeout:           time.Duration(envIntOr("TIMEOUT", 300000)) * time.Millisecond,
	}
	return cfg
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		fmt.Sscanf(v, "%d", &n)
		if n > 0 {
			return n
		}
	}
	return def
}

// ── Server ──

type Server struct {
	cfg           *Config
	accounts      *AccountPool
	conversations *ConversationStore
}

// ── OpenAI response types ──

type openAIDelta struct {
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type openAIMessage struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type openAIChoice struct {
	Index        int            `json:"index"`
	Delta        *openAIDelta   `json:"delta,omitempty"`
	Message      *openAIMessage `json:"message,omitempty"`
	FinishReason *string        `json:"finish_reason"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type openAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
}

func newChunk(content, model, requestID string, finish *string) openAIResponse {
	return openAIResponse{
		ID:      "chatcmpl-" + requestID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openAIChoice{{
			Index:        0,
			Delta:        &openAIDelta{Content: content},
			FinishReason: finish,
		}},
	}
}

func newReasoningChunk(reasoning, model, requestID string) openAIResponse {
	return openAIResponse{
		ID:      "chatcmpl-" + requestID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openAIChoice{{
			Index:        0,
			Delta:        &openAIDelta{ReasoningContent: reasoning},
			FinishReason: nil,
		}},
	}
}

func newCompletion(content, reasoning, prompt, model, requestID string) openAIResponse {
	pt := estimateTokens(prompt)
	ct := estimateTokens(content)
	return openAIResponse{
		ID:      "chatcmpl-" + requestID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openAIChoice{{
			Index:        0,
			Message:      &openAIMessage{Role: "assistant", Content: content, ReasoningContent: reasoning},
			FinishReason: strPtr("stop"),
		}},
		Usage: &openAIUsage{
			PromptTokens:     pt,
			CompletionTokens: ct,
			TotalTokens:      pt + ct,
		},
	}
}

func formatOpenAIError(msg, errType string) map[string]interface{} {
	if errType == "" {
		errType = "api_error"
	}
	return map[string]interface{}{
		"error": map[string]interface{}{
			"message": msg,
			"type":    errType,
			"code":    nil,
			"param":   nil,
		},
	}
}

func strPtr(s string) *string { return &s }

// statusFromError maps a Z.AI/bridge error string to an HTTP status code.
func statusFromError(errMsg string) int {
	switch {
	case strings.Contains(errMsg, "401"):
		return 401
	case strings.Contains(errMsg, "403"):
		return 403
	case strings.Contains(errMsg, "429"):
		return 429
	case strings.Contains(errMsg, "400"):
		return 400
	default:
		return 500
	}
}

// ── Chat request ──

type chatRequest struct {
	Model           string            `json:"model"`
	Messages        []json.RawMessage `json:"messages"`
	Stream          *bool             `json:"stream"`
	DeepThink       *bool             `json:"deepThink"`
	Search          *bool             `json:"search"`
	WebSearch       *bool             `json:"webSearch"`
	Reasoning       *bool             `json:"reasoning"`
	Thinking        json.RawMessage   `json:"thinking"`
	ReasoningEffort string            `json:"reasoning_effort"`
	Tools           []json.RawMessage `json:"tools,omitempty"`
}

func (r *chatRequest) streamEnabled() bool {
	if r.Stream == nil {
		return true // default stream=true like the original
	}
	return *r.Stream
}

// ── Middleware ──

func (s *Server) withAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.AuthEnabled {
			h(w, r)
			return
		}
		provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if provided == "" || provided == "Bearer" {
			provided = strings.TrimSpace(r.Header.Get("x-api-key")) // Anthropic-style clients
		}
		if provided != s.cfg.AuthToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"type": "error",
				"error": map[string]interface{}{
					"type":    "authentication_error",
					"message": "Invalid or missing authentication token",
				},
			})
			return
		}
		h(w, r)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-Id, X-Fresh-Session")
		if r.Method == "OPTIONS" {
			w.WriteHeader(200)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── Routes ──

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/status", s.handleStatus)

	mux.HandleFunc("/v1/models", s.withAuth(s.handleV1Models))
	mux.HandleFunc("/models", s.withAuth(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.withAuth(s.handleChatCompletions))
	mux.HandleFunc("/v1/messages", s.withAuth(s.handleAnthropicMessages))
	mux.HandleFunc("/v1/responses", s.withAuth(s.handleResponses))

	mux.HandleFunc("/prompt", s.withAuth(s.handlePrompt))

	// Dashboard API (behind the same AUTH_TOKEN the SPA gets injected).
	mux.HandleFunc("/api/status", s.withAuth(s.apiStatus))
	mux.HandleFunc("/api/accounts", s.withAuth(s.apiAccounts))
	mux.HandleFunc("/api/accounts/add", s.withAuth(s.apiAccountsAdd))
	mux.HandleFunc("/api/accounts/remove", s.withAuth(s.apiAccountsRemove))
	mux.HandleFunc("/api/accounts/select", s.withAuth(s.apiAccountsSelect))
	mux.HandleFunc("/api/settings", s.withAuth(s.apiSettings))
	mux.HandleFunc("/api/quota/refresh", s.withAuth(s.apiQuotaRefresh))
	mux.HandleFunc("/api/auth/start", s.withAuth(s.apiAuthStart))

	// OAuth redirect target — the browser carries no API token here.
	mux.HandleFunc("/auth/callback", s.handleAuthCallback)

	return corsMiddleware(mux)
}

// ── Handlers ──

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	host := r.Host
	if host == "" {
		host = "localhost:" + s.cfg.Port
	}
	html := dashboardHTML
	html = strings.ReplaceAll(html, "__HOST__", host)
	html = strings.ReplaceAll(html, "__AUTH_TOKEN__", s.cfg.AuthToken)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	count, _, activeJWT, activeUser := s.accounts.StatusSnapshot()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"connected":      count > 0,
		"userName":       activeUser,
		"activeJwt":      activeJWT,
		"accounts":       count,
		"defaultModel":   s.accounts.DefaultModel(),
		"activeSessions": s.conversations.count(),
		"cooldowns":      cooldownSnapshot(),
		"port":           s.cfg.Port,
	})
}

func (s *Server) handleV1Models(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	models := modelCatalog()

	// Pass-through: exactly what the service catalog serves, no alias
	// mapping.
	data := make([]map[string]interface{}, 0, len(models))
	addModel := func(id, name string, m ModelInfo) {
		entry := map[string]interface{}{
			"id":              id,
			"object":          "model",
			"created":         now,
			"owned_by":        "verdent",
			"display_name":    name,
			"description":     m.Description,
			"free":            m.IsLimitFree,
			"cost_multiplier": m.CostMultiplier,
		}
		if len(m.ContextWindows) > 0 {
			ctxCfg := map[string]interface{}{}
			for _, cw := range m.ContextWindows {
				ctxCfg[cw.Display] = cw.Tokens
			}
			entry["context_config"] = ctxCfg
			entry["max_output_tokens"] = m.MaxOutputTokens
		}
		if m.CreditsDescription != "" {
			entry["credits_description"] = m.CreditsDescription
		}
		if len(m.EffortLevels) > 0 {
			entry["effort_levels"] = m.EffortLevels
		}
		data = append(data, entry)
	}
	for _, m := range models {
		addModel(m.ID, m.Name, m)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models := modelCatalog()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"models":       ids,
		"currentModel": s.accounts.DefaultModel(),
	})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// Limit body size (50mb like the original)
	r.Body = http.MaxBytesReader(w, r.Body, 50*1024*1024)

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(formatOpenAIError("failed to read body: "+err.Error(), "invalid_request_error"))
		return
	}
	if !utf8.Valid(raw) {
		// Go's JSON decoder silently coerces invalid bytes to U+FFFD — the
		// model would see «по умол��чанию»-style garbage. Fail loudly instead.
		log.Printf("[TRACE][ingress] REJECTED non-UTF-8 body from %s (%d bytes): %q",
			r.RemoteAddr, len(raw), truncate(string(raw), 160))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(formatOpenAIError(
			"request body is not valid UTF-8 (client console encoding?) — send UTF-8 (chcp 65001 / PYTHONIOENCODING=utf-8)",
			"invalid_request_error"))
		return
	}
	var req chatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(formatOpenAIError("invalid JSON body: "+err.Error(), "invalid_request_error"))
		return
	}

	if len(req.Messages) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(formatOpenAIError("messages is required and must be an array", "invalid_request_error"))
		return
	}

	model := req.Model
	if model == "" {
		model = s.accounts.DefaultModel()
	} else {
		model = resolveModelAlias(model)
		if model == "" {
			model = s.accounts.DefaultModel()
		}
	}

	sessionID := r.Header.Get("X-Session-Id")
	if sessionID == "" {
		sessionID = "default"
	}
	fresh := r.Header.Get("X-Fresh-Session") == "true"
	conv := s.conversations.getOrCreate(sessionID, fresh)

	requestID := generateID()
	prompt := messagesToPrompt(req.Messages)

	if s.cfg.LogLevel == "debug" {
		for i, m := range req.Messages {
			log.Printf("[TRACE][ingress] msg[%d] valid_utf8=%v content=%q", i,
				utf8.ValidString(getMessageContent(json.RawMessage(m))), truncate(getMessageContent(m), 100))
		}
	}

	opts := sendOpts{
		model:           model,
		reasoningEffort: req.ReasoningEffort,
		chatID:          conv.chatID,
		messages:        conv.messages,
		clientMessages:  req.Messages,
	}

	// Tool calling: streaming path intercepts <<<TOOL_CALL>>> live,
	// non-streaming buffers and parses after the fact.
	if len(req.Tools) > 0 {
		if req.streamEnabled() {
			s.handleChatToolsStream(w, r, req, prompt, model, requestID, conv, opts)
		} else {
			s.handleChatWithTools(w, r, req, prompt, model, requestID, conv, opts)
		}
		return
	}

	if req.streamEnabled() {
		s.handleChatStream(w, r, prompt, model, requestID, opts, conv)
	} else {
		s.handleChatNonStream(w, r, prompt, model, requestID, opts, conv)
	}
}

// handleChatWithTools processes non-streaming requests with tools.
// Buffers full response, parses for tool markers, returns either
// tool_calls or normal text in OpenAI format.
func (s *Server) handleChatWithTools(w http.ResponseWriter, r *http.Request, req chatRequest, prompt, model, requestID string, conv *Conversation, opts sendOpts) {
	// Convert messages: prepend tools system prompt, convert tool/assistant roles
	convertedMsgs := convertToolMessages(req.Messages, req.Tools)
	// Recompute prompt from converted messages for signature
	convertedPrompt := messagesToPrompt(convertedMsgs)
	opts.clientMessages = convertedMsgs

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	// Buffered path: take the authoritative parser content, not deltas
	// (edit_content rewrites would garble delta concatenation).
	content, _, err := s.sendToVerdent(ctx, convertedPrompt, opts, nil, nil)

	if err != nil {
		log.Printf("[Tools] Error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusFromError(err.Error()))
		json.NewEncoder(w).Encode(formatOpenAIError(err.Error(), "api_error"))
		return
	}

	// Parse for tool calls
	toolCalls := parseToolCalls(content, req.Tools)
	if len(toolCalls) > 0 {
		cleanContent := stripToolContent(content)

		// Build tool call entries
		type tcFunc struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		type tcEntry struct {
			Index_   int    `json:"index"`
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function tcFunc `json:"function"`
		}

		entries := make([]tcEntry, len(toolCalls))
		for i, c := range toolCalls {
			entries[i] = tcEntry{Index_: i, ID: c.ID, Type: c.Type}
			entries[i].Function.Name = c.Function.Name
			entries[i].Function.Arguments = c.Function.Arguments
		}

		if req.streamEnabled() {
			// SSE streaming format for tool_calls (what opencode/OpenAI clients expect)
			flusher, ok := w.(http.Flusher)
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				writeToolCallsJSON(w, requestID, model, cleanContent, entries)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(200)

			// Chunk 1: role + tool_calls
			chunk1 := map[string]interface{}{
				"id":      "chatcmpl-" + requestID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   model,
				"choices": []map[string]interface{}{{
					"index": 0,
					"delta": map[string]interface{}{
						"role":       "assistant",
						"content":    nil,
						"tool_calls": entries,
					},
					"finish_reason": nil,
				}},
			}
			data1, _ := json.Marshal(chunk1)
			fmt.Fprintf(w, "data: %s\n\n", data1)
			flusher.Flush()

			// Chunk 2: finish_reason
			finish := "tool_calls"
			chunk2 := map[string]interface{}{
				"id":      "chatcmpl-" + requestID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   model,
				"choices": []map[string]interface{}{{
					"index":         0,
					"delta":         map[string]interface{}{},
					"finish_reason": finish,
				}},
			}
			data2, _ := json.Marshal(chunk2)
			fmt.Fprintf(w, "data: %s\n\n", data2)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
		} else {
			// Non-streaming JSON
			w.Header().Set("Content-Type", "application/json")
			writeToolCallsJSON(w, requestID, model, cleanContent, entries)
		}
		log.Printf("[Tools] Returned %d tool calls", len(toolCalls))
		return
	}

	// No tool calls found — return content as-is (text or raw JSON).
	// Previously cleanFallbackContent replaced JSON with a generic message,
	// but that hid legitimate text responses from the user.
	// Don't leak raw contract markers though: strip complete blocks.
	if strings.Contains(content, agentToolCallStart) {
		preview := content
		if len(preview) > 1500 {
			preview = preview[:1500] + "...(truncated)"
		}
		log.Printf("[Tools] markers present but no valid tool call parsed — model output:\n%s", preview)
		content = stripAgentToolCallBlocks(content)
	}
	if req.streamEnabled() {
		// Client expects SSE — send text as stream chunks
		flusher, ok := w.(http.Flusher)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(newCompletion(content, "", prompt, model, requestID))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(200)

		writeSSE := func(v interface{}) {
			data, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}

		// Split content into chunks to avoid oversized SSE events
		const chunkSize = 2000
		for i := 0; i < len(content); {
			end := runeChunkEnd(content, i, i+chunkSize)
			writeSSE(newChunk(content[i:end], model, requestID, nil))
			i = end
		}

		// Final chunk with finish_reason
		writeSSE(newChunk("", model, requestID, strPtr("stop")))
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	} else {
		// Non-streaming JSON
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(newCompletion(content, "", prompt, model, requestID))
	}
	preview := content
	if len(preview) > 500 {
		preview = preview[:500] + "..."
	}
	log.Printf("[Tools] No tool calls in response, returning text (%d chars): %s", len(content), preview)
}

// writeToolCallsJSON writes a non-streaming JSON response with tool_calls.
func writeToolCallsJSON(w http.ResponseWriter, requestID, model, content string, entries interface{}) {
	resp := map[string]interface{}{
		"id":      "chatcmpl-" + requestID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{{
			"index": 0,
			"message": map[string]interface{}{
				"role":       "assistant",
				"content":    content,
				"tool_calls": entries,
			},
			"finish_reason": "tool_calls",
		}},
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request, prompt, model, requestID string, opts sendOpts, conv *Conversation) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher.Flush()

	var writeMu sync.Mutex
	writeSSE := func(v interface{}) {
		writeMu.Lock()
		defer writeMu.Unlock()
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	// Initial chunk
	writeSSE(newChunk("", model, requestID, nil))

	// Keepalive ticker (stopped + joined before return — no writes after close)
	keepAliveStop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				writeSSE(newChunk("", model, requestID, nil))
			case <-keepAliveStop:
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	finalContent, _, err := s.sendToVerdent(ctx, prompt, opts, func(chunk string) {
		writeSSE(newChunk(chunk, model, requestID, nil))
	}, func(chunk string) {
		writeSSE(newReasoningChunk(chunk, model, requestID))
	})

	if err != nil {
		log.Printf("[Stream] Error: %v", err)
		errJSON, _ := json.Marshal(map[string]interface{}{"error": map[string]string{"message": err.Error()}})
		writeMu.Lock()
		fmt.Fprintf(w, "data: %s\n\n", errJSON)
		writeMu.Unlock()
	}

	// Final chunk
	writeSSE(newChunk("", model, requestID, strPtr("stop")))
	writeMu.Lock()
	fmt.Fprintf(w, "data: [DONE]\n\n")
	writeMu.Unlock()
	flusher.Flush()

	close(keepAliveStop)
	wg.Wait()

	// History persistence (authoritative parser content, reasoning excluded)
	if persistHistoryEnabled() && err == nil && finalContent != "" {
		conv.messages = append(conv.messages,
			map[string]interface{}{"role": "user", "content": prompt},
			map[string]interface{}{"role": "assistant", "content": finalContent},
		)
	}
}

// handleChatToolsStream streams a tools request, intercepting
// <<<TOOL_CALL>>> blocks live and rewriting them into tool_calls deltas.
func (s *Server) handleChatToolsStream(w http.ResponseWriter, r *http.Request, req chatRequest, prompt, model, requestID string, conv *Conversation, opts sendOpts) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	convertedMsgs := convertToolMessages(req.Messages, req.Tools)
	convertedPrompt := messagesToPrompt(convertedMsgs)
	opts.clientMessages = convertedMsgs

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher.Flush()

	var writeMu sync.Mutex
	writeSSE := func(v interface{}) {
		writeMu.Lock()
		defer writeMu.Unlock()
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	emitToolCallDelta := func(tc map[string]interface{}) {
		writeSSE(map[string]interface{}{
			"id":      "chatcmpl-" + requestID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         map[string]interface{}{"tool_calls": []map[string]interface{}{tc}},
				"finish_reason": nil,
			}},
		})
	}

	writeSSE(newChunk("", model, requestID, nil))

	keepAliveStop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				writeSSE(newChunk("", model, requestID, nil))
			case <-keepAliveStop:
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	interceptor := newToolStreamInterceptor()
	toolCallEmitted := false

	finalContent, _, err := s.sendToVerdent(ctx, convertedPrompt, opts, func(chunk string) {
		// ponytail: edit_content shrinkage is absorbed by the SSE parser (no
		// deltas emitted until caught up), so the interceptor buffer can go
		// stale across a shrink — the end-of-stream parseToolCalls fallback
		// below recovers naked JSON, fences and truncated blocks.
		contentDelta, toolCalls, _ := interceptor.feed(chunk)
		if contentDelta != "" {
			writeSSE(newChunk(contentDelta, model, requestID, nil))
		}
		for _, tc := range toolCalls {
			emitToolCallDelta(tc)
			toolCallEmitted = true
		}
	}, func(chunk string) {
		writeSSE(newReasoningChunk(chunk, model, requestID))
	})

	if err != nil {
		log.Printf("[Stream Tools] Error: %v", err)
		writeSSE(formatOpenAIError(err.Error(), "api_error"))
		writeMu.Lock()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		writeMu.Unlock()
		close(keepAliveStop)
		wg.Wait()
		return
	}

	// Flush trailing text (only when no tool calls were emitted)
	if rem := interceptor.flushFinal(); rem != "" && !toolCallEmitted {
		writeSSE(newChunk(rem, model, requestID, nil))
	}

	// Safety net: full parser over the authoritative content — catches naked
	// JSON, fences and truncated marker blocks (the marker-only extractor
	// requires a complete end marker).
	if !toolCallEmitted {
		for _, tc := range parseToolCalls(finalContent, req.Tools) {
			emitToolCallDelta(map[string]interface{}{
				"index":    0,
				"id":       tc.ID,
				"type":     "function",
				"function": map[string]interface{}{"name": tc.Function.Name, "arguments": tc.Function.Arguments},
			})
			toolCallEmitted = true
		}
	}

	if toolCallEmitted {
		writeSSE(map[string]interface{}{
			"id":      "chatcmpl-" + requestID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "tool_calls",
			}},
		})
		log.Printf("[Stream Tools] tool_calls emitted")
	} else {
		writeSSE(newChunk("", model, requestID, strPtr("stop")))
	}
	writeMu.Lock()
	fmt.Fprintf(w, "data: [DONE]\n\n")
	writeMu.Unlock()
	flusher.Flush()

	close(keepAliveStop)
	wg.Wait()
}

func (s *Server) handleChatNonStream(w http.ResponseWriter, r *http.Request, prompt, model, requestID string, opts sendOpts, conv *Conversation) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	// Buffered path: authoritative parser content/reasoning, not deltas
	fullContent, fullReasoning, err := s.sendToVerdent(ctx, prompt, opts, nil, nil)

	if err != nil {
		log.Printf("[API] Error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusFromError(err.Error()))
		json.NewEncoder(w).Encode(formatOpenAIError(err.Error(), "api_error"))
		return
	}

	if persistHistoryEnabled() && fullContent != "" {
		conv.messages = append(conv.messages,
			map[string]interface{}{"role": "user", "content": prompt},
			map[string]interface{}{"role": "assistant", "content": fullContent},
		)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(newCompletion(fullContent, fullReasoning, prompt, model, requestID))
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 50*1024*1024)

	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
		return
	}
	if body.Prompt == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Prompt is required"})
		return
	}

	sessionID := r.Header.Get("X-Session-Id")
	if sessionID == "" {
		sessionID = "default"
	}
	fresh := r.Header.Get("X-Fresh-Session") == "true"
	conv := s.conversations.getOrCreate(sessionID, fresh)

	opts := sendOpts{
		model:    s.accounts.DefaultModel(),
		chatID:   conv.chatID,
		messages: conv.messages,
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	fullContent, _, err := s.sendToVerdent(ctx, body.Prompt, opts, nil, nil)

	if err != nil {
		log.Printf("[Prompt] Error: %v", err)
		w.WriteHeader(statusFromError(err.Error()))
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
		return
	}

	if persistHistoryEnabled() && fullContent != "" {
		conv.messages = append(conv.messages,
			map[string]interface{}{"role": "user", "content": body.Prompt},
			map[string]interface{}{"role": "assistant", "content": fullContent},
		)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"response": fullContent,
	})
}

// ── Helpers ──

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ── .env file loader (minimal: KEY=VALUE per line, # comments, quotes stripped) ──

func loadEnvFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // no .env file — that's fine
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, "`\"'")
		if os.Getenv(key) == "" { // env vars take precedence over .env
			os.Setenv(key, val)
		}
	}
}

// ── Banner ──

func printBanner() {
	cyan := "\033[36m"
	yellow := "\033[33m"
	magenta := "\033[35m"
	green := "\033[32m"
	reset := "\033[0m"

	fmt.Println()
	fmt.Println(cyan + "██╗   ██╗ ███████╗ ██████╗  ██████╗  ███████╗ ███╗   ██╗ ████████╗  ██████╗   █████╗  ██████╗  ██╗" + reset)
	fmt.Println(cyan + "██║   ██║ ██╔════╝ ██╔══██╗ ██╔══██╗ ██╔════╝ ████╗  ██║ ╚══██╔══╝ ╚════██╗  ██╔══██╗ ██╔══██╗ ██║" + reset)
	fmt.Println(cyan + "██║   ██║ █████╗   ██████╔╝ ██║  ██║ █████╗   ██╔██╗ ██║    ██║     █████╔╝  ███████║ ██████╔╝ ██║" + reset)
	fmt.Println(cyan + "╚██╗ ██╔╝ ██╔══╝   ██╔══██╗ ██║  ██║ ██╔══╝   ██║╚██╗██║    ██║    ██╔═══╝   ██╔══██║ ██╔═══╝  ██║" + reset)
	fmt.Println(cyan + " ╚████╔╝  ███████╗ ██║  ██║ ██████╔╝ ███████╗ ██║ ╚████║    ██║    ███████╗  ██║  ██║ ██║      ██║" + reset)
	fmt.Println(cyan + "  ╚═══╝   ╚══════╝ ╚═╝  ╚═╝ ╚═════╝  ╚══════╝ ╚═╝  ╚═══╝    ╚═╝    ╚══════╝  ╚═╝  ╚═╝ ╚═╝      ╚═╝" + reset)
	fmt.Println()
	fmt.Println(yellow + "  📧 Telegram:" + reset + " https://t.me/D3_vin")
	fmt.Println(magenta + "  👤 Author:" + reset + " @D3vin_dev")
	fmt.Println(green + "  🔗 GitHub:" + reset + " https://github.com/D3-vin/VERDENT2API")
	fmt.Println(cyan + "  📦 Version:" + reset + " 1.0.1")
	fmt.Println()
}

// ── Main ──

// Main is the server bootstrap invoked by cmd/server.
func Main() {
	loadEnvFile(".env")

	cfg := loadConfig()

	accounts := NewAccountPool(cfg.VerdentToken, cfg.VerdentTokensFile)
	count, _, activeJWT, _ := accounts.StatusSnapshot()

	srv := &Server{
		cfg:           cfg,
		accounts:      accounts,
		conversations: newConversationStore(),
	}
	accounts.onAdd = func(acc *VerdentAccount) { primeAccountMetadata(srv, acc) }

	// Startup banner
	printBanner()
	fmt.Printf(`
╔═══════════════════════════════════════════════════════════════╗
║           Verdent 2API (Go)                                    ║
╠═══════════════════════════════════════════════════════════════╣
║  Dashboard:     http://localhost:%s                          ║
║  OpenAI API:    http://localhost:%s/v1/chat/completions      ║
║  Accounts:      %d (active: %s)
║  Default model: %s
║  Auth Token:    %s
╚═══════════════════════════════════════════════════════════════╝
`, cfg.Port, cfg.Port, count, activeJWT, accounts.DefaultModel(), cfg.AuthToken)

	// Background: keep the live model catalog + account usage fresh. Both
	// loops retry every 15s while no account exists, then settle on 5 min.
	go srv.startCatalogRefresher()
	go func() {
		for {
			if srv.accounts.pick(0) == nil {
				time.Sleep(15 * time.Second)
				continue
			}
			srv.accounts.RefreshUsage()
			time.Sleep(5 * time.Minute)
		}
	}()

	addr := cfg.Host + ":" + cfg.Port
	log.Printf("Listening on %s", addr)
	if err := http.ListenAndServe(addr, srv.routes()); err != nil {
		log.Printf("Server error: %v", err)
		waitForEnter()
		os.Exit(1)
	}
}

// waitForEnter pauses before exit so fatal errors stay visible in the
// console window (e.g. when the .exe is double-clicked on Windows).
// Skipped when stdin is piped/redirected, so automation isn't blocked.
func waitForEnter() {
	stat, err := os.Stdin.Stat()
	if err != nil || (stat.Mode()&os.ModeCharDevice) == 0 {
		return // not interactive (piped/redirected) — don't block
	}
	fmt.Fprintln(os.Stderr, "\nPress Enter to exit...")
	bufio.NewReader(os.Stdin).ReadString('\n')
}
