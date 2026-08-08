package sentinel

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Sentinel field replies arrive in one of two shapes: a flat alternating array
// under RESP2, or a map under RESP3, which go-redis v9 negotiates by default.
// Every parser has to accept both.
func resp2Master() any {
	return []any{
		"name", "rg-master",
		"ip", "172.30.0.10",
		"port", "6379",
		"flags", "master",
		"quorum", "2",
		"num-slaves", "1",
		"num-other-sentinels", "2",
	}
}

func resp3Master() any {
	return map[any]any{
		"name":                "rg-master",
		"ip":                  "172.30.0.10",
		"port":                "6379",
		"flags":               "master",
		"quorum":              "2",
		"num-slaves":          "1",
		"num-other-sentinels": "2",
	}
}

func TestParseMasterReply(t *testing.T) {
	want := &MasterInfo{
		Name: "rg-master", IP: "172.30.0.10", Port: "6379",
		Flags: "master", Quorum: "2", NumSlaves: "1", NumOtherSentinels: "2",
	}

	for name, reply := range map[string]any{"RESP2": resp2Master(), "RESP3": resp3Master()} {
		t.Run(name, func(t *testing.T) {
			got, err := parseMasterReply(reply)
			if err != nil {
				t.Fatalf("parseMasterReply: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("master = %+v, want %+v", got, want)
			}
		})
	}
}

func TestParseMasterReplyRejectsUnexpectedShapes(t *testing.T) {
	for name, reply := range map[string]any{
		"nil":       nil,
		"scalar":    "OK",
		"odd array": []any{"name", "rg-master", "ip"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseMasterReply(reply); err == nil {
				t.Errorf("parseMasterReply(%v) returned no error", reply)
			}
		})
	}
}

func TestParseReplicasReply(t *testing.T) {
	fields := []any{
		"name", "172.30.0.11:6379",
		"ip", "172.30.0.11",
		"port", "6379",
		"flags", "slave",
		"master-link-status", "ok",
		"master-host", "172.30.0.10",
		"master-port", "6379",
	}
	asMap := map[any]any{
		"name": "172.30.0.11:6379", "ip": "172.30.0.11", "port": "6379",
		"flags": "slave", "master-link-status": "ok",
		"master-host": "172.30.0.10", "master-port": "6379",
	}
	want := []ReplicaInfo{{
		Name: "172.30.0.11:6379", IP: "172.30.0.11", Port: "6379", Flags: "slave",
		MasterLinkStatus: "ok", MasterHost: "172.30.0.10", MasterPort: "6379",
	}}

	for name, reply := range map[string]any{
		"RESP2": []any{fields},
		"RESP3": []any{asMap},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseReplicasReply(reply)
			if err != nil {
				t.Fatalf("parseReplicasReply: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("replicas = %+v, want %+v", got, want)
			}
		})
	}
}

func TestParseSentinelsReply(t *testing.T) {
	want := []map[string]string{
		{"name": "a1", "ip": "172.30.0.21", "port": "26379", "flags": "sentinel"},
		{"name": "b2", "ip": "172.30.0.22", "port": "26379", "flags": "s_down,sentinel"},
	}

	for name, reply := range map[string]any{
		"RESP2": []any{
			[]any{"name", "a1", "ip", "172.30.0.21", "port", "26379", "flags", "sentinel"},
			[]any{"name", "b2", "ip", "172.30.0.22", "port", "26379", "flags", "s_down,sentinel"},
		},
		"RESP3": []any{
			map[any]any{"name": "a1", "ip": "172.30.0.21", "port": "26379", "flags": "sentinel"},
			map[any]any{"name": "b2", "ip": "172.30.0.22", "port": "26379", "flags": "s_down,sentinel"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseSentinelsReply(reply)
			if err != nil {
				t.Fatalf("parseSentinelsReply: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("sentinels = %+v, want %+v", got, want)
			}
		})
	}
}

// A malformed entry must surface rather than shrink the list: an empty result
// is indistinguishable from "no sentinels are up", which is what the caller
// alerts on.
func TestParseSentinelsReplySurfacesMalformedEntries(t *testing.T) {
	_, err := parseSentinelsReply([]any{"not-an-entry"})
	if err == nil {
		t.Fatal("malformed entry silently dropped")
	}
	if !strings.Contains(err.Error(), "string") {
		t.Errorf("error %q does not name the offending type", err)
	}
}

// fakeSentinelServer is a minimal RESP2 endpoint: it refuses HELLO so go-redis
// falls back to RESP2, answers SENTINEL FAILOVER with a fixed reply, and counts
// how many failover commands reached it.
type fakeSentinelServer struct {
	ln            net.Listener
	failoverReply string

	mu        sync.Mutex
	failovers int
}

func newFakeSentinelServer(t *testing.T, failoverReply string) *fakeSentinelServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSentinelServer{ln: ln, failoverReply: failoverReply}
	t.Cleanup(func() { _ = ln.Close() })
	go s.serve()
	return s
}

