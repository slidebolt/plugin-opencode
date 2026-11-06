package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	domain "github.com/slidebolt/sb-domain"
	messenger "github.com/slidebolt/sb-messenger-sdk"
	storage "github.com/slidebolt/sb-storage-sdk"
	testkit "github.com/slidebolt/sb-testkit"
)

// fakeServer implements the part of opencode serve the plugin uses, with the shapes
// taken from the real server's OpenAPI document.
type fakeServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	sessions []string
	prompts  []string
	paths    []string
	reply    func(text string) (int, string)
	block    chan struct{}
	aborted  int
	healthy  bool
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{healthy: true}
	f.reply = func(string) (int, string) {
		return 200, `{"info":{"role":"assistant"},"parts":[{"type":"step-start"},{"type":"text","text":"The lights are on."}]}`
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.mu.Lock()
		if f.block != nil {
			close(f.block)
			f.block = nil
		}
		f.mu.Unlock()
		f.srv.CloseClientConnections()
		f.srv.Close()
	})
	return f
}

func (f *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != "opencode" || pass != "secret" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	switch {
	case r.URL.Path == "/global/health":
		f.mu.Lock()
		healthy := f.healthy
		f.mu.Unlock()
		if !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"healthy":true}`))
	case r.URL.Path == "/session":
		f.mu.Lock()
		id := fmt.Sprintf("ses_%d", len(f.sessions)+1)
		f.sessions = append(f.sessions, id)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"id": id})
	case strings.HasSuffix(r.URL.Path, "/abort"):
		f.mu.Lock()
		f.aborted++
		f.mu.Unlock()
		w.Write([]byte(`true`))
	case strings.HasSuffix(r.URL.Path, "/message"):
		var body struct {
			Parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"parts"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		text := body.Parts[0].Text
		f.mu.Lock()
		f.prompts = append(f.prompts, r.URL.Path+" "+text)
		block, reply := f.block, f.reply
		f.mu.Unlock()
		if block != nil {
			select {
			case <-block:
			case <-r.Context().Done():
				return
			}
		}
		code, out := reply(text)
		w.WriteHeader(code)
		w.Write([]byte(out))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

type harness struct {
	t     *testing.T
	f     *fakeServer
	store storage.Storage
	cmds  *messenger.Commands
	key   domain.EntityKey
}

func start(t *testing.T, mutate func(*fakeServer)) *harness {
	t.Helper()
	env := testkit.NewTestEnv(t)
	env.Start("messenger")
	env.Start("storage")
	f := newFakeServer(t)
	if mutate != nil {
		mutate(f)
	}
	t.Setenv("OPENCODE_HOST_URL", f.srv.URL)
	t.Setenv("OPENCODE_HOST_PASSWORD", "secret")
	t.Setenv("OPENCODE_AGENTS", "voice, BAD NAME")
	a := New()
	if _, err := a.OnStart(map[string]json.RawMessage{"messenger": env.MessengerPayload()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.OnShutdown() })
	return &harness{t: t, f: f, store: env.Storage(), cmds: messenger.NewCommands(env.Messenger(), domain.LookupCommand),
		key: domain.EntityKey{Plugin: PluginID, DeviceID: "agent_voice", ID: "prompt"}}
}

func (h *harness) state() domain.Agent {
	h.t.Helper()
	raw, err := h.store.Get(h.key)
	if err != nil {
		h.t.Fatal(err)
	}
	var e domain.Entity
	if err := json.Unmarshal(raw, &e); err != nil {
		h.t.Fatal(err)
	}
	return e.State.(domain.Agent)
}

func (h *harness) eventually(what string, ok func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s; state %+v", what, h.state())
}

func (h *harness) prompt(text, session string) {
	h.t.Helper()
	if err := h.cmds.Send(h.key, domain.AgentPrompt{Text: text, Session: session}); err != nil {
		h.t.Fatal(err)
	}
}

func TestAgentsComeFromConfig(t *testing.T) {
	h := start(t, nil)
	raw, err := h.store.Get(h.key)
	if err != nil {
		t.Fatal(err)
	}
	var e domain.Entity
	json.Unmarshal(raw, &e)
	if e.Type != "agent" || len(e.Commands) != 2 || e.State.(domain.Agent).Status != "idle" {
		t.Fatalf("entity = %+v", e)
	}
	if entries, _ := h.store.Search(PluginID + ".agent_BAD NAME.>"); len(entries) != 0 {
		t.Fatal("a bad name must be ignored")
	}
	h.eventually("availability", func() bool {
		raw, err := h.store.Get(domain.EntityKey{Plugin: PluginID, DeviceID: "agent_voice", ID: "availability"})
		if err != nil {
			return false
		}
		var a domain.Entity
		json.Unmarshal(raw, &a)
		return a.State.(domain.BinarySensor).On
	})
}

func TestPromptProducesResult(t *testing.T) {
	h := start(t, nil)
	h.prompt("turn the lights on", "voice")
	h.eventually("result", func() bool { return h.state().Result == "The lights are on." })
	s := h.state()
	if s.Status != "idle" || s.Prompt != "turn the lights on" || s.Session != "voice" || s.Seq != 1 || s.Error != "" {
		t.Fatalf("state = %+v", s)
	}
}

func TestSessionsAreReusedByName(t *testing.T) {
	h := start(t, nil)
	h.prompt("one", "voice")
	h.eventually("first", func() bool { return h.state().Seq == 1 && h.state().Status == "idle" })
	h.prompt("two", "voice")
	h.eventually("second", func() bool { return h.state().Seq == 2 && h.state().Status == "idle" })
	h.prompt("three", "other")
	h.eventually("third", func() bool { return h.state().Seq == 3 && h.state().Status == "idle" })
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	if len(h.f.sessions) != 2 {
		t.Fatalf("sessions = %v", h.f.sessions)
	}
	if !strings.HasPrefix(h.f.prompts[1], "/session/ses_1/") || !strings.HasPrefix(h.f.prompts[2], "/session/ses_2/") {
		t.Fatalf("prompts = %v", h.f.prompts)
	}
}

func TestPromptsRunInOrderOneAtATime(t *testing.T) {
	h := start(t, func(f *fakeServer) { f.block = make(chan struct{}) })
	h.prompt("one", "voice")
	h.eventually("thinking", func() bool { return h.state().Status == "thinking" })
	h.prompt("two", "voice")
	time.Sleep(50 * time.Millisecond)
	h.f.mu.Lock()
	running := len(h.f.prompts)
	close(h.f.block)
	h.f.block = nil
	h.f.mu.Unlock()
	if running != 1 {
		t.Fatalf("%d prompts ran at once", running)
	}
	h.eventually("both", func() bool { return h.state().Seq == 2 && h.state().Status == "idle" })
	if s := h.state(); s.Prompt != "two" {
		t.Fatalf("state = %+v", s)
	}
}

func TestErrorsAreVisible(t *testing.T) {
	cases := map[string]struct {
		code int
		body string
		want string
	}{
		"http":  {500, `boom`, "HTTP 500"},
		"model": {200, `{"info":{"error":{"name":"ProviderAuthError","data":{"message":"bad key"}}},"parts":[]}`, "bad key"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := start(t, func(f *fakeServer) { f.reply = func(string) (int, string) { return tc.code, tc.body } })
			h.prompt("x", "s")
			h.eventually("error", func() bool { return h.state().Status == "error" })
			if s := h.state(); !strings.Contains(s.Error, tc.want) || s.Result != "" {
				t.Fatalf("state = %+v", s)
			}
		})
	}
}

func TestErrorClearsOnNextPrompt(t *testing.T) {
	calls := 0
	h := start(t, func(f *fakeServer) {
		f.reply = func(string) (int, string) {
			calls++
			if calls == 1 {
				return 500, "boom"
			}
			return 200, `{"info":{},"parts":[{"type":"text","text":"ok"}]}`
		}
	})
	h.prompt("x", "s")
	h.eventually("error", func() bool { return h.state().Status == "error" })
	h.prompt("y", "s")
	h.eventually("recovery", func() bool { s := h.state(); return s.Status == "idle" && s.Result == "ok" && s.Error == "" })
}

func TestCancelStopsPromptAndAbortsSession(t *testing.T) {
	h := start(t, func(f *fakeServer) { f.block = make(chan struct{}) })
	h.prompt("long", "s")
	h.eventually("running", func() bool {
		h.f.mu.Lock()
		defer h.f.mu.Unlock()
		return len(h.f.prompts) == 1
	})
	if err := h.cmds.Send(h.key, domain.AgentCancel{}); err != nil {
		t.Fatal(err)
	}
	h.eventually("cancelled", func() bool { s := h.state(); return s.Status == "idle" && s.Error == "cancelled" })
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	if h.f.aborted != 1 {
		t.Fatalf("aborted = %d", h.f.aborted)
	}
}

func TestTimeoutIsReported(t *testing.T) {
	t.Setenv("OPENCODE_PROMPT_TIMEOUT_SECONDS", "1")
	h := start(t, func(f *fakeServer) { f.block = make(chan struct{}) })
	h.prompt("long", "s")
	h.eventually("timeout", func() bool { s := h.state(); return s.Status == "error" && s.Error == "timed out" })
}

func TestEmptyPromptAndOtherEntitiesAreIgnored(t *testing.T) {
	h := start(t, nil)
	h.prompt("   ", "s")
	h.cmds.Send(domain.EntityKey{Plugin: PluginID, DeviceID: "agent_nobody", ID: "prompt"}, domain.AgentPrompt{Text: "x"})
	h.cmds.Send(domain.EntityKey{Plugin: PluginID, DeviceID: "agent_voice", ID: "availability"}, domain.AgentPrompt{Text: "x"})
	time.Sleep(100 * time.Millisecond)
	if s := h.state(); s.Seq != 0 {
		t.Fatalf("state = %+v", s)
	}
}

func TestServerDownIsVisible(t *testing.T) {
	h := start(t, nil)
	h.f.srv.Close()
	h.prompt("x", "s")
	h.eventually("error", func() bool { return h.state().Status == "error" })
}

// A failed OnStart aborts sb-manager's startAll and stops every binary.
func TestOnStart_MissingConfigStaysUp(t *testing.T) {
	env := testkit.NewTestEnv(t)
	env.Start("messenger")
	env.Start("storage")
	t.Setenv("OPENCODE_HOST_URL", "")
	t.Setenv("OPENCODE_HOST_PASSWORD", "")
	t.Setenv("OPENCODE_AGENTS", "voice")
	a := New()
	if _, err := a.OnStart(map[string]json.RawMessage{"messenger": env.MessengerPayload()}); err != nil {
		t.Fatalf("OnStart must not fail: %v", err)
	}
	defer a.OnShutdown()
	h := &harness{t: t, store: env.Storage(), key: domain.EntityKey{Plugin: PluginID, DeviceID: "agent_voice", ID: "prompt"}}
	if s := h.state(); s.Status != "error" || s.Error == "" {
		t.Fatalf("state = %+v", s)
	}
}

func TestHello(t *testing.T) {
	h := New().Hello()
	if h.ID != PluginID || h.Validate() != nil {
		t.Fatalf("hello = %+v", h)
	}
}
