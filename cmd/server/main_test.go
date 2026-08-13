package main

import (
	"net"
	"syscall"
	"testing"
	"time"
)

// SIGTERM must unwind run() through the worker wait and the deferred
// scheduler stop / database close, returning a zero exit code. If any
// shutdown step called os.Exit the test binary would die before reporting.
func TestRunShutsDownCleanlyOnSIGTERM(t *testing.T) {
	// Grab a free port, then release it for the server to bind.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}

	t.Setenv("ADDR", addr)
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("HMAC_SECRET", "test-secret")

	done := make(chan int, 1)
	go func() { done <- run() }()

	// Wait for the server to come up before signalling.
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start listening within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("run() = %d, want 0 after clean SIGTERM shutdown", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run() did not return within 30s of SIGTERM")
	}
}
