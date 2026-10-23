package main

import "testing"

func TestRejectUnexpectedArgumentsBeforeUsingSession(t *testing.T) {
	for _, command := range []string{"qr", "check", "login", "watch", "status", "heartbeat", "sync"} {
		t.Run(command, func(t *testing.T) {
			if err := run(nil, []string{command, "--client-version", "0x18003e2b"}, nil); err == nil {
				t.Fatal("unexpected arguments accepted")
			}
		})
	}
	if err := run(nil, nil, nil); err == nil {
		t.Fatal("missing command accepted")
	}
}
