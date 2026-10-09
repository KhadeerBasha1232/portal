package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/muesli/termenv"
	ma "github.com/multiformats/go-multiaddr"
	"golang.org/x/term"
)

// All UI goes to stderr. On a terminal, share/open run a live Bubble Tea
// dashboard (dashboard.go). Otherwise (piped, recorded, --plain) they print
// plain lines through this file; on a terminal there is one "live" spinner
// line at the bottom that permanent lines scroll above.

var (
	re = lipgloss.NewRenderer(os.Stderr)

	colAccent = lipgloss.Color("#8B5CF6")
	colCyan   = lipgloss.Color("#22D3EE")
	colGreen  = lipgloss.Color("#34D399")
	colYellow = lipgloss.Color("#FBBF24")
	colRed    = lipgloss.Color("#F87171")
	colDim    = lipgloss.Color("#7C8594")

	sLogo   = re.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(colAccent).Padding(0, 1)
	sDim    = re.NewStyle().Foreground(colDim)
	sBold   = re.NewStyle().Bold(true)
	sAccent = re.NewStyle().Foreground(colAccent).Bold(true)
	sCyan   = re.NewStyle().Foreground(colCyan).Bold(true)
	sOK     = re.NewStyle().Foreground(colGreen).Bold(true)
	sWarn   = re.NewStyle().Foreground(colYellow).Bold(true)
	sErr    = re.NewStyle().Foreground(colRed).Bold(true)
	sBox    = re.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colAccent).Padding(0, 3).MarginLeft(2)
	sOKBox  = re.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colGreen).Padding(0, 2).MarginLeft(2)
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// stderrTTY reports whether stderr is an interactive terminal.
var stderrTTY = term.IsTerminal(int(os.Stderr.Fd()))

type ui struct {
	mu     sync.Mutex
	tty    bool
	live   func() string
	drawn  bool
	frame  int
	asking bool     // a question is on screen: hold other lines until answered
	held   []string // lines printed while asking
}

var out = newUI()

func newUI() *ui {
	u := &ui{tty: stderrTTY}
	// CLICOLOR_FORCE=1 keeps full colors when output is piped or recorded.
	if v := os.Getenv("CLICOLOR_FORCE"); v != "" && v != "0" {
		re.SetColorProfile(termenv.TrueColor)
	}
	if u.tty {
		termenv.EnableVirtualTerminalProcessing(termenv.NewOutput(os.Stderr))
		go func() {
			for range time.Tick(100 * time.Millisecond) {
				u.mu.Lock()
				u.frame++
				u.redraw()
				u.mu.Unlock()
			}
		}()
	}
	return u
}

func (u *ui) redraw() {
	if !u.tty || u.live == nil {
		return
	}
	fmt.Fprint(os.Stderr, "\r\x1b[2K"+u.live())
	u.drawn = true
}

func (u *ui) clear() {
	if u.drawn {
		fmt.Fprint(os.Stderr, "\r\x1b[2K")
		u.drawn = false
	}
}

// Println prints a permanent line above the live line.
func (u *ui) Println(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.asking {
		u.held = append(u.held, s)
		return
	}
	u.clear()
	fmt.Fprintln(os.Stderr, s)
	u.redraw()
}

// Spin shows a spinner with text on the live line.
func (u *ui) Spin(text string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.tty {
		return // piped output: only permanent lines
	}
	u.live = func() string {
		return "  " + sAccent.Render(spinnerFrames[u.frame%len(spinnerFrames)]) + " " + sDim.Render(text)
	}
	u.redraw()
}

func (u *ui) StopLive() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.clear()
	u.live = nil
}

func failLine(err error) {
	out.StopLive()
	out.Println("\n  " + sErr.Render("✗ "+err.Error()))
	var h hinted
	if asHint(err, &h) {
		out.Println("    " + sDim.Render(h.hint))
	}
}

func headerLine(subtitle string) string {
	return "  " + sLogo.Render("◆ portal") + "  " + sDim.Render(subtitle)
}

func box(lines ...string) string {
	return sBox.Render(strings.Join(lines, "\n"))
}

