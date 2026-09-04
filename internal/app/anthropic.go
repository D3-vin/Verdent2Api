// anthropic.go — Anthropic API compatibility layer for Claude Desktop integration.
// ponytail: /v1/messages only; reuses sendToVerdent + getMessageContent.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	systemReminderRe = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
)

// stripAnthropicNoise removes Desktop/CLI injected blocks GLM doesn't need (cuts ~30k chars).
func stripAnthropicNoise(s string) string {
	s = systemReminderRe.ReplaceAllString(s, "")
	if i := strings.Index(s, "x-anthropic-billing-header:"); i >= 0 {
		if j := strings.Index(s[i:], "\n"); j >= 0 {
			s = s[:i] + s[i+j+1:]
		} else {
			s = s[:i]
		}
	}
	return strings.TrimSpace(s)
}

// auxClaudeCodeMarkers — markers of Claude Code CLI auxiliary requests
// (suggestion of the next user input, "stepped away" recap). These must be
// answered TEXT-ONLY: a tool_use in such a response is rejected by the CLI
// ("No tools needed for suggestion" error tool_result) and poisons the main
// conversation history.
var auxClaudeCodeMarkers = []string{
	"[SUGGESTION MODE:",
	"The user stepped away and is coming back. Recap in under 40 words",
}

// isAuxiliaryClaudeCodeRequest reports whether the LAST user message of the
// request is one of the CLI auxiliary prompts (checked on RAW messages — the
// marker sits in plain user content, outside system-reminders).
func isAuxiliaryClaudeCodeRequest(msgs []json.RawMessage) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(msgs[i], &m) != nil {
			continue
		}
		if m.Role != "user" {
			continue
		}
		text := getMessageContent(m.Content)
		for _, marker := range auxClaudeCodeMarkers {
			if strings.Contains(text, marker) {
				return true
			}
		}
		return false // only the last user message matters
	}
	return false
}

// sanitizeAnthropicMessages strips noise and caps huge system prompts before the upstream.
func sanitizeAnthropicMessages(msgs []json.RawMessage) []json.RawMessage {
	const maxSystem = 8000
	var out []json.RawMessage
	for _, raw := range msgs {
		var msg map[string]interface{}
		if json.Unmarshal(raw, &msg) != nil {
			out = append(out, raw)
			continue
		}
		role, _ := msg["role"].(string)
		content, _ := msg["content"].(string)
		content = stripAnthropicNoise(content)
		if role == "system" && len(content) > maxSystem {
			// Keep head AND tail: environment/cwd info usually lives at the end
			head := maxSystem * 3 / 4
			tail := maxSystem - head
			log.Printf("[Anthropic] truncating system %d → %d chars (head+tail)", len(content), maxSystem)
			content = content[:head] + "\n...[truncated]...\n" + content[len(content)-tail:]
		}
		if content == "" {
			continue
		}
		msg["content"] = content
		b, _ := json.Marshal(msg)
		out = append(out, b)
	}
	return out
}

// resolveGLMModel resolves the requested model to a lineup id. No mapping
// table: the catalog is passed through as the service serves it; anything
// the lineup doesn't know falls back to the default model.
func resolveGLMModel(model string) string {
	clean := strings.TrimSuffix(model, "[1m]")
	for _, id := range modelIDs() {
		if strings.EqualFold(id, clean) {
			return id
		}
	}
	def := modelIDs()
	if len(def) > 0 {
		log.Printf("[Anthropic] unknown model %q, fallback %s", model, def[0])
		return def[0]
	}
	return "glm-5.3-flash-free"
}

// ponytail: Desktop sends 59 tools — cap keeps tool framing under ~15k chars.
const maxAnthropicTools = 30

// coreClaudeTools are kept FIRST when capping — dropping Write/Edit/Bash
// makes the model call tools that the bridge then rejects as "unknown".
var coreClaudeTools = []string{
	"Read", "Write", "Edit", "Bash", "Glob", "Grep", "Task", "TodoWrite",
	"WebFetch", "WebSearch", "BashOutput", "KillShell", "NotebookEdit",
	"SlashCommand", "ExitPlanMode",
}

