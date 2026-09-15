package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const opencodeNotRetryableFailure = `{"info":{"id":"msg1","role":"assistant","error":{"name":"APIError",` +
	`"data":{"message":"bad request","statusCode":400,"isRetryable":false}}},"parts":[]}`

// opencodeSessionServer models the session store of a real `opencode serve`:
// sessions are bound to the `directory` query parameter at creation, persist
// until deleted, and an unknown session answers 404 NotFoundError. It records
// every session request so a test can assert which sessions were created,
// resumed, and deleted.
type opencodeSessionServer struct {
	*httptest.Server

	mu       sync.Mutex
	created  int
	dirs     map[string]string
	requests []string
	bodies   []string
	sent     int
}

func newOpencodeSessionServer(t *testing.T, bodies ...string) *opencodeSessionServer {
	t.Helper()
	s := &opencodeSessionServer{dirs: map[string]string{}, bodies: bodies}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

func (s *opencodeSessionServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/global/event" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	if r.URL.Path == "/session" && r.Method == http.MethodPost {
		s.created++
		id := fmt.Sprintf("ses_test%d", s.created)
		dir := r.URL.Query().Get("directory")
		s.dirs[id] = dir
		fmt.Fprintf(w, `{"id":%q,"directory":%q}`, id, dir)
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/session/"), "/")
	id := parts[0]
	dir, known := s.dirs[id]
	if !known {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"name":"NotFoundError","data":{"message":"Session not found: %s"}}`, id)
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		fmt.Fprintf(w, `{"id":%q,"directory":%q}`, id, dir)
	case len(parts) == 1 && r.Method == http.MethodDelete:
		delete(s.dirs, id)
	case len(parts) == 2 && parts[1] == "message" && r.Method == http.MethodPost:
		body := `{"info":{"id":"msg1","role":"assistant","tokens":{"input":10,"output":5}},"parts":[{"type":"text","text":"done"}]}`
		if len(s.bodies) > 0 {
			body = s.bodies[len(s.bodies)-1]
			if s.sent < len(s.bodies) {
				body = s.bodies[s.sent]
			}
		}
		s.sent++
		fmt.Fprint(w, body)
	}
}

func (s *opencodeSessionServer) count(request string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if r == request {
			n++
		}
	}
	return n
}

func (s *opencodeSessionServer) exists(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.dirs[id]
	return ok
}

func (s *opencodeSessionServer) agent() *opencodeAgent {
	return &opencodeAgent{bin: "opencode", server: &managedServer{port: mustParsePort(s.URL)}}
}

func TestOpencodeAgent_DurableSessionIsBoundToTheWorktreeAndKept(t *testing.T) {
	server := newOpencodeSessionServer(t)
	cwd := t.TempDir()

	result, err := server.agent().Run(context.Background(), RunOpts{Prompt: "fix it", CWD: cwd, Session: &SessionRef{}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.SessionID != "ses_test1" || result.Resumed {
		t.Fatalf("SessionID=%q Resumed=%v, want a started ses_test1", result.SessionID, result.Resumed)
	}
	if !server.exists("ses_test1") {
		t.Fatal("a durable session must outlive its successful turn")
	}
	if got := server.dirs["ses_test1"]; got != cwd {
		t.Fatalf("session bound to %q, want the invocation CWD %q", got, cwd)
	}
}

// TestOpencodeAgent_ResumesThePersistedSessionFromANewAgent covers the fixer
// session's second turn, including after a daemon restart: a new adapter
// instance with no memory of the first turn continues the same opencode
// session instead of creating another one.
func TestOpencodeAgent_ResumesThePersistedSessionFromANewAgent(t *testing.T) {
	server := newOpencodeSessionServer(t)
	cwd := t.TempDir()

	first, err := server.agent().Run(context.Background(), RunOpts{Prompt: "fix round 1", CWD: cwd, Session: &SessionRef{}})
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	second, err := server.agent().Run(context.Background(), RunOpts{
		Prompt:  "fix round 2",
		CWD:     cwd,
		Session: &SessionRef{ID: first.SessionID, Agent: "opencode"},
	})
	if err != nil {
		t.Fatalf("resumed turn: %v", err)
	}
	if second.SessionID != first.SessionID || !second.Resumed {
		t.Fatalf("SessionID=%q Resumed=%v, want resumed %q", second.SessionID, second.Resumed, first.SessionID)
	}
	if second.SessionUsageCumulative {
		t.Fatal("opencode reports usage per message, so resumed usage must not be marked cumulative")
	}
	if n := server.count("POST /session"); n != 1 {
		t.Fatalf("created %d sessions, want 1", n)
	}
	if n := server.count("GET /session/ses_test1"); n != 1 {
		t.Fatalf("verified the stored session %d times, want 1", n)
	}
	if n := server.count("POST /session/ses_test1/message"); n != 2 {
		t.Fatalf("sent %d turns to the session, want 2", n)
	}
	if server.count("DELETE /session/ses_test1") != 0 {
		t.Fatal("a resumed session must not be deleted")
	}
}

func TestOpencodeAgent_ResumeOfMissingSessionFailsWithoutSendingTheTurn(t *testing.T) {
	server := newOpencodeSessionServer(t)

	result, err := server.agent().Run(context.Background(), RunOpts{
		Prompt:  "fix it",
		CWD:     t.TempDir(),
		Session: &SessionRef{ID: "ses_pruned", Agent: "opencode"},
	})
	if err == nil {
		t.Fatalf("expected the resume to fail, got %+v", result)
	}
	if !strings.Contains(err.Error(), "Session not found") {
		t.Fatalf("error should carry opencode's cause, got %v", err)
	}
	// The pipeline's fresh-session fallback owns recovery, so the adapter must
	// neither retry the dead session nor quietly replace it.
	if n := server.count("GET /session/ses_pruned"); n != 1 {
		t.Fatalf("checked the dead session %d times, want 1 (no retry)", n)
	}
	if server.count("POST /session") != 0 || server.count("POST /session/ses_pruned/message") != 0 {
		t.Fatal("a failed resume must not create a session or send the turn")
	}
}

func TestOpencodeAgent_ResumeRefusesASessionBoundToAnotherDirectory(t *testing.T) {
	server := newOpencodeSessionServer(t)

	first, err := server.agent().Run(context.Background(), RunOpts{Prompt: "fix it", CWD: t.TempDir(), Session: &SessionRef{}})
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	_, err = server.agent().Run(context.Background(), RunOpts{
		Prompt:  "fix it again",
		CWD:     t.TempDir(),
		Session: &SessionRef{ID: first.SessionID, Agent: "opencode"},
	})
	if err == nil || !strings.Contains(err.Error(), "different directory") {
		t.Fatalf("expected a directory mismatch error, got %v", err)
	}
	if n := server.count("POST /session/" + first.SessionID + "/message"); n != 1 {
		t.Fatalf("sent %d turns, want only the first: tools would run in the wrong worktree", n)
	}
}

func TestOpencodeAgent_ResumeRejectsAnInvalidSessionIDBeforeAnyRequest(t *testing.T) {
	server := newOpencodeSessionServer(t)

	_, err := server.agent().Run(context.Background(), RunOpts{
		Prompt:  "fix it",
		CWD:     t.TempDir(),
		Session: &SessionRef{ID: "../../config", Agent: "opencode"},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid opencode session identity") {
		t.Fatalf("expected an invalid identity error, got %v", err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.requests) != 0 {
		t.Fatalf("an invalid identity must not reach opencode, got %v", server.requests)
	}
}

func TestOpencodeAgent_ColdRunStillDeletesItsSession(t *testing.T) {
	server := newOpencodeSessionServer(t)

	result, err := server.agent().Run(context.Background(), RunOpts{Prompt: "review", CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.SessionID != "" {
		t.Fatalf("a cold run must not report a resumable session, got %q", result.SessionID)
	}
	if server.count("DELETE /session/ses_test1") != 1 {
		t.Fatal("a cold run must delete its single-use session")
	}
}

func TestOpencodeAgent_FailedTurnCleanup(t *testing.T) {
	t.Run("new durable session is deleted", func(t *testing.T) {
		server := newOpencodeSessionServer(t, opencodeNotRetryableFailure)

		_, err := server.agent().Run(context.Background(), RunOpts{Prompt: "fix it", CWD: t.TempDir(), Session: &SessionRef{}})
		if err == nil {
			t.Fatal("expected the turn to fail")
		}
		if server.exists("ses_test1") {
			t.Fatal("a session the pipeline never records must not be left behind")
		}
	})

	t.Run("resumed session is kept and reported", func(t *testing.T) {
		server := newOpencodeSessionServer(t, `{"info":{"id":"msg1","role":"assistant","tokens":{"input":10,"output":5}},"parts":[{"type":"text","text":"done"}]}`, opencodeNotRetryableFailure)
		cwd := t.TempDir()

		first, err := server.agent().Run(context.Background(), RunOpts{Prompt: "fix it", CWD: cwd, Session: &SessionRef{}})
		if err != nil {
			t.Fatalf("first turn: %v", err)
		}
		result, err := server.agent().Run(context.Background(), RunOpts{
			Prompt:  "fix it again",
			CWD:     cwd,
			Session: &SessionRef{ID: first.SessionID, Agent: "opencode"},
		})
		if err == nil {
			t.Fatal("expected the resumed turn to fail")
		}
		if result == nil || result.SessionID != first.SessionID || result.Resumed {
			t.Fatalf("failed resume must report the served session without claiming success, got %+v", result)
		}
		if !server.exists(first.SessionID) {
			t.Fatal("a failed turn must not delete the session it resumed")
		}
	})
}
