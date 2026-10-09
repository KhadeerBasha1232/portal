// portal shares a local port with a friend using a short code.
//
//	portal share 3000          -> prints a code like 8-maple-otter
//	portal open 8-maple-otter  -> localhost:3000 on their machine tunnels to yours
//
// Any TCP service works (web apps, websockets, Postgres, SSH, game servers).
// No server to run: peers find each other via mDNS (LAN) or the public
// libp2p DHT (internet), connect directly (hole punching through NAT), and
// prove they know the code with SPAKE2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"time"

	logging "github.com/ipfs/go-log/v2"
)

var version = "dev"

// forceTUI (set with -ldflags "-X main.forceTUI=1") runs the dashboard even
// when stderr is not a terminal. Only used to test and record the dashboard.
var forceTUI string

func printUsage() {
	out.Println("\n" + headerLine("share a local port with a friend using a short code") + "\n")
	cmd := func(c, desc string) {
		out.Println("    " + sAccent.Render(fmt.Sprintf("%-34s", c)) + sDim.Render(desc))
	}
	out.Println("  " + sBold.Render("Usage"))
	cmd("portal share <port|host:port>", "share it, prints a code like 8-maple-otter")
	cmd("portal open <code>", "on your friend's machine: use it on localhost")
	cmd("portal version", "print the version")
	out.Println("")
	out.Println("  " + sBold.Render("Flags"))
	cmd("share --yes, -y", "let guests in without asking")
	cmd("share --max-guests N", "how many guests at once (default 1)")
	cmd("share --code CODE", "use your own code instead of a random one")
	cmd("open  --port N", "local port (default: same as the sharer's)")
	cmd("open  --timeout 3m", "how long to look for the sharer")
	cmd("--plain", "plain line output instead of the live dashboard")
	cmd("--verbose, -v", "show libp2p network logs")
	out.Println("")
	out.Println("  " + sDim.Render("Works for any TCP service: web apps, websockets, databases, SSH, game servers."))
	out.Println("  " + sDim.Render("No server needed: LAN via mDNS, internet via the public libp2p network."))
	out.Println("")
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			err = errors.New("cancelled")
		}
		failLine(err)
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.Usage = printUsage
	verbose := fs.Bool("verbose", false, "")
	fs.BoolVar(verbose, "v", false, "")
	plain := fs.Bool("plain", false, "")

	switch args[0] {
	case "share":
		code := fs.String("code", "", "")
		yes := fs.Bool("yes", false, "")
		fs.BoolVar(yes, "y", false, "")
		maxGuests := fs.Int("max-guests", 1, "")
		pos, err := parse(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return withHint("Example: portal share 3000", "usage: portal share <port|host:port>")
		}
		if *maxGuests < 1 {
			return errors.New("--max-guests must be at least 1")
		}
		quiet(*verbose)
		return cmdShare(ctx, pos[0], *code, *yes, *maxGuests, *plain || *verbose)

	case "open", "join", "connect":
		port := fs.Int("port", 0, "")
		timeout := fs.Duration("timeout", 3*time.Minute, "")
		pos, err := parse(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return withHint("Example: portal open 8-maple-otter", "usage: portal open <code>")
		}
		if *port < 0 || *port > 65535 {
			return errors.New("--port must be between 1 and 65535")
		}
		quiet(*verbose)
		return cmdOpen(ctx, openOpts{code: pos[0], port: *port, timeout: *timeout}, *plain || *verbose)

	case "version", "--version":
		fmt.Println("portal", version)
		return nil

	case "help", "-h", "--help":
		printUsage()
		return nil
	}
	return withHint("Try: portal share 3000 · portal open <code>", "unknown command %q", args[0])
}

func cmdShare(ctx context.Context, target, code string, yes bool, maxGuests int, plain bool) error {
	addr, port, err := parseTarget(target)
	if err != nil {
		return err
	}
	if err := checkTarget(addr, port); err != nil {
		return err
	}
	if code == "" {
		if code, err = generateCode(); err != nil {
			return err
		}
	}
	code, _, err = parseCode(code)
	if err != nil {
		return err
	}
	opts := shareOpts{target: addr, port: port, code: code, yes: yes, maxGuests: maxGuests}

	if !plain && (stderrTTY || forceTUI != "") {
		return dashboardShare(ctx, opts)
	}
	out.Println("\n" + headerLine("sharing "+addr) + "\n")
	out.Println(box(sDim.Render("your code"), sCyan.Render(code)))
	out.Println("")
	out.Println("  Your friend runs:")
	out.Println("    " + sAccent.Render("portal open "+code))
	out.Println("")
	err = runShare(ctx, opts, &plainReporter{}, nil)
	out.StopLive()
	if err == nil {
		out.Println("\n  " + sOK.Render("Stopped sharing.") + "\n")
	}
	return err
}

func cmdOpen(ctx context.Context, opts openOpts, plain bool) error {
	if !plain && (stderrTTY || forceTUI != "") {
		return dashboardOpen(ctx, opts)
	}
	out.Println("\n" + headerLine("open a shared port") + "\n")
	err := runOpen(ctx, opts, &plainReporter{}, nil)
	out.StopLive()
	if err == nil {
		out.Println("\n  " + sOK.Render("Disconnected.") + "\n")
	}
	return err
}

// parse allows flags before or after the positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func quiet(verbose bool) {
	if verbose {
		logging.SetAllLoggers(logging.LevelInfo)
		return
	}
	logging.SetAllLoggers(logging.LevelFatal)
	log.SetOutput(io.Discard) // the mDNS library logs through the standard logger
	os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
}
