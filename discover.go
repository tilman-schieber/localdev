package main

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Listener is a TCP socket listening on a loopback or wildcard address.
type Listener struct {
	Port         int    `json:"port"`
	Addr         string `json:"addr"`
	PID          int    `json:"pid,omitempty"`
	PGID         int    `json:"-"`
	Process      string `json:"process,omitempty"`
	Command      string `json:"command,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
	HTTP         bool   `json:"http"`
	RegisteredAs string `json:"registered_as,omitempty"`
}

func dedupeListeners(ls []Listener) []Listener {
	seen := map[[2]int]bool{}
	var out []Listener
	for _, l := range ls {
		k := [2]int{l.Port, l.PID}
		if !seen[k] {
			seen[k] = true
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// discover scans listeners on unprivileged ports and probes whether they speak HTTP.
func discover(skipPorts map[int]bool, registered map[int]string) ([]Listener, error) {
	ls, err := listListeners()
	if err != nil {
		return nil, err
	}
	var out []Listener
	for _, l := range ls {
		if l.Port >= 1024 && !skipPorts[l.Port] {
			l.RegisteredAs = registered[l.Port]
			out = append(out, l)
		}
	}
	client := &http.Client{
		Timeout:       700 * time.Millisecond,
		Transport:     &http.Transport{Proxy: nil, DialContext: dialLoopback, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var wg sync.WaitGroup
	for i := range out {
		wg.Add(1)
		go func(l *Listener) {
			defer wg.Done()
			resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", l.Port))
			if err == nil {
				resp.Body.Close()
				l.HTTP = true
			}
		}(&out[i])
	}
	wg.Wait()
	return out, nil
}
