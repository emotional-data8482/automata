package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/emotional-data/automata/core"
)

// --- bridge messages handled by the TUI -----------------------------------

// uiEvent wraps a stream event from the orchestrator run. Sub-agent events
// arrive with ev.Agent set (the sub-agent tool's name) via core.AsTool.
type uiEvent struct{ ev core.StreamEvent }

// doneMsg is pushed when the orchestrator run returns.
type doneMsg struct {
	output string
	err    error
}

// waitForMsg blocks on the shared channel and surfaces the next message. It is
// re-issued after each handled message so exactly one reader is ever pending.
func waitForMsg(sub chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-sub }
}

// --- styles ---------------------------------------------------------------

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	headerStyle = lipgloss.NewStyle().Bold(true)
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	ruleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	okStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	errStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))

	orchestratorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	researcherStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	writerStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
)

func agentLabel(agent string) string {
	if agent == "" {
		return "orchestrator"
	}
	return agent
}

func agentStyle(agent string) lipgloss.Style {
	switch agent {
	case "researcher":
		return researcherStyle
	case "writer":
		return writerStyle
	default:
		return orchestratorStyle
	}
}

// --- phases & model -------------------------------------------------------

type phase int

const (
	phaseInput phase = iota
	phaseRunning
	phaseDone
)

type model struct {
	cfg    appConfig
	phase  phase
	sub    chan tea.Msg
	ctx    context.Context
	cancel context.CancelFunc
	build  func(topic string) *core.Agent

	input    textinput.Model
	spinner  spinner.Model
	viewport viewport.Model

	topic string
	todos []todoItem
	// acc folds the run's stream events into per-agent views; the TUI just
	// renders acc.Views() and acc.Totals(). A pointer so every bubbletea
	// model copy shares the same accumulator.
	acc *core.StreamAccumulator

	savedPath string
	output    string
	err       error

	start   time.Time
	elapsed time.Duration
	width   int
	height  int
}

func newModel(cfg appConfig, sub chan tea.Msg, ctx context.Context, cancel context.CancelFunc, build func(string) *core.Agent) model {
	ti := textinput.New()
	ti.Placeholder = "e.g. the impact of GLP-1 drugs on US healthcare costs"
	ti.Focus()
	ti.CharLimit = 200
	ti.Width = 60

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = dimStyle

	ph := phaseInput
	if cfg.topic != "" {
		ph = phaseRunning
	}

	return model{
		cfg:      cfg,
		phase:    ph,
		sub:      sub,
		ctx:      ctx,
		cancel:   cancel,
		build:    build,
		input:    ti,
		spinner:  sp,
		viewport: viewport.New(80, 20),
		topic:    cfg.topic,
		acc:      &core.StreamAccumulator{},
		start:    time.Now(),
	}
}

func (m model) Init() tea.Cmd {
	if m.phase == phaseRunning {
		return tea.Batch(m.spinner.Tick, waitForMsg(m.sub), m.startRun(m.topic))
	}
	return tea.Batch(textinput.Blink, waitForMsg(m.sub))
}

