package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
)

func (a *opencodeAgent) ensureServer(ctx context.Context, cwd string, env []string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		return a.server.baseURL(), nil
	}
	port, err := getAvailablePort()
	if err != nil {
		return "", fmt.Errorf("opencode port: %w", err)
	}
	args := buildOpencodeServeArgs(a.extraArgs, port)
	srv, err := startServerWithPort(ctx, "opencode", a.bin, args, cwd, "/global/health", port, a.overlay(), env)
	if err != nil {
		return "", fmt.Errorf("opencode server: %w", err)
	}
	a.server = srv
	return srv.baseURL(), nil
}

// buildOpencodeServeArgs builds `opencode serve`'s argv with user-supplied
// extras inserted after the "serve" subcommand and before the managed flags.
func buildOpencodeServeArgs(extraArgs []string, port int) []string {
	args := make([]string, 0, len(extraArgs)+6)
	args = append(args, "serve")
	args = append(args, extraArgs...)
	args = append(args, "--hostname", "127.0.0.1", "--port", fmt.Sprintf("%d", port), "--print-logs")
	return args
}

// openSession returns the session a turn runs in: a new one for a cold or
// newly durable invocation, or the verified stored one for a resume.
func (a *opencodeAgent) openSession(ctx context.Context, baseURL string, opts RunOpts) (string, error) {
	if opts.Session == nil || opts.Session.ID == "" {
		return a.createSession(ctx, baseURL, opts.CWD)
	}
	if err := a.verifyResumableSession(ctx, baseURL, opts.Session.ID, opts.CWD); err != nil {
		return "", err
	}
	return opts.Session.ID, nil
}

func (a *opencodeAgent) createSession(ctx context.Context, baseURL, cwd string) (string, error) {
	body := map[string]any{
		"permission": []map[string]string{
			{"permission": "*", "pattern": "*", "action": "allow"},
		},
	}
	// opencode binds a session to the `directory` query parameter, or to the
	// server process's own working directory without one; a `directory` body
	// field is silently ignored (verified against opencode 1.18.30). The bound
	// directory is where the session's tools run for its whole life - a later
	// message cannot move it - so a resumable session must be bound explicitly.
	sessionURL := baseURL + "/session"
	if cwd != "" {
		sessionURL += "?directory=" + url.QueryEscape(cwd)
	}
	resp, err := doJSON(ctx, http.MethodPost, sessionURL, nil, body)
	if err != nil {
		return "", fmt.Errorf("opencode create session: %w", err)
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return "", fmt.Errorf("opencode create session parse: %w", err)
	}
	return result.ID, nil
}

// opencodeSessionIDPattern accepts only the identity shape opencode mints.
// no-mistakes persists only IDs opencode returned, so anything else is corrupt
// metadata. Refusing it before any request keeps it out of the URL path and
// gives a clear error instead of opencode's opaque 500 UnknownError for a
// malformed ID.
var opencodeSessionIDPattern = regexp.MustCompile(`^ses_[A-Za-z0-9]{1,64}$`)

// verifyResumableSession confirms a stored session still exists and is bound
// to the invocation's working directory before a turn is sent to it. Sessions
// persist in opencode's own database, so a restarted server still finds one;
// a pruned or deleted session answers 404, which fails the turn and lets the
// pipeline start a fresh session instead.
func (a *opencodeAgent) verifyResumableSession(ctx context.Context, baseURL, sessionID, cwd string) error {
	if !opencodeSessionIDPattern.MatchString(sessionID) {
		return errors.New("invalid opencode session identity")
	}
	resp, err := doJSON(ctx, http.MethodGet, baseURL+"/session/"+sessionID, nil, nil)
	if err != nil {
		return fmt.Errorf("opencode resume session: %w", err)
	}
	var session struct {
		ID        string `json:"id"`
		Directory string `json:"directory"`
	}
	if err := json.Unmarshal(resp, &session); err != nil {
		return fmt.Errorf("opencode resume session parse: %w", err)
	}
	if session.ID != sessionID {
		return errors.New("opencode did not confirm the requested session")
	}
	if cwd != "" && !sameDirectory(session.Directory, cwd) {
		return errors.New("opencode session is bound to a different directory")
	}
	return nil
}

func sameDirectory(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

func (a *opencodeAgent) connectEventStream(ctx context.Context, baseURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/global/event", nil)
	if err != nil {
		return nil, fmt.Errorf("opencode event stream request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencode event stream: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("opencode event stream failed with %d: %s", resp.StatusCode, string(body))
	}

	return resp.Body, nil
}

func (a *opencodeAgent) sendMessage(ctx context.Context, baseURL, sessionID, prompt string, schema json.RawMessage) (*opencodeMessageResponse, error) {
	respBytes, err := doJSON(ctx, http.MethodPost, baseURL+"/session/"+sessionID+"/message", nil, a.messageBody(prompt, schema))
	if err != nil {
		return nil, err
	}

	var resp opencodeMessageResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, fmt.Errorf("opencode message parse: %w", err)
	}
	return &resp, nil
}

// messageBody builds the POST /session/{id}/message payload.
//
// Model and reasoning effort ride the body rather than argv because `opencode
// serve` has no flag for either: it exits with usage on an unknown option, so an
// argv-based pin would take the whole server down instead of tuning it. The
// provider/model split is validated at construction by agentcfg, so a non-empty
// model always yields the two fields opencode requires; opencode calls reasoning
// effort a provider-specific "variant".
func (a *opencodeAgent) messageBody(prompt string, schema json.RawMessage) map[string]any {
	body := map[string]any{
		"role":  "user",
		"parts": []map[string]string{{"type": "text", "text": prompt}},
	}
	if provider, modelID, ok := agentcfg.SplitProviderModel(a.profile.Model); ok {
		body["model"] = map[string]string{"providerID": provider, "modelID": modelID}
	}
	if a.profile.Effort != "" {
		body["variant"] = string(a.profile.Effort)
	}
	if len(schema) > 0 {
		body["info"] = map[string]any{
			"format": map[string]any{
				"type":       "json_schema",
				"schema":     json.RawMessage(schema),
				"retryCount": 2,
			},
		}
	}
	return body
}

func (a *opencodeAgent) abortSession(baseURL, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	doJSON(ctx, http.MethodPost, baseURL+"/session/"+sessionID+"/abort", nil, nil)
}

func (a *opencodeAgent) deleteSession(baseURL, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+"/session/"+sessionID, nil)
	if req != nil {
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp != nil {
			resp.Body.Close()
		}
	}
}

// buildOpencodePrompt appends schema instructions to the prompt.
func buildOpencodePrompt(prompt string, schema json.RawMessage) string {
	return strings.Join([]string{
		prompt,
		"",
		"When you finish, reply with only valid JSON.",
		"Do not wrap the JSON in markdown fences.",
		"Do not include any prose before or after the JSON.",
		"The JSON must match this schema exactly: " + string(schema),
	}, "\n")
}