func toolNameOf(raw json.RawMessage) string {
	var t struct {
		Name string `json:"name"`
	}
	json.Unmarshal(raw, &t)
	return t.Name
}

func limitAnthropicTools(tools []json.RawMessage) []json.RawMessage {
	if len(tools) <= maxAnthropicTools {
		return tools
	}
	// Keep core tools in priority order, then fill the remaining slots
	// with the rest (original order).
	priority := make(map[string]int, len(coreClaudeTools))
	for i, n := range coreClaudeTools {
		priority[n] = i
	}
	var core, rest []json.RawMessage
	for _, raw := range tools {
		if _, ok := priority[toolNameOf(raw)]; ok {
			core = append(core, raw)
		} else {
			rest = append(rest, raw)
		}
	}
	sort.SliceStable(core, func(i, j int) bool {
		return priority[toolNameOf(core[i])] < priority[toolNameOf(core[j])]
	})
	out := append(core, rest...)
	if len(out) > maxAnthropicTools {
		out = out[:maxAnthropicTools]
	}
	log.Printf("[Anthropic] capping tools %d → %d (core prioritized)", len(tools), len(out))
	return out
}

type anthropicRequest struct {
	Model       string            `json:"model"`
	Messages    []json.RawMessage `json:"messages"`
	MaxTokens   int               `json:"max_tokens"`
	Stream      bool              `json:"stream"`
	System      json.RawMessage   `json:"system,omitempty"`
	Temperature *float64          `json:"temperature,omitempty"`
	TopP        *float64          `json:"top_p,omitempty"`
	Tools       []json.RawMessage `json:"tools,omitempty"`
	WebSearch   *bool             `json:"webSearch,omitempty"`
	DeepThink   *bool             `json:"deepThink,omitempty"`
}

// anthropicToOpenAIMessages converts Anthropic messages to OpenAI shape (string content).
func anthropicToOpenAIMessages(msgs []json.RawMessage, system json.RawMessage) []json.RawMessage {
	var out []json.RawMessage
	if text := getMessageContent(system); text != "" {
		raw, _ := json.Marshal(map[string]string{"role": "system", "content": text})
		out = append(out, raw)
	}
	for _, m := range msgs {
		out = append(out, convertAnthropicMessage(m)...)
	}
	return out
}

func convertAnthropicMessage(raw json.RawMessage) []json.RawMessage {
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil || len(msg.Content) == 0 {
		return nil
	}

	// Plain string content
	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		b, _ := json.Marshal(map[string]string{"role": msg.Role, "content": s})
		return []json.RawMessage{b}
	}

	// Content block array
	var blocks []map[string]interface{}
	if json.Unmarshal(msg.Content, &blocks) != nil {
		return nil
	}

	var textParts []string
	var toolCalls []map[string]interface{}
	var out []json.RawMessage

	flushAssistant := func() {
		if len(toolCalls) == 0 && len(textParts) == 0 {
			return
		}
		m := map[string]interface{}{"role": "assistant", "content": strings.Join(textParts, "\n")}
		if len(toolCalls) > 0 {
			m["tool_calls"] = toolCalls
		}
		b, _ := json.Marshal(m)
		out = append(out, b)
		textParts = nil
		toolCalls = nil
	}

	for _, block := range blocks {
		t, _ := block["type"].(string)
		switch t {
		case "text":
			if text, _ := block["text"].(string); text != "" {
				textParts = append(textParts, text)
			}
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			args, _ := json.Marshal(block["input"])
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   id,
				"type": "function",
				"function": map[string]string{
					"name":      name,
					"arguments": string(args),
				},
			})
		case "tool_result":
			flushAssistant()
			id, _ := block["tool_use_id"].(string)
			content := toolResultText(block["content"])
			b, _ := json.Marshal(map[string]string{
				"role":         "tool",
				"tool_call_id": id,
				"content":      content,
			})
			out = append(out, b)
		}
	}

	if msg.Role == "assistant" {
		flushAssistant()
		return out
	}
	// user message with only text blocks
	if len(textParts) > 0 {
		b, _ := json.Marshal(map[string]string{"role": msg.Role, "content": strings.Join(textParts, "\n")})
		out = append(out, b)
	}
	return out
}