// startRun builds the orchestrator for the topic and launches it in a
// goroutine, forwarding every stream event onto the shared channel and a final
// doneMsg when it returns.
func (m model) startRun(topic string) tea.Cmd {
	return func() tea.Msg {
		orch := m.build(topic)
		go func() {
			out, err := orch.RunStream(m.ctx, topic, func(ev core.StreamEvent) {
				m.sub <- uiEvent{ev: ev}
			})
			m.sub <- doneMsg{output: out, err: err}
		}()
		return nil
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.Width = max(20, msg.Width-4)
		m.resize()
		return m, nil

	case tea.KeyMsg:
		if s := msg.String(); s == "ctrl+c" || s == "esc" {
			m.cancel()
			return m, tea.Quit
		}
		if m.phase == phaseInput {
			if msg.String() == "enter" {
				topic := strings.TrimSpace(m.input.Value())
				if topic == "" {
					return m, nil
				}
				m.topic = topic
				m.phase = phaseRunning
				m.start = time.Now()
				return m, tea.Batch(m.spinner.Tick, m.startRun(topic))
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
		if m.phase == phaseDone {
			if s := msg.String(); s == "q" || s == "enter" {
				return m, tea.Quit
			}
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd

	case spinner.TickMsg:
		if m.phase == phaseDone {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case uiEvent:
		m.acc.Add(msg.ev)
		m.resize()
		return m, waitForMsg(m.sub)

	case todoMsg:
		m.todos = msg.items
		m.resize()
		return m, waitForMsg(m.sub)

	case savedMsg:
		m.savedPath = msg.path
		return m, waitForMsg(m.sub)

	case doneMsg:
		m.phase = phaseDone
		m.output = msg.output
		m.err = msg.err
		m.elapsed = time.Since(m.start)
		return m, nil
	}
	return m, nil
}

// resize recomputes the viewport dimensions around the header, to-do panel, and
// footer, then refreshes its content and follows the tail.
func (m *model) resize() {
	if m.width == 0 {
		return
	}
	reserved := 3 // title + rule + footer
	if len(m.todos) > 0 {
		reserved += 1 + len(m.todos)
	}
	vh := max(3, m.height-reserved)
	m.viewport.Width = m.width
	m.viewport.Height = vh
	m.viewport.SetContent(m.renderLog())
	m.viewport.GotoBottom()
}

// renderLog renders the accumulator snapshot as one panel per agent (the
// orchestrator first, sub-agents in first-seen order): the agent's streamed
// text, then each tool call with its result. The per-event bookkeeping the
// TUI used to hand-roll lives in core.StreamAccumulator now.
func (m model) renderLog() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	wrap := lipgloss.NewStyle().Width(width)
	var b strings.Builder
	for i, v := range m.acc.Views() {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(agentStyle(v.Agent).Render("[" + agentLabel(v.Agent) + "]"))
		if text := strings.TrimSpace(v.Text); text != "" {
			b.WriteByte('\n')
			b.WriteString(wrap.Render(text))
		}
		for _, tc := range v.ToolCalls {
			b.WriteByte('\n')
			b.WriteString(wrap.Render("→ " + tc.Call.Function.Name + "(" + snippet(tc.Call.Function.Arguments, 80) + ")"))
			if !tc.Done {
				continue
			}
			b.WriteByte('\n')
			if tc.Err != nil {
				b.WriteString(wrap.Render(errStyle.Render("← error: " + tc.Err.Error())))
			} else {
				b.WriteString(wrap.Render(dimStyle.Render("← " + snippet(tc.Result, 200))))
			}
		}
	}
	return b.String()
}

func (m model) View() string {
	if m.phase == phaseInput {
		return m.inputView()
	}
	return m.runningView()
}

func (m model) inputView() string {
	return fmt.Sprintf("%s\n\nWhat should I research?\n\n%s\n\n%s",
		titleStyle.Render("🔬 Deep Research"),
		m.input.View(),
		dimStyle.Render("enter to start · ctrl+c to quit"),
	)
}

func (m model) runningView() string {
	rule := ruleStyle.Render(strings.Repeat("─", max(10, m.width)))

	var b strings.Builder
	b.WriteString(titleStyle.Render("🔬 Deep Research"))
	b.WriteString(" ")
	b.WriteString(dimStyle.Render(m.topic))
	b.WriteByte('\n')
	b.WriteString(rule)
	b.WriteByte('\n')
	if len(m.todos) > 0 {
		b.WriteString(headerStyle.Render("To-do:"))
		b.WriteByte('\n')
		for _, it := range m.todos {
			if it.Done {
				b.WriteString(dimStyle.Render(" [x] " + it.Text))
			} else {
				b.WriteString(" [ ] ")
				b.WriteString(it.Text)
			}
			b.WriteByte('\n')
		}
	}
	b.WriteString(m.viewport.View())
	b.WriteByte('\n')
	b.WriteString(rule)
	b.WriteByte('\n')
	b.WriteString(m.footer())
	return b.String()
}

func (m model) footer() string {
	elapsed := m.elapsed
	if m.phase != phaseDone {
		elapsed = time.Since(m.start)
	}
	done := 0
	for _, it := range m.todos {
		if it.Done {
			done++
		}
	}
	searches := 0
	for _, v := range m.acc.Views() {
		for _, tc := range v.ToolCalls {
			if tc.Call.Function.Name == "web_search" {
				searches++
			}
		}
	}
	totals := m.acc.Totals()
	stats := dimStyle.Render(fmt.Sprintf("searches %d · todos %d/%d · ⏱ %s · tokens ▲%d ▼%d",
		searches, done, len(m.todos), elapsed.Truncate(time.Second), totals.InputTokens, totals.OutputTokens))

	if m.phase == phaseDone {
		var head string
		switch {
		case m.err != nil:
			head = errStyle.Render("✗ " + m.err.Error())
		case m.savedPath != "":
			head = okStyle.Render("✓ Saved to " + m.savedPath)
		default:
			head = okStyle.Render("✓ Done")
		}
		return head + "\n" + stats + "  " + dimStyle.Render("press q to quit")
	}
	return m.spinner.View() + " working   " + stats
}
