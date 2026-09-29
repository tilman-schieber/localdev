//go:build !linux && !darwin

package main

import "errors"

func listListeners() ([]Listener, error) {
	return nil, errors.New("port discovery is not supported on this platform")
}