func toolResultText(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case []interface{}:
		var parts []string
		for _, item := range x {
			if m, ok := item.(map[string]interface{}); ok {
				if t, _ := m["text"].(string); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func anthropicToolsToOpenAI(tools []json.RawMessage) []json.RawMessage {
	var result []json.RawMessage
	for _, raw := range tools {
		var t struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description"`
			InputSchema map[string]interface{} `json:"input_schema"`
		}
		if json.Unmarshal(raw, &t) != nil || t.Name == "" {
			continue
		}
		out, _ := json.Marshal(map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.InputSchema,
			},
		})
		result = append(result, out)
	}
	return result
}

type anthropicResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Model      string         `json:"model"`
	Role       string         `json:"role"`
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      usage          `json:"usage,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ponytail: one in-flight /v1/messages globally + min gap between upstream
// calls (Desktop + CLI flood; free-tier rate limits).
var (
	anthropicGate     sync.Mutex
	anthropicGapMu    sync.Mutex
	lastAnthropicCall time.Time
)

func lockAnthropicSession(_ string) func() {
	anthropicGate.Lock()
	return anthropicGate.Unlock
}

func waitAnthropicGap() {
	anthropicGapMu.Lock()
	defer anthropicGapMu.Unlock()
	gap := 1500 * time.Millisecond
	if v := os.Getenv("ANTHROPIC_MIN_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			gap = time.Duration(ms) * time.Millisecond
		}
	}
	if wait := gap - time.Since(lastAnthropicCall); wait > 0 {
		log.Printf("[Anthropic] throttle %v", wait.Round(time.Millisecond))
		time.Sleep(wait)
	}
	lastAnthropicCall = time.Now()
}

func (s *Server) anthropicSend(ctx context.Context, prompt string, opts sendOpts) (string, error) {
	waitAnthropicGap()
	content, _, err := s.sendToVerdent(ctx, prompt, opts, nil, nil)
	return content, err
}

func (s *Server) anthropicStream(ctx context.Context, prompt string, opts sendOpts, onChunk func(string)) (string, error) {
	waitAnthropicGap()
	content, _, err := s.sendToVerdent(ctx, prompt, opts, onChunk, nil)
	return content, err
}

func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 50*1024*1024)

	rawBody, _ := io.ReadAll(r.Body)
	if !utf8.Valid(rawBody) {
		log.Printf("[TRACE][ingress] REJECTED non-UTF-8 /v1/messages body from %s (%d bytes): %q",
			r.RemoteAddr, len(rawBody), truncate(string(rawBody), 160))
		writeAnthropicAPIError(w, fmt.Errorf("request body is not valid UTF-8 (client console encoding?) — send UTF-8 (chcp 65001)"))
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
		return
	}
	if len(req.Messages) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "messages is required"})
		return
	}

	model := resolveGLMModel(req.Model)

	sessionID := r.Header.Get("X-Session-Id")
	if sessionID == "" {
		sessionID = "default"
	}
	unlock := lockAnthropicSession(sessionID)
	defer unlock()

	conv := s.conversations.getOrCreate(sessionID, r.Header.Get("X-Fresh-Session") == "true")
	requestID := generateID()

	openAIMsgs := sanitizeAnthropicMessages(anthropicToOpenAIMessages(req.Messages, req.System))
	openAITools := anthropicToolsToOpenAI(limitAnthropicTools(req.Tools))
	prompt := messagesToPrompt(openAIMsgs)

	// Claude Code CLI auxiliary requests (suggestion of the next user input,
	// "user stepped away" recap) carry the full tool set, but the CLI rejects
	// any tool_use in their responses ("No tools needed for suggestion") and
	// feeds that error back into the main history — stalling the visible
	// conversation. Answer them TEXT-ONLY.
	auxiliary := isAuxiliaryClaudeCodeRequest(req.Messages)
	if auxiliary && len(openAITools) > 0 {
		log.Printf("[Anthropic] auxiliary request (suggestion/recap) — tools stripped")
		openAITools = nil
	}

	logAnthropicRequest(req, model, openAIMsgs, openAITools, prompt)

	if len(openAITools) > 0 {
		log.Printf("[Anthropic] tools path (glm=%s claude=%s stream=%v)", model, req.Model, req.Stream)
		s.handleAnthropicWithTools(w, r, req, openAIMsgs, openAITools, model, requestID, conv)
		return
	}

	opts := sendOpts{
		model:          model,
		chatID:         conv.chatID,
		messages:       conv.messages,
		clientMessages: openAIMsgs,
	}

	if req.Stream {
		log.Printf("[Anthropic] stream path prompt=%d chars", len(prompt))
		s.handleAnthropicStream(w, r, prompt, req.Model, requestID, opts, conv)
	} else {
		s.handleAnthropicNonStream(w, r, prompt, req.Model, requestID, opts, conv)
	}
}

// logAnthropicRequest dumps conversion results at info level for debugging Claude Desktop.
func logAnthropicRequest(req anthropicRequest, glmModel string, openAIMsgs, openAITools []json.RawMessage, prompt string) {
	log.Printf("[Anthropic] in: claude=%s glm=%s stream=%v raw_msgs=%d raw_tools=%d",
		req.Model, glmModel, req.Stream, len(req.Messages), len(req.Tools))
	for i, raw := range req.Messages {
		log.Printf("[Anthropic] raw[%d]: %s", i, truncate(string(raw), 400))
	}
	if len(req.System) > 0 {
		log.Printf("[Anthropic] system: %s", truncate(getMessageContent(req.System), 400))
	}
	log.Printf("[Anthropic] converted: openai_msgs=%d openai_tools=%d prompt=%d chars",
		len(openAIMsgs), len(openAITools), len(prompt))
	for i, raw := range openAIMsgs {
		log.Printf("[Anthropic] openai[%d]: %s", i, truncate(string(raw), 400))
	}
	if len(req.Tools) > 0 && len(openAITools) == 0 {
		log.Printf("[Anthropic] WARN: tools conversion failed — raw tool[0]: %s",
			truncate(string(req.Tools[0]), 400))
	}
	if len(openAIMsgs) == 0 {
		log.Printf("[Anthropic] WARN: no messages after conversion — upstream will get empty payload")
	}
	if prompt == "" {
		log.Printf("[Anthropic] WARN: empty prompt after conversion")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (s *Server) handleAnthropicStream(w http.ResponseWriter, r *http.Request, prompt, model, requestID string, opts sendOpts, conv *Conversation) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	flusher.Flush()

	writeSSE := func(event, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
	}

	msgID := fmt.Sprintf("msg_%s", requestID)
	writeSSE("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","model":%q,"content":[]}}`, msgID, model))
	writeSSE("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	writeSSE("ping", `{"type":"ping"}`)

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	var fullContent strings.Builder
	finalContent, err := s.anthropicStream(ctx, prompt, opts, func(chunk string) {
		fullContent.WriteString(chunk)
		data, _ := json.Marshal(map[string]interface{}{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]string{"type": "text_delta", "text": chunk},
		})
		writeSSE("content_block_delta", string(data))
	})

	stopReason := "end_turn"
	if err != nil {
		log.Printf("[Anthropic Stream] Error: %v", err)
		stopReason = "error"
	}

	writeSSE("content_block_stop", `{"type":"content_block_stop","index":0}`)
	writeSSE("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null}}`, stopReason))
	writeSSE("message_stop", `{"type":"message_stop"}`)

	if persistHistoryEnabled() && err == nil {
		// Authoritative parser content (edit_content-safe)
		if finalContent == "" {
			finalContent = fullContent.String()
		}
		conv.messages = append(conv.messages,
			map[string]interface{}{"role": "user", "content": prompt},
			map[string]interface{}{"role": "assistant", "content": finalContent},
		)
	}
}

