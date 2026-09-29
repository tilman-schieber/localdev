package main

import (
	"context"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

type proxyTarget struct {
	name        string
	port        int
	rewriteHost bool
}

type targetKey struct{}

// dialLoopback connects to 127.0.0.1, falling back to ::1 for servers that bind
// only to the IPv6 loopback (common with Node when "localhost" resolves to ::1).
func dialLoopback(ctx context.Context, network, addr string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var dialer net.Dialer
	c, err := dialer.DialContext(ctx, "tcp", "127.0.0.1:"+port)
	if err == nil {
		return c, nil
	}
	if c6, err6 := dialer.DialContext(ctx, "tcp", "[::1]:"+port); err6 == nil {
		return c6, nil
	}
	return nil, err
}

func (d *Daemon) newProxy() http.Handler {
	transport := &http.Transport{
		Proxy:                 nil, // never route local traffic through HTTP_PROXY
		DialContext:           dialLoopback,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 0, // dev servers may take a long time on first compile
		DisableCompression:    true,
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			t := pr.In.Context().Value(targetKey{}).(proxyTarget)
			pr.SetURL(&url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", t.port)})
			// ReverseProxy re-encodes the query before Rewrite (always, under the
			// urlmaxqueryparams=0 GODEBUG implied by go.mod's go version; otherwise for ";"
			// or bad escapes). That reorders and drops flags dev servers match on, such as
			// Vite's "?svelte&type=style&lang.css", so forward the client's query verbatim.
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.SetXForwarded()
			if !t.rewriteHost {
				pr.Out.Host = pr.In.Host
			}
		},
		Transport:     transport,
		FlushInterval: -1, // stream responses (SSE, chunked) without buffering
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			t := r.Context().Value(targetKey{}).(proxyTarget)
			d.renderUnavailable(w, r, t.name)
		},
	}
}

func (d *Daemon) serveProxy(w http.ResponseWriter, r *http.Request, sub string) {
	// foo.myapp.localhost routes to myapp, so apps can use their own subdomains.
	name := sub[strings.LastIndexByte(sub, '.')+1:]
	d.mu.Lock()
	a := d.apps[name]
	var t proxyTarget
	var started *Proc
	if a != nil {
		// Start on first visit: a managed app that hasn't run since the daemon started
		// (e.g. after a reboot) is launched by a request. Apps that were stopped
		// explicitly or crashed stay down.
		if a.cfg.Command != "" && a.proc == nil {
			if err := d.startLocked(a, nil); err != nil {
				log.Printf("localdev: start %s on request: %v", name, err)
			} else {
				log.Printf("localdev: started %s on first request", name)
				started = a.proc
			}
		}
		t = proxyTarget{name: name, port: a.cfg.Port, rewriteHost: a.cfg.RewriteHost}
	}
	d.mu.Unlock()
	if a == nil {
		d.renderPage(w, r, http.StatusNotFound, "Unknown app",
			fmt.Sprintf("No app named <code>%s</code> is registered.", html.EscapeString(name)),
			fmt.Sprintf("localdev run %s -- &lt;command&gt;\nlocaldev register %s --port &lt;port&gt;", html.EscapeString(name), html.EscapeString(name)), false)
		return
	}
	if started != nil {
		if wantsHTML(r) {
			d.renderUnavailable(w, r, name) // "starting…" page, reloads itself
			return
		}
		// API clients, curl, etc.: hold the request until the app listens.
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && !started.exited(); {
			d.mu.Lock()
			t.port = a.cfg.Port // may change if the app ignored $PORT
			d.mu.Unlock()
			if portOpen(t.port) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	d.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), targetKey{}, t)))
}

func (d *Daemon) renderUnavailable(w http.ResponseWriter, r *http.Request, name string) {
	v, ok := d.view(name)
	if !ok {
		d.renderPage(w, r, http.StatusBadGateway, "Unavailable", "The app was removed.", "", false)
		return
	}
	n := html.EscapeString(name)
	switch v.Status {
	case "starting":
		d.renderPage(w, r, http.StatusServiceUnavailable, n+" is starting…",
			fmt.Sprintf("Waiting for <code>%s</code> to listen on port %d. This page reloads automatically.", n, v.Port),
			"localdev logs "+n, true)
	case "running":
		d.renderPage(w, r, http.StatusBadGateway, n+": bad gateway",
			fmt.Sprintf("Port %d accepted the connection but the request failed.", v.Port), "localdev logs "+n, true)
	case "down":
		d.renderPage(w, r, http.StatusBadGateway, n+" is not running",
			fmt.Sprintf("Nothing is listening on port %d. localdev does not manage this app's process.", v.Port), "", true)
	default:
		hint := "localdev start " + n
		msg := fmt.Sprintf("<code>%s</code> is %s.", n, v.Status)
		if v.ExitCode != nil {
			msg = fmt.Sprintf("<code>%s</code> exited with code %d.", n, *v.ExitCode)
			hint += "\nlocaldev logs " + n
		}
		d.renderPage(w, r, http.StatusBadGateway, n+" is not running", msg, hint, true)
	}
}

func wantsHTML(r *http.Request) bool { return strings.Contains(r.Header.Get("Accept"), "text/html") }

func (d *Daemon) renderPage(w http.ResponseWriter, r *http.Request, status int, title, body, hint string, reload bool) {
	w.Header().Set("Cache-Control", "no-store")
	if !wantsHTML(r) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		fmt.Fprintf(w, "localdev: %s\n", html.UnescapeString(title))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	refresh := ""
	if reload {
		refresh = `<meta http-equiv="refresh" content="2">`
	}
	pre := ""
	if hint != "" {
		pre = "<pre>" + hint + "</pre>"
	}
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">%s<title>%s</title>
<style>:root{color-scheme:light dark;--bg:#fafafa;--fg:#1a1a1a;--mute:#666;--card:#fff;--line:#e4e4e4}
@media (prefers-color-scheme:dark){:root{--bg:#111;--fg:#eee;--mute:#999;--card:#1a1a1a;--line:#2a2a2a}}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,sans-serif;display:grid;place-items:center;min-height:100vh;padding:16px;box-sizing:border-box}
main{max-width:560px;width:100%%;background:var(--card);border:1px solid var(--line);border-radius:10px;padding:24px}
h1{font-size:18px;margin:0 0 8px}p{color:var(--mute);margin:0 0 12px}code,pre{font-family:ui-monospace,monospace;font-size:13px}
pre{background:var(--bg);border:1px solid var(--line);border-radius:6px;padding:10px;overflow-x:auto;margin:0 0 12px}a{color:inherit}</style></head>
<body><main><h1>%s</h1><p>%s</p>%s<p><a href="%s">localdev dashboard</a></p></main></body></html>`,
		refresh, title, title, body, pre, d.baseURL("localhost"))
}
