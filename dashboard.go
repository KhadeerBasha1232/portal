package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The live dashboard: a Bubble Tea program on stderr that redraws a few
// times a second from the sharer's / opener's state. share.go and open.go
// talk to it through teaReporter, exactly like they talk to plainReporter.

const (
	maxSteps = 4  // status lines kept on screen
	maxReqs  = 8  // request log lines kept on screen
	history  = 40 // seconds of throughput in the sparkline
)

type (
	stepMsg struct {
		text string
		warn bool
	}
	spinMsg string
	reqMsg  reqEntry
	askMsg  struct {
		q     string
		reply chan bool
	}
	unaskMsg struct{}
	tickMsg  time.Time
	doneMsg  struct{ err error }
	readyMsg struct {
		sh *sharer
		op *opener
	}
)

type teaReporter struct{ p *tea.Program }

func (r teaReporter) Step(s string)      { r.p.Send(stepMsg{text: s}) }
func (r teaReporter) Warn(s string)      { r.p.Send(stepMsg{text: s, warn: true}) }
func (r teaReporter) Spin(s string)      { r.p.Send(spinMsg(s)) }
func (r teaReporter) Request(e reqEntry) { r.p.Send(reqMsg(e)) }
func (r teaReporter) Ask(ctx context.Context, q string) bool {
	reply := make(chan bool, 1)
	r.p.Send(askMsg{q, reply})
	select {
	case ok := <-reply:
		return ok
	case <-ctx.Done():
		r.p.Send(unaskMsg{})
		return false
	}
}

type dash struct {
	sharing bool
	share   shareOpts
	sh      *sharer
	op      *opener
	cancel  context.CancelFunc

	steps    []stepMsg
	spin     string
	reqs     []reqEntry
	ask      *askMsg
	frame    int
	width    int
	stopping bool
	done     bool

	// throughput, sampled once a second
	samples         []float64
	lastIn, lastOut int64
	inRate, outRate float64
	lastSample      time.Time
}

func (m *dash) Init() tea.Cmd { return tick() }