func (s *Server) handleAnthropicNonStream(w http.ResponseWriter, r *http.Request, prompt, model, requestID string, opts sendOpts, conv *Conversation) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	text, err := s.anthropicSend(ctx, prompt, opts)
	if err != nil {
		log.Printf("[Anthropic NonStream] Error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"type": "error",
			"error": map[string]interface{}{
				"type":    "api_error",
				"message": err.Error(),
			},
		})
		return
	}

	resp := anthropicResponse{
		ID:         fmt.Sprintf("msg_%s", requestID),
		Type:       "message",
		Model:      model,
		Role:       "assistant",
		Content:    []contentBlock{{Type: "text", Text: text}},
		StopReason: "end_turn",
		Usage:      usage{InputTokens: estimateTokens(prompt), OutputTokens: estimateTokens(text)},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)

	if persistHistoryEnabled() {
		conv.messages = append(conv.messages,
			map[string]interface{}{"role": "user", "content": prompt},
			map[string]interface{}{"role": "assistant", "content": text},
		)
	}
}

func buildAnthropicToolsFraming(tools []json.RawMessage) string {
	// Same strict contract as the OpenAI path (tools.go) — soft phrasing
	// makes GLM announce actions in prose instead of emitting tool blocks.
	return fmt.Sprintf(toolCallContract, buildCompactToolList(tools))
}

