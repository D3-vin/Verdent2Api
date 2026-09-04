// verdentoauht.go — Verdent PKCE OAuth login (port of the reference
// router's oauth.js): the dashboard starts the flow, the system browser
// completes it on verdent.ai, and /auth/callback exchanges the code for a
// fresh JWT bound to the deviceId generated at flow start.
package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const verdentAuthBase = "https://www.verdent.ai/auth"
const verdentPKCECallback = "https://login.verdent.ai/passport/pkce/callback"

type pendingOAuth struct {
	verifier string
	deviceID string
	ts       time.Time
}

var (
	pendingOAuthMu sync.Mutex
	pendingOAuths  = map[string]*pendingOAuth{}
)

func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// apiAuthStart kicks off the PKCE flow: builds the login URL for the
// system browser and remembers verifier+deviceId for the callback.
func (s *Server) apiAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	verifierRaw := make([]byte, 32)
	rand.Read(verifierRaw)
	verifier := b64url(verifierRaw)
	challenge := b64url([]byte(sha256Sum(verifier)))
	state := b64url(randomBytes(32))
	deviceID := strings.ToLower(uuidV4())
	callback := fmt.Sprintf("http://127.0.0.1:%s/auth/callback", s.cfg.Port)

	pendingOAuthMu.Lock()
	// opportunistic cleanup of stale entries (>15 min)
	for k, p := range pendingOAuths {
		if time.Since(p.ts) > 15*time.Minute {
			delete(pendingOAuths, k)
		}
	}
	pendingOAuths[state] = &pendingOAuth{verifier: verifier, deviceID: deviceID, ts: time.Now()}
	pendingOAuthMu.Unlock()

	url := fmt.Sprintf("%s?challenge=%s&state=%s&intent=signin&callback=%s&ots=deck&source=deck&id=%s",
		verdentAuthBase, challenge, state, urlQueryEscape(callback), deviceID)

	writeJSON(w, map[string]interface{}{"ok": true, "url": url})
}

func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func urlQueryEscape(s string) string {
	var sb strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			sb.WriteByte(c)
		} else {
			fmt.Fprintf(&sb, "%%%02X", c)
		}
	}
	return sb.String()
}

// handleAuthCallback finishes the PKCE flow (unauthenticated: the browser
// redirect carries no API token).
func (s *Server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")

	pendingOAuthMu.Lock()
	pending := pendingOAuths[state]
	delete(pendingOAuths, state)
	pendingOAuthMu.Unlock()

	fail := func(msg string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(400)
		fmt.Fprintf(w, "<h3>OAuth failed</h3><pre>%s</pre>", msg)
	}
	if code == "" || pending == nil {
		fail("invalid state/code — restart login from the dashboard")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	body := fmt.Sprintf(`{"code":%q,"codeVerifier":%q}`, code, pending.verifier)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, verdentPKCECallback, strings.NewReader(body))
	if err != nil {
		fail(err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := verdentHTTPClient.Do(req)
	if err != nil {
		fail("exchange: " + err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != 200 {
		fail(fmt.Sprintf("exchange HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200)))
		return
	}

	var out struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Data.Token == "" {
		fail("no token in exchange response: " + truncate(string(raw), 200))
		return
	}

	if err := s.accounts.AddAccountWithDevice(out.Data.Token, pending.deviceID); err != nil {
		fail("add account: " + err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<h3>✅ Logged in — return to the Verdent 2API dashboard</h3>`)
}