// describeConn turns a libp2p connection into "direct · QUIC · LAN 192.168.1.5".
func describeConn(c network.Conn) string {
	addr := c.RemoteMultiaddr()
	s := addr.String()

	kind := "direct"
	if c.Stat().Limited || strings.Contains(s, "/p2p-circuit") {
		kind = "relayed"
	}
	transport := "TCP"
	switch {
	case strings.Contains(s, "/webrtc"):
		transport = "WebRTC"
	case strings.Contains(s, "/webtransport"):
		transport = "WebTransport"
	case strings.Contains(s, "/quic"):
		transport = "QUIC"
	}
	ip, err := addr.ValueForProtocol(ma.P_IP4)
	if err != nil {
		ip, _ = addr.ValueForProtocol(ma.P_IP6)
	}
	where := "internet"
	if p := net.ParseIP(ip); p != nil && (p.IsPrivate() || p.IsLoopback() || p.IsLinkLocalUnicast()) {
		where = "LAN"
	}
	return fmt.Sprintf("%s · %s · %s %s", kind, transport, where, ip)
}

// shortID shortens a peer ID for display: "12D3…x7Qa".
func shortID(id string) string {
	if len(id) <= 10 {
		return id
	}
	return id[:4] + "…" + id[len(id)-4:]
}

var stdin = bufio.NewReader(os.Stdin)

// confirm asks a yes/no question on stdin. With no terminal to answer from
// (stdin closed) it says no: a guest is never let in by accident.
func confirm(question string) bool {
	out.StopLive()
	out.mu.Lock()
	out.asking = true
	fmt.Fprint(os.Stderr, "  "+sCyan.Render("?")+" "+sBold.Render(question)+" "+sDim.Render("[Y/n] "))
	out.mu.Unlock()
	line, err := stdin.ReadString('\n')
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(os.Stderr, strings.TrimSpace(line)) // echo answers that came from a pipe
	}
	out.mu.Lock()
	out.asking = false
	held := out.held
	out.held = nil
	out.mu.Unlock()
	for _, l := range held {
		out.Println(l)
	}
	if err != nil && line == "" {
		fmt.Fprintln(os.Stderr, sDim.Render("no answer (stdin closed), declining · use --yes to allow guests automatically"))
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}

// gradientBar draws a bar that fades from violet to cyan.
func gradientBar(frac float64, width int) string {
	filled := int(frac * float64(width))
	var b strings.Builder
	for i := range width {
		if i < filled {
			t := float64(i) / float64(max(width-1, 1))
			b.WriteString(re.NewStyle().Foreground(lerpColor(0x8B5CF6, 0x22D3EE, t)).Render("━"))
		} else {
			b.WriteString(sDim.Render("─"))
		}
	}
	return b.String()
}

// sparkline draws samples as ▁▂▃▄▅▆▇█, scaled to the largest one and colored
// with the same violet -> cyan gradient. It is right-aligned in width cells.
func sparkline(samples []float64, width int) string {
	const ticks = "▁▂▃▄▅▆▇█"
	levels := []rune(ticks)
	if len(samples) > width {
		samples = samples[len(samples)-width:]
	}
	peak := 0.0
	for _, v := range samples {
		peak = max(peak, v)
	}
	var b strings.Builder
	b.WriteString(sDim.Render(strings.Repeat("·", width-len(samples))))
	for i, v := range samples {
		lvl := 0
		if peak > 0 {
			lvl = int(v / peak * float64(len(levels)-1))
		}
		t := float64(width-len(samples)+i) / float64(max(width-1, 1))
		st := re.NewStyle().Foreground(lerpColor(0x8B5CF6, 0x22D3EE, t))
		if v == 0 {
			st = sDim
		}
		b.WriteString(st.Render(string(levels[lvl])))
	}
	return b.String()
}

func lerpColor(a, b uint32, t float64) lipgloss.Color {
	ch := func(shift uint) uint32 {
		x, y := float64((a>>shift)&0xff), float64((b>>shift)&0xff)
		return uint32(x + (y-x)*t)
	}
	return lipgloss.Color(fmt.Sprintf("#%02X%02X%02X", ch(16), ch(8), ch(0)))
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// humanDuration prints 42s, 4m12s, 1h03m.
func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}