// convertAnthropicToolMessages — lighter framing than OpenAI unit-test path (CLI simple chat).
func convertAnthropicToolMessages(messages, tools []json.RawMessage) []json.RawMessage {
	framing := buildAnthropicToolsFraming(tools)
	lastUserIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		var msg map[string]interface{}
		if json.Unmarshal(messages[i], &msg) == nil {
			if role, _ := msg["role"].(string); role == "user" {
				if _, hasToolCallID := msg["tool_call_id"]; !hasToolCallID {
					lastUserIdx = i
					break
				}
			}
		}
	}

	var result []json.RawMessage
	for i, raw := range messages {
		var msg map[string]interface{}
		if json.Unmarshal(raw, &msg) != nil {
			result = append(result, raw)
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "system":
			// Keep system messages — they carry the client's instructions
			// (role=system is accepted; the framing contract rides the last user msg)
			result = append(result, raw)
		case "tool":
			content, _ := msg["content"].(string)
			toolCallID, _ := msg["tool_call_id"].(string)
			b, _ := json.Marshal(map[string]string{
				"role":    "user",
				"content": fmt.Sprintf("[Tool result for %s]: %s", toolCallID, content),
			})
			result = append(result, b)
		case "assistant":
			if tc, ok := msg["tool_calls"].([]interface{}); ok && len(tc) > 0 {
				var sb strings.Builder
				for _, call := range tc {
					if c, ok := call.(map[string]interface{}); ok {
						if fn, ok := c["function"].(map[string]interface{}); ok {
							name, _ := fn["name"].(string)
							args, _ := fn["arguments"].(string)
							sb.WriteString(fmt.Sprintf("{\"name\":\"%s\",\"arguments\":%s}\n", name, args))
						}
					}
				}
				origContent, _ := msg["content"].(string)
				b, _ := json.Marshal(map[string]string{
					"role":    "assistant",
					"content": strings.TrimSpace(origContent + "\n" + sb.String()),
				})
				result = append(result, b)
			} else {
				result = append(result, raw)
			}
		case "user":
			if i == lastUserIdx {
				origContent, _ := msg["content"].(string)
				trimmed := strings.TrimSpace(origContent)
				// CLI continuation nudges: empty or "(no content)" placeholders
				// after an intent-only assistant turn. Framing them with an
				// empty Input makes GLM narrate the action instead of calling
				// the tool — direct it to EXECUTE now.
				if trimmed == "" || trimmed == "(no content)" {
					b, _ := json.Marshal(map[string]string{
						"role": "user",
						"content": framing + "\n\nUser: (empty continuation nudge)\n\n" +
							"Continue the CURRENT task NOW: you already announced the next action — emit its tool call block immediately. Do not answer in prose.",
					})
					result = append(result, b)
					break
				}
				b, _ := json.Marshal(map[string]string{
					"role":    "user",
					"content": framing + "\n\nUser: " + origContent,
				})
				result = append(result, b)
			} else {
				result = append(result, raw)
			}
		default:
			result = append(result, raw)
		}
	}
	if lastUserIdx < 0 {
		b, _ := json.Marshal(map[string]string{"role": "system", "content": framing})
		result = append([]json.RawMessage{b}, result...)
	}
	return result
}

