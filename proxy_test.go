package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The proxy must forward query strings verbatim: Vite matches raw flags like
// "?svelte&type=style&lang.css", which re-encoding would reorder into "lang.css=&svelte=&type=style".
func TestProxyPreservesRawQuery(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.URL.RawQuery)
	}))
	defer backend.Close()
	_, portStr, _ := net.SplitHostPort(backend.Listener.Addr().String())
	var port int
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}

	proxy := (&Daemon{}).newProxy()
	for _, q := range []string{
		"svelte&type=style&lang.css",
		"a=1;b=2",
		"x=%zz&y",
		"t=1700000000&import",
	} {
		req := httptest.NewRequest("GET", "http://app.localhost/src/App.svelte?"+q, nil)
		req = req.WithContext(context.WithValue(req.Context(), targetKey{}, proxyTarget{name: "app", port: port}))
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		if got := rec.Body.String(); got != q {
			t.Errorf("query %q forwarded as %q", q, got)
		}
	}
}
