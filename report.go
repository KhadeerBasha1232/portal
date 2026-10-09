package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// reporter is how share/open tell the user what is happening. There are two
// implementations: plainReporter (lines on stderr) and the Bubble Tea
// dashboard (dashboard.go).
type reporter interface {
	Step(msg string)                        // something worked: ✓
	Warn(msg string)                        // something to know about: !
	Spin(msg string)                        // what we're waiting for right now
	Request(r reqEntry)                     // one HTTP request through the tunnel
	Ask(ctx context.Context, q string) bool // yes/no question
}

type plainReporter struct{ askMu sync.Mutex }

func (*plainReporter) Step(msg string) { out.Println("  " + sOK.Render("✓") + " " + msg) }
func (*plainReporter) Warn(msg string) { out.Println("  " + sWarn.Render("!") + " " + msg) }
func (*plainReporter) Spin(msg string) { out.Spin(msg) }
func (*plainReporter) Request(r reqEntry) {
	out.Println("  " + formatReq(r, 40))
}

func (p *plainReporter) Ask(ctx context.Context, q string) bool {
	p.askMu.Lock()
	defer p.askMu.Unlock()
	answer := make(chan bool, 1)
	go func() { answer <- confirm(q) }()
	select {
	case ok := <-answer:
		return ok
	case <-ctx.Done():
		return false
	}
}

// formatReq renders "GET  /api/users   200  12ms".
func formatReq(r reqEntry, pathW int) string {
	path := r.Path
	if len([]rune(path)) > pathW {
		path = string([]rune(path)[:pathW-1]) + "…"
	}
	st := sOK
	switch {
	case r.Status >= 500:
		st = sErr
	case r.Status >= 400:
		st = sWarn
	case r.Status >= 300 || r.Status < 200:
		st = sCyan
	}
	return sDim.Render(r.At.Format("15:04:05")) + "  " +
		sAccent.Render(fmt.Sprintf("%-6s", r.Method)) + " " +
		fmt.Sprintf("%-*s", pathW, path) + "  " +
		st.Render(fmt.Sprintf("%d", r.Status)) + "  " +
		sDim.Render(fmt.Sprintf("%6s", humanLatency(r.Took)))
}

func humanLatency(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return "<1ms"
	case d < 10*time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
