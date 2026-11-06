// Package app translates between an OpenCode server and SlideBolt. Each configured
// agent is a device. It holds no behavior: scripts decide what to ask and what to do
// with the answer.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slidebolt/plugin-opencode/internal/hostapi"
	contract "github.com/slidebolt/sb-contract"
	domain "github.com/slidebolt/sb-domain"
	messenger "github.com/slidebolt/sb-messenger-sdk"
	storage "github.com/slidebolt/sb-storage-sdk"
)

const (
	PluginID = "plugin-opencode"

	devicePrefix   = "agent_"
	entityPrompt   = "prompt"
	entityOnline   = "availability"
	defaultTimeout = 120
	healthInterval = 15 * time.Second
	defaultSession = "default"
	queueSize      = 16
)

var (
	agentCommands = []string{"agent_prompt", "agent_cancel"}
	agentName     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// App is the importable runtime for the plugin-opencode binary.
type App struct {
	msg    messenger.Messenger
	store  storage.Storage
	cmds   *messenger.Commands
	subs   []messenger.Subscription
	host   *hostapi.Client
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	limit  time.Duration

	agents map[string]*agent
}

// agent runs one prompt at a time, in order.
type agent struct {
	name  string
	queue chan domain.AgentPrompt

	mu       sync.Mutex
	state    domain.Agent
	online   bool
	sessions map[string]string
	abort    context.CancelFunc
	running  string
}

func New() *App {
	return &App{agents: map[string]*agent{}}
}

func (a *App) Hello() contract.HelloResponse {
	return contract.HelloResponse{
		ID:              PluginID,
		Kind:            contract.KindPlugin,
		ContractVersion: contract.ContractVersion,
		DependsOn:       []string{"messenger", "storage"},
	}
}

// OnStart stays up when the host is unreachable or unconfigured. A failed start
// aborts sb-manager's startAll, which would stop every other binary.
func (a *App) OnStart(deps map[string]json.RawMessage) (json.RawMessage, error) {
	msg, err := messenger.Connect(deps)
	if err != nil {
		return nil, fmt.Errorf("connect messenger: %w", err)
	}
	a.msg = msg
	store, err := storage.Connect(deps)
	if err != nil {
		return nil, fmt.Errorf("connect storage: %w", err)
	}
	a.store = store

	a.cmds = messenger.NewCommands(msg, domain.LookupCommand)
	sub, err := a.cmds.Receive(PluginID+".>", a.handleCommand)
	if err != nil {
		return nil, fmt.Errorf("subscribe commands: %w", err)
	}
	a.subs = append(a.subs, sub)
	a.ctx, a.cancel = context.WithCancel(context.Background())

	a.limit = time.Duration(defaultTimeout) * time.Second
	if v, err := strconv.Atoi(os.Getenv("OPENCODE_PROMPT_TIMEOUT_SECONDS")); err == nil && v > 0 {
		a.limit = time.Duration(v) * time.Second
	}
	for _, name := range strings.Split(os.Getenv("OPENCODE_AGENTS"), ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !agentName.MatchString(name) {
			log.Printf("plugin-opencode: ignoring agent with a bad name: %q", name)
			continue
		}
		a.agents[name] = &agent{
			name: name, queue: make(chan domain.AgentPrompt, queueSize), sessions: map[string]string{},
			state: domain.Agent{Status: "idle"},
		}
	}

	host, err := hostapi.New(os.Getenv("OPENCODE_HOST_URL"), os.Getenv("OPENCODE_HOST_PASSWORD"))
	if err != nil {
		log.Printf("plugin-opencode: disabled: %v", err)
		for _, ag := range a.agents {
			ag.state.Status = "error"
			ag.state.Error = err.Error()
		}
	} else {
		a.host = host
	}
	for _, ag := range a.agents {
		a.save(ag)
		if a.host != nil {
			a.wg.Add(1)
			go a.worker(ag)
		}
	}
	if a.host != nil {
		a.wg.Add(1)
		go a.watchHealth()
	}
	log.Printf("plugin-opencode: started with %d agents", len(a.agents))
	return nil, nil
}

func (a *App) OnShutdown() error {
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	for _, sub := range a.subs {
		sub.Unsubscribe()
	}
	if a.store != nil {
		a.store.Close()
	}
	if a.msg != nil {
		a.msg.Close()
	}
	return nil
}

func (a *App) save(ag *agent) {
	ag.mu.Lock()
	state, online := ag.state, ag.online
	ag.mu.Unlock()
	d := devicePrefix + ag.name
	prompt := domain.Entity{
		ID: entityPrompt, Plugin: PluginID, DeviceID: d, Type: "agent", Name: ag.name,
		Commands: agentCommands, State: state,
	}
	availability := domain.Entity{
		ID: entityOnline, Plugin: PluginID, DeviceID: d, Type: "binary_sensor", Name: "Availability",
		State: domain.BinarySensor{On: online, DeviceClass: "connectivity"},
	}
	device := domain.Device{ID: d, Plugin: PluginID, Name: "Agent " + ag.name, Entities: []domain.Entity{availability, prompt}}
	for _, v := range []storage.Keyed{device, availability, prompt} {
		if err := a.store.Save(v); err != nil {
			log.Printf("plugin-opencode: save %s: %v", v.Key(), err)
		}
	}
}

func (a *App) watchHealth() {
	defer a.wg.Done()
	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
		err := a.host.Health(ctx)
		cancel()
		if a.ctx.Err() != nil {
			return
		}
		for _, ag := range a.agents {
			ag.mu.Lock()
			changed := ag.online != (err == nil)
			ag.online = err == nil
			ag.mu.Unlock()
			if changed {
				a.save(ag)
			}
		}
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) handleCommand(addr messenger.Address, cmd any) {
	name, ok := strings.CutPrefix(addr.DeviceID, devicePrefix)
	if !ok || addr.EntityID != entityPrompt {
		return
	}
	ag := a.agents[name]
	if ag == nil {
		log.Printf("plugin-opencode: unknown agent %q", name)
		return
	}
	switch c := cmd.(type) {
	case domain.AgentPrompt:
		a.enqueue(ag, c)
	case domain.AgentCancel:
		a.cancelPrompt(ag)
	}
}

func (a *App) enqueue(ag *agent, p domain.AgentPrompt) {
	if strings.TrimSpace(p.Text) == "" {
		return
	}
	if a.host == nil {
		log.Printf("plugin-opencode: %s: ignored, host is not configured", ag.name)
		return
	}
	select {
	case ag.queue <- p:
	default:
		ag.mu.Lock()
		ag.state.Error = "too many prompts waiting"
		ag.mu.Unlock()
		a.save(ag)
	}
}

func (a *App) cancelPrompt(ag *agent) {
	ag.mu.Lock()
	abort, session := ag.abort, ag.running
	ag.mu.Unlock()
	if abort == nil {
		return
	}
	abort()
	if a.host == nil || session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
	defer cancel()
	if err := a.host.Abort(ctx, session); err != nil {
		log.Printf("plugin-opencode: %s: abort: %v", ag.name, err)
	}
}

func (a *App) worker(ag *agent) {
	defer a.wg.Done()
	for {
		select {
		case <-a.ctx.Done():
			return
		case p := <-ag.queue:
			a.run(ag, p)
		}
	}
}

func (a *App) run(ag *agent, p domain.AgentPrompt) {
	session := p.Session
	if session == "" {
		session = defaultSession
	}
	ag.mu.Lock()
	ag.state.Status = "thinking"
	ag.state.Session = session
	ag.state.Prompt = p.Text
	ag.state.Result = ""
	ag.state.Error = ""
	ag.state.Seq++
	seq := ag.state.Seq
	ag.mu.Unlock()
	a.save(ag)

	ctx, cancel := context.WithTimeout(a.ctx, a.limit)
	defer cancel()
	result, err := a.ask(ctx, ag, session, p.Text, cancel)

	ag.mu.Lock()
	ag.abort, ag.running = nil, ""
	if ag.state.Seq == seq {
		if err != nil {
			ag.state.Status = "error"
			ag.state.Error = err.Error()
			if ctx.Err() == context.DeadlineExceeded {
				ag.state.Error = "timed out"
			} else if ctx.Err() == context.Canceled && a.ctx.Err() == nil {
				ag.state.Status = "idle"
				ag.state.Error = "cancelled"
			}
		} else {
			ag.state.Status = "idle"
			ag.state.Result = result
		}
	}
	ag.mu.Unlock()
	if a.ctx.Err() == nil {
		a.save(ag)
	}
}

func (a *App) ask(ctx context.Context, ag *agent, session, text string, cancel context.CancelFunc) (string, error) {
	ag.mu.Lock()
	id := ag.sessions[session]
	ag.abort = cancel
	ag.mu.Unlock()
	if id == "" {
		created, err := a.host.CreateSession(ctx, ag.name+" "+session)
		if err != nil {
			return "", err
		}
		id = created
		ag.mu.Lock()
		ag.sessions[session] = id
		ag.mu.Unlock()
	}
	ag.mu.Lock()
	ag.running = id
	ag.mu.Unlock()
	return a.host.Prompt(ctx, id, text)
}