func (s *fakeSentinelServer) addr() string { return s.ln.Addr().String() }

func (s *fakeSentinelServer) failoverCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failovers
}

func (s *fakeSentinelServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeSentinelServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	for {
		args, err := readRESPCommand(r)
		if err != nil {
			return
		}
		reply := "+OK\r\n"
		switch {
		case strings.EqualFold(args[0], "HELLO"):
			reply = "-ERR unknown command 'HELLO'\r\n"
		case strings.EqualFold(args[0], "SENTINEL") && len(args) > 1 && strings.EqualFold(args[1], "failover"):
			s.mu.Lock()
			s.failovers++
			s.mu.Unlock()
			reply = s.failoverReply + "\r\n"
		}
		if _, err := conn.Write([]byte(reply)); err != nil {
			return
		}
	}
}

// readRESPCommand parses one client command: an array of bulk strings.
func readRESPCommand(r *bufio.Reader) ([]string, error) {
	line, err := respLine(r)
	if err != nil {
		return nil, err
	}
	if len(line) < 2 || line[0] != '*' {
		return nil, fmt.Errorf("unexpected line %q", line)
	}
	n, err := strconv.Atoi(line[1:])
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, n)
	for i := 0; i < n; i++ {
		line, err := respLine(r)
		if err != nil {
			return nil, err
		}
		if len(line) < 2 || line[0] != '$' {
			return nil, fmt.Errorf("unexpected bulk header %q", line)
		}
		size, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}

func respLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// A second forced failover started while one is running is not agreed with the
// sentinel already driving the first: two sentinels then promote two different
// replicas. The -INPROG refusal means the promotion the caller wants is already
// happening, so it is success, not a reason to ask the next sentinel.
func TestFailoverFromPoolStopsOnInProgress(t *testing.T) {
	busy := newFakeSentinelServer(t, "-INPROG Failover already in progress")
	idle := newFakeSentinelServer(t, "+OK")

	pool := NewSentinelClientPool([]string{busy.addr(), idle.addr()}, "")
	if err := pool.FailoverFromPool(context.Background(), "rg-master"); err != nil {
		t.Fatalf("FailoverFromPool during an in-progress failover: %v", err)
	}
	if got := idle.failoverCount(); got != 0 {
		t.Errorf("the second sentinel received %d SENTINEL FAILOVER commands; an in-progress reply must end the traversal", got)
	}
}

func TestFailoverFromPoolTriesNextSentinelOnOtherErrors(t *testing.T) {
	broken := newFakeSentinelServer(t, "-ERR No such master with that name")
	healthy := newFakeSentinelServer(t, "+OK")

	pool := NewSentinelClientPool([]string{broken.addr(), healthy.addr()}, "")
	if err := pool.FailoverFromPool(context.Background(), "rg-master"); err != nil {
		t.Fatalf("FailoverFromPool: %v", err)
	}
	if got := healthy.failoverCount(); got != 1 {
		t.Errorf("the healthy sentinel received %d SENTINEL FAILOVER commands, want 1", got)
	}
}
