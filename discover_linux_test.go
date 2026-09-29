package main

import "testing"

func TestParseProcIP(t *testing.T) {
	if ip := parseProcIP("0100007F"); ip.String() != "127.0.0.1" {
		t.Errorf("v4: %v", ip)
	}
	if ip := parseProcIP("00000000000000000000000001000000"); ip.String() != "::1" {
		t.Errorf("v6: %v", ip)
	}
}
