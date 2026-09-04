// bridge.go — shared plumbing between the client-facing handlers and the
// Verdent upstream: request options, per-session conversation state, and
// small utilities (moved out of the former zai.go).
package app

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ── Per-conversation state ──

type Conversation struct {
	chatID   string
	messages []map[string]interface{}
	lastUsed time.Time
}

type ConversationStore struct {
	mu       sync.Mutex
	sessions map[string]*Conversation
}

const sessionTTL = 30 * time.Minute

func newConversationStore() *ConversationStore {
	cs := &ConversationStore{sessions: make(map[string]*Conversation)}
	go cs.cleanup()
	return cs
}

func (cs *ConversationStore) getOrCreate(sessionID string, fresh bool) *Conversation {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if fresh || cs.sessions[sessionID] == nil {
		cs.sessions[sessionID] = &Conversation{
			chatID:   uuidV4(),
			lastUsed: time.Now(),
		}
	}
	s := cs.sessions[sessionID]
	s.lastUsed = time.Now()
	return s
}

func (cs *ConversationStore) clear() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.sessions = make(map[string]*Conversation)
}

func (cs *ConversationStore) count() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.sessions)
}

func (cs *ConversationStore) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cs.mu.Lock()
		now := time.Now()
		for id, s := range cs.sessions {
			if now.Sub(s.lastUsed) > sessionTTL {
				delete(cs.sessions, id)
			}
		}
		cs.mu.Unlock()
	}
}

// ── Upstream request options ──

type sendOpts struct {
	model           string
	reasoningEffort string // client reasoning_effort → upstream effort
	thinking        string // "" = model default | "enabled" | "disabled"
	context         string // "" = model default | "300K" | "1M"
	chatID          string
	messages        []map[string]interface{}
	clientMessages  []json.RawMessage
}

// ── Utility ──

func uuidV4() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0F) | 0x40
	b[8] = (b[8] & 0x3F) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func generateID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}

// getMessageContent extracts text from an OpenAI message content field,
// which can be a string or an array of content parts.
func getMessageContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []map[string]interface{}
	if json.Unmarshal(raw, &parts) == nil {
		var texts []string
		for _, p := range parts {
			if t, ok := p["type"].(string); ok && t != "text" {
				continue
			}
			if text, ok := p["text"].(string); ok {
				texts = append(texts, text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

// messagesToPrompt flattens an OpenAI messages array → prompt string (token
// estimation, history persistence, logs).
func messagesToPrompt(messages []json.RawMessage) string {
	var sb strings.Builder
	for _, raw := range messages {
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		sb.WriteString(getMessageContent(msg.Content))
		sb.WriteString("\n\n")
	}
	return strings.TrimSpace(sb.String())
}

// runeChunkEnd returns the largest chunk end ≤ max that does not split a
// UTF-8 rune (backs off up to 3 bytes to a rune start). Byte-sliced chunks
// would cut multi-byte characters (Cyrillic, emoji) in half — json.Marshal
// renders the halves as U+FFFD ("��"). Fix ported from GLM-Free-API prod.
func runeChunkEnd(s string, i, max int) int {
	end := max
	if end > len(s) {
		end = len(s)
	}
	for end < len(s) && end > i+1 && !utf8.RuneStart(s[end]) {
		end--
	}
	return end
}

// jwtPayload decodes the payload section of a JWT (base64url, no padding).
func jwtPayload(token string) (map[string]interface{}, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, err
	}
	return m, nil
}
