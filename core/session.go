package core

import (
	"context"
	"sync"
)

// Session is an in-process multi-turn conversation with an [Agent]: each Run or RunStream
// continues where the previous one left off, and the full transcript — system
// prompt, user tasks, assistant replies, tool calls and results — is always
// available via [Session.Messages]. A plain [Agent.Run] is one-shot and
// discards its conversation; a Session is the stateful layer on top.
//
// The transcript survives failures: if a run errors mid-way (provider
// failure, [ErrMaxStepsExceeded], cancellation), everything up to the failure
// is recorded, so the run can be audited and the conversation continued. When
// a parallel tool batch aborts, every requested call is committed with either
// its actual result or a synthetic aborted/canceled error result before the
// fatal error is returned. Cancellation is cooperative: a tool may complete an
// external side effect while cancellation races its return, and a synthetic
// canceled result does not imply that side effect was rolled back.
//
// Session remains useful as the conversation abstraction. This implementation
// is process-local; Runtime-backed durable conversations will replace its
// manual persistence path. Until then, [Message] marshals to JSON, so store
// session.Messages() anywhere and rebuild with [Agent.ResumeSession]:
//
//	blob, _ := json.Marshal(session.Messages())
//	// ... later, possibly in another process ...
//	var transcript []core.Message
//	_ = json.Unmarshal(blob, &transcript)
//	session, err := agent.ResumeSession(transcript)
//	if err != nil { /* report invalid history */ }
//
// Note that [Message.Meta] is excluded from JSON (it is transport-scoped) and
// does not round-trip.
//
// Concurrency: a Session is safe for concurrent use. Runs are serialized — a
// Run, RunStream, or [RunSessionTyped] call blocks until the previous operation
// finishes — and Messages may be called from any goroutine (it reflects the
// transcript as of the latest committed checkpoint, including during callbacks).
type Session struct {
	agent *Agent

	// runMu serializes runs: interleaving two conversations into one
	// transcript would corrupt both.
	runMu sync.Mutex
	// mu guards messages for snapshot readers while a run is in flight.
	mu       sync.Mutex
	messages []Message
}

// NewSession returns an empty Session for the agent. The first run seeds the
// transcript with the agent's system prompt, exactly like [Agent.Run].
func (a *Agent) NewSession() *Session {
	return &Session{agent: a}
}

// ResumeSession rebuilds a Session from a previously saved transcript
// (normally the output of [Session.Messages]). The transcript is used
// verbatim — it already contains its system message, so the agent's system
// prompt is not re-applied. An empty transcript behaves like [Agent.NewSession].
func (a *Agent) ResumeSession(transcript []Message) (*Session, error) {
	if err := validateHistory(transcript); err != nil {
		return nil, err
	}
	return &Session{
		agent:    a,
		messages: cloneMessages(transcript),
	}, nil
}

// Run continues the conversation with task and returns the [RunResult], like
// [Agent.Run] but with history. The transcript is updated even when an error is
// returned.
func (s *Session) Run(ctx context.Context, task string, opts ...RunOption) (RunResult, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.run(ctx, task, opts...)
}

// run continues the session while its caller holds runMu. Keeping the locking
// boundary separate lets compound operations such as RunSessionTyped serialize
// both their initial run and forced fallback as one conversation operation.
func (s *Session) run(ctx context.Context, task string, opts ...RunOption) (RunResult, error) {
	cfg := s.agent.newRunConfig(opts)
	scope, err := s.agent.beginRun(ctx, cfg, s.Messages(), s.commit, "sync")
	if err != nil {
		return scope.finish(scope.result, err)
	}
	out, err := s.runPhase(scope, task, cfg)
	return scope.finish(out, err)
}

// RunStream continues the conversation like [Session.Run] while delivering
// the live event stream to onEvent, with the same contract as
// [Agent.RunStream]. The transcript is updated even when an error is returned.
func (s *Session) RunStream(ctx context.Context, task string, onEvent func(StreamEvent), opts ...RunOption) (RunResult, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.runStream(ctx, task, onEvent, opts...)
}

func (s *Session) runStream(ctx context.Context, task string, onEvent func(StreamEvent), opts ...RunOption) (RunResult, error) {
	cfg := s.agent.newRunConfig(opts)
	scope, err := s.agent.beginRun(ctx, cfg, s.Messages(), s.commit, "stream")
	if err != nil {
		return scope.finish(scope.result, err)
	}
	cfg.scope = scope
	out, err := s.agent.runStream(scope.ctx, newLoop(s.agent, s.Messages()), task, onEvent, cfg)
	return scope.finish(out, err)
}

// Messages returns a detached snapshot of the latest committed checkpoint.
// Nested blocks, bytes, and usage may be changed without affecting the session.
func (s *Session) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneMessages(s.messages)
}

// commit replaces the transcript at a reconciled canonical boundary.
func (s *Session) commit(messages []Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = cloneMessages(messages)
}

func (s *Session) runPhase(scope *runScope, task string, cfg runConfig) (RunResult, error) {
	cfg.scope = scope
	if scope.turns >= scope.config.maxTurns {
		return scope.result, ErrMaxStepsExceeded
	}
	return s.agent.runSync(scope.ctx, newLoop(s.agent, s.Messages()), task, cfg)
}