func dropEmptyRequiredToolCalls(calls []parsedToolCall, tools []json.RawMessage) []parsedToolCall {
	schemas := buildSchemaMap(tools)
	var out []parsedToolCall
	for _, c := range calls {
		args := strings.TrimSpace(c.Function.Arguments)
		if args == "" || args == "{}" {
			schema := schemas[c.Function.Name]
			if schema == nil {
				// Unknown schema — empty input would fail client-side
				// validation (e.g. "required parameter command is missing")
				log.Printf("[Anthropic Tools] drop %q: empty args and no schema to verify", c.Function.Name)
				continue
			}
			if req, ok := schema["required"].([]interface{}); ok && len(req) > 0 {
				log.Printf("[Anthropic Tools] drop %q: empty args but schema has required fields", c.Function.Name)
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

func (s *Server) handleAnthropicWithTools(w http.ResponseWriter, r *http.Request, req anthropicRequest, openAIMsgs, openAITools []json.RawMessage, glmModel, requestID string, conv *Conversation) {
	convertedMsgs := convertAnthropicToolMessages(openAIMsgs, openAITools)
	convertedPrompt := messagesToPrompt(convertedMsgs)
	log.Printf("[Anthropic Tools] prompt=%d chars tools=%d", len(convertedPrompt), len(openAITools))

	opts := sendOpts{
		model:          glmModel,
		chatID:         conv.chatID,
		messages:       conv.messages,
		clientMessages: convertedMsgs,
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()

	content, err := s.anthropicSend(ctx, convertedPrompt, opts)
	if err != nil {
		log.Printf("[Anthropic Tools] Error: %v", err)
		writeAnthropicAPIError(w, err)
		return
	}

	toolCalls := dropEmptyRequiredToolCalls(parseToolCalls(content, openAITools), openAITools)
	if len(toolCalls) > 0 {
		writeAnthropicToolResponse(w, req, req.Model, requestID, stripToolContent(content), toolCalls)
		log.Printf("[Anthropic Tools] Returned %d tool_use blocks", len(toolCalls))
		return
	}
	// No parsable calls. Don't leak raw contract markers to the client:
	// strip complete <<<TOOL_CALL>>> blocks if the model emitted any.
	if strings.Contains(content, agentToolCallStart) {
		preview := content
		if len(preview) > 1500 {
			preview = preview[:1500] + "...(truncated)"
		}
		log.Printf("[Anthropic Tools] markers present but no valid tool call parsed — model output:\n%s", preview)
		content = stripAgentToolCallBlocks(content)
	}
	writeAnthropicTextResponse(w, req, req.Model, requestID, content)
}

func writeAnthropicAPIError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	statusCode := 500
	if strings.Contains(err.Error(), "401") {
		statusCode = 401
	} else if strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "rate") {
		statusCode = 429
		w.Header().Set("Retry-After", "15")
	}
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "api_error",
			"message": err.Error(),
		},
	})
}

func toolCallsToAnthropicBlocks(calls []parsedToolCall, text string) []map[string]interface{} {
	var blocks []map[string]interface{}
	if t := strings.TrimSpace(text); t != "" {
		blocks = append(blocks, map[string]interface{}{"type": "text", "text": t})
	}
	for i, c := range calls {
		var input map[string]interface{}
		_ = json.Unmarshal([]byte(c.Function.Arguments), &input)
		if input == nil {
			input = map[string]interface{}{}
		}
		id := c.ID
		if !strings.HasPrefix(id, "toolu_") {
			id = fmt.Sprintf("toolu_%d", i+1)
		}
		blocks = append(blocks, map[string]interface{}{
			"type":  "tool_use",
			"id":    id,
			"name":  c.Function.Name,
			"input": input,
		})
	}
	return blocks
}