func tick() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *dash) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.ask != nil {
			switch msg.String() {
			case "y", "Y", "enter":
				m.answer(true)
			case "n", "N", "esc":
				m.answer(false)
			}
		}
		switch msg.String() {
		case "ctrl+c", "q":
			if m.ask != nil {
				m.answer(false)
			}
			m.stopping = true
			m.cancel()
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case readyMsg:
		m.sh, m.op = msg.sh, msg.op
	case stepMsg:
		m.steps = append(m.steps, msg)
		if len(m.steps) > maxSteps {
			m.steps = m.steps[len(m.steps)-maxSteps:]
		}
	case spinMsg:
		m.spin = string(msg)
	case reqMsg:
		m.reqs = append(m.reqs, reqEntry(msg))
		if len(m.reqs) > maxReqs {
			m.reqs = m.reqs[len(m.reqs)-maxReqs:]
		}
	case askMsg:
		m.ask = &msg
	case unaskMsg:
		m.ask = nil
	case tickMsg:
		m.frame++
		m.sample(time.Time(msg))
		return m, tick()
	case doneMsg:
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

func (m *dash) answer(ok bool) {
	m.ask.reply <- ok
	m.ask = nil
}

func (m *dash) stats() *stats {
	switch {
	case m.sh != nil:
		return m.sh.stats
	case m.op != nil:
		return m.op.stats
	}
	return nil
}

func (m *dash) sample(now time.Time) {
	st := m.stats()
	if st == nil || now.Sub(m.lastSample) < time.Second {
		return
	}
	in, outB := st.in.Load(), st.out.Load()
	if !m.lastSample.IsZero() {
		dt := now.Sub(m.lastSample).Seconds()
		m.inRate = float64(in-m.lastIn) / dt
		m.outRate = float64(outB-m.lastOut) / dt
		m.samples = append(m.samples, m.inRate+m.outRate)
		if len(m.samples) > history {
			m.samples = m.samples[len(m.samples)-history:]
		}
	}
	m.lastIn, m.lastOut, m.lastSample = in, outB, now
}

// ---------------------------------------------------------------- views

var (
	sLabel = re.NewStyle().Foreground(colAccent).Bold(true)
	sKey   = re.NewStyle().Foreground(lipgloss.Color("#E5E7EB")).Bold(true)
)

func (m *dash) View() string {
	if m.sharing {
		return m.shareView()
	}
	return m.openView()
}

func (m *dash) spinner() string {
	return sAccent.Render(spinnerFrames[m.frame%len(spinnerFrames)])
}

func (m *dash) shareView() string {
	var b strings.Builder
	line := func(s string) { b.WriteString(s + "\n") }

	line("")
	line(headerLine("sharing " + m.share.target))
	line("")
	line(box(sDim.Render("your code"), sCyan.Render(m.share.code)))
	line("  " + sDim.Render("Your friend runs ") + sAccent.Render("portal open "+m.share.code))
	line("")

	var guests []guestView
	if m.sh != nil {
		guests = m.sh.guestList()
	}
	line("  " + sLabel.Render("GUESTS") + "  " + sDim.Render(fmt.Sprintf("%d/%d", len(guests), m.share.maxGuests)))
	switch {
	case m.ask != nil:
		line("  " + sCyan.Render("●") + " " + sDim.Render("Someone with the right code is knocking · answer below"))
	case len(guests) == 0 && !m.done:
		line("  " + m.spinner() + " " + sDim.Render(orDefault(m.spin, "Waiting for your friend…")))
	}
	for _, g := range guests {
		conns := "idle"
		if g.Active > 0 {
			conns = fmt.Sprintf("%d open", g.Active)
		}
		line("  " + sOK.Render("●") + " " + sKey.Render(shortID(g.ID)) + "  " + sDim.Render(g.Conn) +
			"  " + humanDuration(time.Since(g.Since)) + "  " + sDim.Render(conns))
	}
	line("")
	m.trafficView(line, "from guests", "to guests")
	if st := m.stats(); st != nil && st.requests.Load() > 0 {
		line("")
		line("  " + sLabel.Render("REQUESTS") + "  " + sDim.Render(fmt.Sprintf("%d total", st.requests.Load())))
		for _, r := range m.reqs {
			line("  " + formatReq(r, m.pathWidth()))
		}
	}
	m.footer(line, "stop sharing")
	return b.String()
}

func (m *dash) openView() string {
	var b strings.Builder
	line := func(s string) { b.WriteString(s + "\n") }

	line("")
	line(headerLine("open a shared port"))
	line("")
	var v openView
	if m.op != nil {
		v = m.op.view()
	}
	switch {
	case v.Connected:
		url := "http://" + v.Local
		line(sOKBox.Render(sOK.Render("✓ Connected") + sDim.Render(" · open ") + sCyan.Render(url)))
		line("  " + sDim.Render(v.Conn+" · "+humanDuration(time.Since(v.Since))))
		line("  " + sDim.Render("Any TCP client works too: "+v.Local))
	case !m.done:
		line("  " + m.spinner() + " " + sDim.Render(orDefault(m.spin, "Looking for the sharer…")))
	}
	line("")
	if v.Local != "" {
		m.trafficView(line, "downloaded", "uploaded")
	}
	m.footer(line, "disconnect")
	return b.String()
}

func (m *dash) trafficView(line func(string), inLabel, outLabel string) {
	st := m.stats()
	if st == nil {
		return
	}
	line("  " + sLabel.Render("TRAFFIC") + "  " + sDim.Render(fmt.Sprintf("%d open · %s total", st.active.Load(), plural(st.conns.Load(), "connection"))))
	line("  " + sparkline(m.samples, history) + "  " + sDim.Render("last 40s"))
	row := func(arrow, label string, total int64, rate float64) {
		line("  " + sCyan.Render(arrow) + " " + sDim.Render(fmt.Sprintf("%-12s", label)) +
			sKey.Render(fmt.Sprintf("%10s", humanBytes(total))) + "  " + sDim.Render(humanBytes(int64(rate))+"/s"))
	}
	row("↓", inLabel, st.in.Load(), m.inRate)
	row("↑", outLabel, st.out.Load(), m.outRate)
}

func (m *dash) footer(line func(string), stop string) {
	if len(m.steps) > 0 {
		line("")
		for _, s := range m.steps {
			mark := sOK.Render("✓")
			if s.warn {
				mark = sWarn.Render("!")
			}
			line("  " + mark + " " + s.text)
		}
	}
	line("")
	switch {
	case m.ask != nil:
		line("  " + sCyan.Render("?") + " " + sBold.Render(m.ask.q) + " " + sDim.Render("[Y/n]"))
	case m.stopping && !m.done:
		line("  " + m.spinner() + " " + sDim.Render("Stopping…"))
	case !m.done:
		line("  " + sDim.Render("ctrl+c "+stop))
	}
}

func (m *dash) pathWidth() int {
	if m.width == 0 {
		return 32
	}
	return max(16, min(40, m.width-40))
}

func plural(n int64, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ---------------------------------------------------------------- running

func runDashboard(m *dash, core func(ctx context.Context, rep reporter) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.cancel = cancel
	p := tea.NewProgram(m, tea.WithOutput(os.Stderr))
	rep := teaReporter{p}
	errc := make(chan error, 1)
	go func() {
		err := core(ctx, rep)
		errc <- err
		p.Send(doneMsg{err})
	}()
	if _, err := p.Run(); err != nil {
		cancel()
		return err
	}
	cancel()
	return <-errc
}

func dashboardShare(ctx context.Context, opts shareOpts) error {
	m := &dash{sharing: true, share: opts}
	err := runDashboard(m, func(dctx context.Context, rep reporter) error {
		ctx, cancel := mergeCtx(ctx, dctx)
		defer cancel()
		return runShare(ctx, opts, rep, func(s *sharer) { rep.(teaReporter).p.Send(readyMsg{sh: s}) })
	})
	if err == nil {
		out.Println("  " + sOK.Render("Stopped sharing.") + "\n")
	}
	return err
}

func dashboardOpen(ctx context.Context, opts openOpts) error {
	m := &dash{}
	err := runDashboard(m, func(dctx context.Context, rep reporter) error {
		ctx, cancel := mergeCtx(ctx, dctx)
		defer cancel()
		return runOpen(ctx, opts, rep, func(o *opener) { rep.(teaReporter).p.Send(readyMsg{op: o}) })
	})
	if err == nil {
		out.Println("  " + sOK.Render("Disconnected.") + "\n")
	}
	return err
}

// mergeCtx is cancelled when either a or b is.
func mergeCtx(a, b context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(a)
	stop := context.AfterFunc(b, cancel)
	return ctx, func() { stop(); cancel() }
}
