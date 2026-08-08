package sentinel

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePubSubServer extends the RESP2 trick of fakeSentinelServer with a
// SUBSCRIBE path: it accepts one client, records every command, and pushes
// the publications the test hands it.
type fakePubSubServer struct {
	ln net.Listener

	mu       sync.Mutex
	commands []string

	// publish sends one publication to the subscribed client.
	publish chan string
	// hangup closes the client connection from the server side.
	hangup chan struct{}
}

func newFakePubSubServer(t *testing.T) *fakePubSubServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakePubSubServer{
		ln:      ln,
		publish: make(chan string, 16),
		hangup:  make(chan struct{}),
	}
	t.Cleanup(func() { _ = ln.Close() })
	go s.serve()
	return s
}

func (s *fakePubSubServer) addr() string { return s.ln.Addr().String() }

func (s *fakePubSubServer) received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

func (s *fakePubSubServer) serve() {
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	go func() {
		select {
		case <-s.hangup:
			_ = conn.Close()
		case <-time.After(30 * time.Second):
		}
	}()

	r := bufio.NewReader(conn)
	for {
		args, err := readRESPCommand(r)
		if err != nil || len(args) == 0 {
			return
		}
		s.mu.Lock()
		s.commands = append(s.commands, strings.Join(args, " "))
		s.mu.Unlock()

		switch strings.ToLower(args[0]) {
		case "hello":
			// An old server: go-redis falls back to RESP2 and sends AUTH.
			_, _ = fmt.Fprintf(conn, "-ERR unknown command 'HELLO'\r\n")
		case "subscribe":
			channel := args[1]
			_, _ = fmt.Fprintf(conn, "*3\r\n$9\r\nsubscribe\r\n$%d\r\n%s\r\n:1\r\n", len(channel), channel)
			go func() {
				for payload := range s.publish {
					_, _ = fmt.Fprintf(conn, "*3\r\n$7\r\nmessage\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n",
						len(channel), channel, len(payload), payload)
				}
			}()
		default:
			_, _ = fmt.Fprintf(conn, "+OK\r\n")
		}
	}
}

// collector gathers the master names onEvent delivers.
type collector struct {
	mu    sync.Mutex
	names []string
}

func (c *collector) add(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names = append(c.names, name)
}

func (c *collector) get() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.names...)
}

func TestSubscribeSwitchMasterDeliversMasterNames(t *testing.T) {
	server := newFakePubSubServer(t)
	got := &collector{}

	done := make(chan error, 1)
	go func() {
		done <- SubscribeSwitchMaster(context.Background(), server.addr(), "", nil, got.add)
	}()

	server.publish <- "mymaster 10.0.0.1 6379 10.0.0.2 6379"
	server.publish <- "other-master 10.1.0.1 6379 10.1.0.2 6379"

	deadline := time.Now().Add(5 * time.Second)
	for {
		if names := got.get(); len(names) == 2 {
			if names[0] != "mymaster" || names[1] != "other-master" {
				t.Fatalf("delivered names = %v", names)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("publications never delivered; got %v", got.get())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The connection dropping must surface as an error so the caller can
	// reconnect; a silent return would leave the trigger dead.
	close(server.hangup)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("SubscribeSwitchMaster returned nil after the server hung up")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SubscribeSwitchMaster did not return after the server hung up")
	}
}

func TestSubscribeSwitchMasterPresentsThePassword(t *testing.T) {
	server := newFakePubSubServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- SubscribeSwitchMaster(ctx, server.addr(), "s3cret", nil, func(string) {})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		authed := false
		for _, cmd := range server.received() {
			if strings.Contains(cmd, "s3cret") {
				authed = true
			}
		}
		if authed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the password never reached the sentinel; commands: %v", server.received())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestSubscribeSwitchMasterReturnsPromptlyOnContextCancel(t *testing.T) {
	server := newFakePubSubServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- SubscribeSwitchMaster(ctx, server.addr(), "", nil, func(string) {})
	}()

	// Wait for the subscription to be established, then cancel while the
	// connection is idle: the blocked read must be unblocked, not waited out.
	deadline := time.Now().Add(5 * time.Second)
	for {
		subscribed := false
		for _, cmd := range server.received() {
			if strings.HasPrefix(strings.ToLower(cmd), "subscribe") {
				subscribed = true
			}
		}
		if subscribed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no SUBSCRIBE seen; commands: %v", server.received())
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SubscribeSwitchMaster kept blocking after its context was cancelled")
	}
}

func TestSubscribeSwitchMasterFailsWhenNothingListens(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	err = SubscribeSwitchMaster(context.Background(), addr, "", nil, func(string) {})
	if err == nil {
		t.Fatal("SubscribeSwitchMaster returned nil against a closed port")
	}
}