func writeAnthropicToolResponse(w http.ResponseWriter, req anthropicRequest, claudeModel, requestID, text string, calls []parsedToolCall) {
	blocks := toolCallsToAnthropicBlocks(calls, text)
	msgID := fmt.Sprintf("msg_%s", requestID)

	if req.Stream {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeAnthropicMessageJSON(w, claudeModel, msgID, blocks, "tool_use")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(200)
		flusher.Flush()
		writeAnthropicSSE := func(event, data string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
			flusher.Flush()
		}
		writeAnthropicSSE("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","model":%q,"content":[]}}`, msgID, claudeModel))

		// Proper Anthropic streaming protocol: tool input travels via
		// input_json_delta events — clients ignore "input" inside
		// content_block_start (sending it there yields empty-input tools).
		idx := 0
		if t := strings.TrimSpace(text); t != "" {
			writeAnthropicSSE("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, idx))
			d, _ := json.Marshal(map[string]interface{}{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]string{"type": "text_delta", "text": t},
			})
			writeAnthropicSSE("content_block_delta", string(d))
			writeAnthropicSSE("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, idx))
			idx++
		}
		for i, c := range calls {
			tooluID := c.ID
			if !strings.HasPrefix(tooluID, "toolu_") {
				tooluID = fmt.Sprintf("toolu_%d", i+1)
			}
			writeAnthropicSSE("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, idx, tooluID, c.Function.Name))
			partial := c.Function.Arguments
			if strings.TrimSpace(partial) == "" {
				partial = "{}"
			}
			d, _ := json.Marshal(map[string]interface{}{
				"type":  "content_block_delta",
				"index": idx,
				"delta": map[string]string{"type": "input_json_delta", "partial_json": partial},
			})
			writeAnthropicSSE("content_block_delta", string(d))
			writeAnthropicSSE("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, idx))
			idx++
		}
		writeAnthropicSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null}}`)
		writeAnthropicSSE("message_stop", `{"type":"message_stop"}`)
		return
	}
	writeAnthropicMessageJSON(w, claudeModel, msgID, blocks, "tool_use")
}

func writeAnthropicTextResponse(w http.ResponseWriter, req anthropicRequest, claudeModel, requestID, text string) {
	msgID := fmt.Sprintf("msg_%s", requestID)
	blocks := []map[string]interface{}{{"type": "text", "text": text}}

	if req.Stream {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeAnthropicMessageJSON(w, claudeModel, msgID, blocks, "end_turn")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(200)
		flusher.Flush()
		writeAnthropicSSE := func(event, data string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
			flusher.Flush()
		}
		writeAnthropicSSE("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","model":%q,"content":[]}}`, msgID, claudeModel))
		writeAnthropicSSE("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		const chunkSize = 2000
		for i := 0; i < len(text); {
			end := runeChunkEnd(text, i, i+chunkSize)
			delta, _ := json.Marshal(map[string]interface{}{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]string{"type": "text_delta", "text": text[i:end]},
			})
			writeAnthropicSSE("content_block_delta", string(delta))
			i = end
		}
		writeAnthropicSSE("content_block_stop", `{"type":"content_block_stop","index":0}`)
		writeAnthropicSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null}}`)
		writeAnthropicSSE("message_stop", `{"type":"message_stop"}`)
		return
	}
	writeAnthropicMessageJSON(w, claudeModel, msgID, blocks, "end_turn")
}

func writeAnthropicMessageJSON(w http.ResponseWriter, claudeModel, msgID string, blocks []map[string]interface{}, stopReason string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":          msgID,
		"type":        "message",
		"role":        "assistant",
		"model":       claudeModel,
		"content":     blocks,
		"stop_reason": stopReason,
	})
}
