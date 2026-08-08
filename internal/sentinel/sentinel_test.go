package sentinel

import (
	"reflect"
	"strings"
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
