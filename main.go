// Command localdev is a local development service registry and reverse proxy:
// http://localhost shows a dashboard, http://<name>.localhost proxies to each app.
package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"
)

const version = "0.1.0"

//go:embed docs/agents.md
var agentDocs []byte

// Exit codes (documented in docs/agents.md).
const (
	exitOK          = 0
	exitError       = 1
	exitUsage       = 2
	exitNotFound    = 3
	exitDaemon      = 4
	exitStartFailed = 5
	exitTimeout     = 6
)

var jsonOut bool

const usage = `localdev - stable *.localhost URLs for local dev servers

Usage: localdev <command> [args] [--json]

Apps:
  run [name] [flags] -- <cmd...>   register (or update) a managed app and start it; prints its URL
  register [name] --port N         register an app you run yourself (proxy only)
  register [name] --cmd CMD        register a managed app without starting it
  unregister <name>                remove an app (stops it if managed)       alias: rm
  list                             list apps and their status                alias: ls
  info [name]                      show one app
  url [name]                       print the app's URL
  open [name]                      open the app in a browser
  start|restart [name]             start/restart a managed app and wait until it listens
  stop [name]                      stop a managed app
  wait [name] [--timeout 60s]      wait until the app accepts connections
  logs [name] [-n 100] [-f]        print (or follow) a managed app's output

Discovery:
  discover [--all]                 list unregistered services listening on localhost

Daemon:
  daemon [run|start|stop|status]   run in foreground / background, stop, show status
  status                           same as 'daemon status'

Other:
  agent-help                       print the guide for coding agents
  version

If [name] is omitted, the app whose directory is the current directory is used,
else the current directory's name. Commands start the daemon on demand.
Flags for run/register: --port N  --cwd DIR  --env K=V (repeatable)  --rewrite-host
Flags for run/start/restart: --no-wait  --timeout DURATION (default 60s)
Exit codes: 0 ok, 1 error, 2 usage, 3 not found, 4 daemon unavailable,
            5 app failed to start, 6 timed out.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	// --json is global and may appear anywhere before "--".
	var clean []string
	for i, a := range args {
		if a == "--" {
			clean = append(clean, args[i:]...)
			break
		}
		if a == "--json" || a == "-json" {
			jsonOut = true
			continue
		}
		clean = append(clean, a)
	}
	if len(clean) == 0 {
		fmt.Print(usage)
		return exitOK
	}
	cmd, rest := clean[0], clean[1:]
	switch cmd {
	case "run":
		return cmdRun(rest)
	case "register", "add":
		return cmdRegister(rest)
	case "unregister", "rm", "remove":
		return cmdUnregister(rest)
	case "list", "ls":
		return cmdList(rest)
	case "info", "show":
		return cmdInfo(rest)
	case "url":
		return cmdURL(rest)
	case "open":
		return cmdOpen(rest)
	case "start":
		return cmdStart(rest, "start")
	case "restart":
		return cmdStart(rest, "restart")
	case "stop":
		return cmdStop(rest)
	case "wait":
		return cmdWait(rest)
	case "logs", "log":
		return cmdLogs(rest)
	case "discover":
		return cmdDiscover(rest)
	case "daemon":
		return cmdDaemon(rest)
	case "status":
		return cmdDaemon([]string{"status"})
	case "agent-help":
		os.Stdout.Write(agentDocs)
		return exitOK
	case "version", "--version", "-v":
		fmt.Println(version)
		return exitOK
	case "help", "--help", "-h":
		fmt.Print(usage)
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "localdev: unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}
}

// ---- output helpers ----

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// fail reports err on stderr (as JSON with --json) and returns the matching exit code.
func fail(err error) int {
	code, kind := exitError, "error"
	var ce *callError
	var ue usageError
	switch {
	case errors.As(err, &ue):
		code, kind = exitUsage, "usage"
	case errors.Is(err, errDaemonDown):
		code, kind = exitDaemon, "daemon_unavailable"
	case errors.As(err, &ce):
		kind = ce.Code
		switch {
		case ce.Code == "start_failed":
			code = exitStartFailed
		case ce.Status == 404:
			code = exitNotFound
		case ce.Status == 400:
			code = exitUsage
		}
	}
	if jsonOut {
		b, _ := json.Marshal(apiError{Error: err.Error(), Code: kind})
		fmt.Fprintln(os.Stderr, string(b))
	} else {
		logf("localdev: %v", err)
	}
	return code
}

// ---- argument parsing ----

type envFlag map[string]string

func (e envFlag) String() string { return "" }
func (e envFlag) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("expected KEY=VALUE, got %q", s)
	}
	e[k] = v
	return nil
}

// parseArgs parses flags interspersed with positional arguments. Everything after
// a literal "--" is returned separately as the command.
func parseArgs(fs *flag.FlagSet, args []string) (pos, command []string, err error) {
	for i, a := range args {
		if a == "--" {
			args, command = args[:i], args[i+1:]
			break
		}
	}
	fs.SetOutput(io.Discard)
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Print(usage)
				os.Exit(exitOK)
			}
			return nil, nil, usageError{err.Error()}
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, command, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func cwdOrFlag(dir string) (string, error) {
	if dir == "" {
		return os.Getwd()
	}
	return filepath.Abs(dir)
}

// resolveName returns the explicit name, or the app registered for dir, or dir's basename.
func resolveName(c *client, pos []string, dir string) (string, error) {
	if len(pos) > 1 {
		return "", usageError{"too many arguments: " + strings.Join(pos[1:], " ")}
	}
	if len(pos) == 1 {
		if !validName(pos[0]) {
			return "", usageError{fmt.Sprintf("invalid app name %q (use lowercase letters, digits, dashes; e.g. %q)", pos[0], sanitizeName(pos[0]))}
		}
		return pos[0], nil
	}
	var apps []AppView
	if err := c.do("GET", "/api/apps", nil, &apps); err != nil {
		return "", err
	}
	var matches []string
	for _, a := range apps {
		if a.Cwd == dir {
			matches = append(matches, a.Name)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", usageError{"several apps use this directory, specify one: " + strings.Join(matches, ", ")}
	}
	name := sanitizeName(filepath.Base(dir))
	if name == "" {
		return "", usageError{"cannot derive an app name from " + dir + "; pass one explicitly"}
	}
	return name, nil
}

// connectAndResolve is the common prologue: connect to the daemon and pick the app name.
func connectAndResolve(pos []string, dir string) (*client, string, error) {
	c, err := connect()
	if err != nil {
		return nil, "", err
	}
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return nil, "", err
		}
	}
	name, err := resolveName(c, pos, dir)
	return c, name, err
}

// ---- commands ----

type defFlags struct {
	port        *int
	cwd         *string
	env         envFlag
	rewriteHost *bool
}

func addDefFlags(fs *flag.FlagSet) *defFlags {
	d := &defFlags{env: envFlag{}}
	d.port = fs.Int("port", 0, "port the app listens on (default: assign one and pass it as $PORT)")
	d.cwd = fs.String("cwd", "", "working directory (default: current directory)")
	fs.Var(d.env, "env", "extra environment variable KEY=VALUE (repeatable)")
	d.rewriteHost = fs.Bool("rewrite-host", false, "send Host: localhost:<port> upstream instead of <name>.localhost")
	return d
}

type waitFlags struct {
	noWait  *bool
	timeout *time.Duration
}

func addWaitFlags(fs *flag.FlagSet) *waitFlags {
	return &waitFlags{
		noWait:  fs.Bool("no-wait", false, "return immediately instead of waiting for the app to listen"),
		timeout: fs.Duration("timeout", 60*time.Second, "how long to wait for the app to listen"),
	}
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	df := addDefFlags(fs)
	wf := addWaitFlags(fs)
	pos, command, err := parseArgs(fs, args)
	if err != nil {
		return fail(err)
	}
	dir, err := cwdOrFlag(*df.cwd)
	if err != nil {
		return fail(err)
	}
	c, name, err := connectAndResolve(pos, dir)
	if err != nil {
		return fail(err)
	}
	cmdline := strings.Join(command, " ")
	if cmdline == "" {
		var v AppView
		if err := c.do("GET", "/api/apps/"+name, nil, &v); err != nil {
			var ce *callError
			if errors.As(err, &ce) && ce.Status == 404 {
				return fail(usageError{"no command given and no app named " + name + "; use: localdev run " + name + " -- <command>"})
			}
			return fail(err)
		}
	} else {
		req := putRequest{Port: *df.port, Command: cmdline, Cwd: dir, Env: df.env, RewriteHost: *df.rewriteHost}
		if err := c.do("PUT", "/api/apps/"+name, req, nil); err != nil {
			return fail(err)
		}
	}
	return startAndWait(c, name, "start", wf)
}

func startAndWait(c *client, name, action string, wf *waitFlags) int {
	var v AppView
	if err := c.do("POST", "/api/apps/"+name+"/"+action, startRequest{Env: os.Environ()}, &v); err != nil {
		return fail(err)
	}
	code := exitOK
	if !*wf.noWait {
		v, code = waitReady(c, name, *wf.timeout)
	}
	if jsonOut {
		printJSON(v)
	} else if code == exitOK {
		fmt.Println(v.URL)
	}
	return code
}

func waitReady(c *client, name string, timeout time.Duration) (AppView, int) {
	start := time.Now()
	last := ""
	for {
		var v AppView
		if err := c.do("GET", "/api/apps/"+name, nil, &v); err != nil {
			return v, fail(err)
		}
		switch v.Status {
		case "running":
			return v, exitOK
		case "exited", "stopped":
			msg := name + " is " + v.Status
			if v.ExitCode != nil {
				msg = fmt.Sprintf("%s exited with code %d", name, *v.ExitCode)
			}
			logf("localdev: %s", msg)
			printLogTail(c, name)
			return v, exitStartFailed
		}
		if time.Since(start) > timeout {
			logf("localdev: timed out after %s waiting for %s to listen on port %d", timeout, name, v.Port)
			printLogTail(c, name)
			return v, exitTimeout
		}
		if v.Status != last {
			logf("%s: %s (port %d)", name, v.Status, v.Port)
			last = v.Status
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func printLogTail(c *client, name string) {
	var s string
	if c.do("GET", "/api/apps/"+name+"/logs?tail=30", nil, &s) == nil && s != "" {
		logf("--- last lines of %s output ---\n%s---", name, s)
	}
}

func cmdRegister(args []string) int {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	df := addDefFlags(fs)
	cmdStr := fs.String("cmd", "", "shell command that starts the app (makes it managed)")
	pos, command, err := parseArgs(fs, args)
	if err != nil {
		return fail(err)
	}
	if *cmdStr == "" && len(command) > 0 {
		*cmdStr = strings.Join(command, " ")
	}
	if *cmdStr == "" && *df.port == 0 {
		return fail(usageError{"register needs --port N (app you run yourself) or --cmd CMD (managed app)"})
	}
	dir, err := cwdOrFlag(*df.cwd)
	if err != nil {
		return fail(err)
	}
	c, name, err := connectAndResolve(pos, dir)
	if err != nil {
		return fail(err)
	}
	var v AppView
	req := putRequest{Port: *df.port, Command: *cmdStr, Cwd: dir, Env: df.env, RewriteHost: *df.rewriteHost}
	if err := c.do("PUT", "/api/apps/"+name, req, &v); err != nil {
		return fail(err)
	}
	if jsonOut {
		printJSON(v)
		return exitOK
	}
	logf("registered %s -> port %d", name, v.Port)
	if v.Stale {
		logf("note: %s is running with an older definition; run 'localdev restart %s' to apply", name, name)
	}
	fmt.Println(v.URL)
	return exitOK
}

func cmdUnregister(args []string) int {
	fs := flag.NewFlagSet("unregister", flag.ContinueOnError)
	pos, _, err := parseArgs(fs, args)
	if err != nil {
		return fail(err)
	}
	if len(pos) != 1 {
		return fail(usageError{"usage: localdev unregister <name>"})
	}
	c, name, err := connectAndResolve(pos, "")
	if err != nil {
		return fail(err)
	}
	if err := c.do("DELETE", "/api/apps/"+name, nil, nil); err != nil {
		return fail(err)
	}
	if jsonOut {
		printJSON(map[string]any{"ok": true, "name": name})
	} else {
		logf("unregistered %s", name)
	}
	return exitOK
}

func cmdList(args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	if _, _, err := parseArgs(fs, args); err != nil {
		return fail(err)
	}
	c, err := connect()
	if err != nil {
		return fail(err)
	}
	var apps []AppView
	if err := c.do("GET", "/api/apps", nil, &apps); err != nil {
		return fail(err)
	}
	if jsonOut {
		printJSON(apps)
		return exitOK
	}
	if len(apps) == 0 {
		logf("no apps registered; try: localdev run <name> -- <command>")
		return exitOK
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tPORT\tURL\tCOMMAND")
	for _, a := range apps {
		cmd := a.Command
		if cmd == "" {
			cmd = "-"
		}
		st := a.Status
		if a.Stale {
			st += "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", a.Name, st, a.Port, a.URL, cmd)
	}
	tw.Flush()
	return exitOK
}

func getApp(args []string) (*client, AppView, int) {
	fs := flag.NewFlagSet("app", flag.ContinueOnError)
	pos, _, err := parseArgs(fs, args)
	if err != nil {
		return nil, AppView{}, fail(err)
	}
	c, name, err := connectAndResolve(pos, "")
	if err != nil {
		return nil, AppView{}, fail(err)
	}
	var v AppView
	if err := c.do("GET", "/api/apps/"+name, nil, &v); err != nil {
		return nil, v, fail(err)
	}
	return c, v, exitOK
}

func cmdInfo(args []string) int {
	_, v, code := getApp(args)
	if code != exitOK {
		return code
	}
	if jsonOut {
		printJSON(v)
		return exitOK
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	row := func(k string, val any) { fmt.Fprintf(tw, "%s:\t%v\n", k, val) }
	row("name", v.Name)
	row("url", v.URL)
	row("status", v.Status)
	row("port", fmt.Sprintf("%d%s", v.Port, map[bool]string{true: " (auto)"}[v.PortAuto]))
	if v.Managed {
		row("command", v.Command)
	}
	if v.Cwd != "" {
		row("cwd", v.Cwd)
	}
	for k, val := range v.Env {
		row("env", k+"="+val)
	}
	if v.PID != 0 {
		row("pid", v.PID)
	}
	if v.ExitCode != nil {
		row("exit code", *v.ExitCode)
	}
	if v.Stale {
		row("stale", "definition changed since start; restart to apply")
	}
	if v.LogFile != "" {
		row("log", v.LogFile)
	}
	tw.Flush()
	return exitOK
}

func cmdURL(args []string) int {
	_, v, code := getApp(args)
	if code != exitOK {
		return code
	}
	if jsonOut {
		printJSON(map[string]string{"name": v.Name, "url": v.URL})
	} else {
		fmt.Println(v.URL)
	}
	return exitOK
}

func cmdOpen(args []string) int {
	_, v, code := getApp(args)
	if code != exitOK {
		return code
	}
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	if err := exec.Command(opener, v.URL).Start(); err != nil {
		return fail(fmt.Errorf("could not open browser: %v (url: %s)", err, v.URL))
	}
	fmt.Println(v.URL)
	return exitOK
}

func cmdStart(args []string, action string) int {
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	wf := addWaitFlags(fs)
	pos, _, err := parseArgs(fs, args)
	if err != nil {
		return fail(err)
	}
	c, name, err := connectAndResolve(pos, "")
	if err != nil {
		return fail(err)
	}
	return startAndWait(c, name, action, wf)
}

func cmdStop(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	pos, _, err := parseArgs(fs, args)
	if err != nil {
		return fail(err)
	}
	c, name, err := connectAndResolve(pos, "")
	if err != nil {
		return fail(err)
	}
	var v AppView
	if err := c.do("POST", "/api/apps/"+name+"/stop", nil, &v); err != nil {
		return fail(err)
	}
	if jsonOut {
		printJSON(v)
	} else {
		logf("%s: %s", name, v.Status)
	}
	return exitOK
}

func cmdWait(args []string) int {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 60*time.Second, "how long to wait")
	pos, _, err := parseArgs(fs, args)
	if err != nil {
		return fail(err)
	}
	c, name, err := connectAndResolve(pos, "")
	if err != nil {
		return fail(err)
	}
	v, code := waitReady(c, name, *timeout)
	if jsonOut {
		printJSON(v)
	} else if code == exitOK {
		fmt.Println(v.URL)
	}
	return code
}

func cmdLogs(args []string) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	n := fs.Int("n", 100, "number of lines")
	follow := fs.Bool("f", false, "follow output")
	fs.BoolVar(follow, "follow", false, "follow output")
	pos, _, err := parseArgs(fs, args)
	if err != nil {
		return fail(err)
	}
	c, name, err := connectAndResolve(pos, "")
	if err != nil {
		return fail(err)
	}
	var v AppView
	if err := c.do("GET", "/api/apps/"+name, nil, &v); err != nil {
		return fail(err)
	}
	if !v.Managed {
		return fail(fmt.Errorf("%s is not managed by localdev, so there are no logs", name))
	}
	if !*follow {
		var s string
		if err := c.do("GET", fmt.Sprintf("/api/apps/%s/logs?tail=%d", name, *n), nil, &s); err != nil {
			return fail(err)
		}
		fmt.Print(s)
		return exitOK
	}
	return followFile(v.LogFile, *n)
}

func followFile(path string, n int) int {
	fmt.Print(tailFile(path, n))
	f, _ := os.Open(path)
	if f != nil {
		f.Seek(0, io.SeekEnd)
	}
	for {
		time.Sleep(250 * time.Millisecond)
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		var fst os.FileInfo
		if f != nil {
			fst, _ = f.Stat()
		}
		if f == nil || fst == nil || !os.SameFile(st, fst) { // restarted: new log file
			if f != nil {
				f.Close()
			}
			if f, err = os.Open(path); err != nil {
				f = nil
				continue
			}
		}
		io.Copy(os.Stdout, f)
	}
}

func cmdDiscover(args []string) int {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	all := fs.Bool("all", false, "include non-HTTP and already registered services")
	if _, _, err := parseArgs(fs, args); err != nil {
		return fail(err)
	}
	// Runs locally and never starts the daemon.
	skip := map[int]bool{}
	registered := map[int]string{}
	if c, info, ok := findDaemon(); ok {
		skip[info.Port] = true
		var apps []AppView
		if c.do("GET", "/api/apps", nil, &apps) == nil {
			for _, a := range apps {
				registered[a.Port] = a.Name
			}
		}
	} else if cfgs, err := loadRegistry(getPaths()); err == nil {
		for _, a := range cfgs {
			registered[a.Port] = a.Name
		}
	}
	ls, err := discover(skip, registered)
	if err != nil {
		return fail(err)
	}
	out := []Listener{}
	for _, l := range ls {
		if *all || (l.HTTP && l.RegisteredAs == "") {
			out = append(out, l)
		}
	}
	if jsonOut {
		printJSON(out)
		return exitOK
	}
	if len(out) == 0 {
		logf("no unregistered HTTP services found (use --all to see everything)")
		return exitOK
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PORT\tADDR\tHTTP\tPID\tPROCESS\tCWD\tREGISTERED")
	for _, l := range out {
		fmt.Fprintf(tw, "%d\t%s\t%v\t%d\t%s\t%s\t%s\n", l.Port, l.Addr, l.HTTP, l.PID, dash(l.Process), dash(l.Cwd), dash(l.RegisteredAs))
	}
	tw.Flush()
	logf("register one with: localdev register <name> --port <port>")
	return exitOK
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdDaemon(args []string) int {
	sub := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "run":
		return cmdDaemonRun(args)
	case "start":
		c, info, ok := findDaemon()
		if !ok {
			if os.Getenv("LOCALDEV_NO_AUTOSTART") != "" {
				return fail(errDaemonDown)
			}
			var err error
			if info, err = spawnDaemon(); err != nil {
				return fail(err)
			}
		} else {
			info, _ = c.health()
		}
		if jsonOut {
			printJSON(info)
		} else {
			fmt.Println(info.URL)
		}
		return exitOK
	case "stop":
		c, _, ok := findDaemon()
		if !ok {
			if !jsonOut {
				logf("daemon is not running")
			}
			return exitOK
		}
		if err := c.do("POST", "/api/shutdown", nil, nil); err != nil {
			return fail(err)
		}
		for i := 0; i < 100; i++ {
			if _, ok := c.health(); !ok {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if jsonOut {
			printJSON(map[string]bool{"ok": true})
		} else {
			logf("daemon stopped")
		}
		return exitOK
	case "status":
		_, info, ok := findDaemon()
		if !ok {
			if jsonOut {
				printJSON(map[string]any{"running": false})
			} else {
				logf("daemon is not running")
			}
			return exitDaemon
		}
		if jsonOut {
			printJSON(struct {
				Running bool `json:"running"`
				daemonInfo
			}{true, info})
		} else {
			fmt.Printf("running: %s (pid %d, version %s)\nregistry: %s\ndiscovery: %v\n", info.URL, info.PID, info.Version, info.Config, info.Discover)
		}
		return exitOK
	default:
		return fail(usageError{"usage: localdev daemon [run|start|stop|status]"})
	}
}
