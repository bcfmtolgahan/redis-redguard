package fake_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/redguard/redguard/internal/redisclient/fake"
)

func TestACLSetUserRecordsEveryAddress(t *testing.T) {
	f := fake.NewFactory()
	ctx := context.Background()

	c1 := f.NewClient("10.0.0.1:6379", "", nil)
	c2 := f.NewClient("10.0.0.2:6379", "", nil)

	if err := c1.ACLSetUser(ctx, "app", "on", ">pw", "~*", "+@read"); err != nil {
		t.Fatal(err)
	}
	if err := c2.ACLSetUser(ctx, "app", "on", ">pw", "~*", "+@all"); err != nil {
		t.Fatal(err)
	}

	calls := f.Calls()
	for _, want := range []string{"10.0.0.1:6379:ACLSetUser", "10.0.0.2:6379:ACLSetUser"} {
		if !slices.Contains(calls, want) {
			t.Errorf("call log missing %q, got %v", want, calls)
		}
	}

	got := f.Users()["app"]
	want := []string{"on", ">pw", "~*", "+@all"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Users()[app] = %v, want the last write %v", got, want)
	}
}

func TestACLDelUserRemovesFromUsers(t *testing.T) {
	f := fake.NewFactory()
	ctx := context.Background()

	c := f.NewClient("10.0.0.1:6379", "", nil)
	if err := c.ACLSetUser(ctx, "app", "on"); err != nil {
		t.Fatal(err)
	}
	if err := c.ACLDelUser(ctx, "app"); err != nil {
		t.Fatal(err)
	}

	if _, ok := f.Users()["app"]; ok {
		t.Error("deleted user still present in Users()")
	}
	if !slices.Contains(f.Calls(), "10.0.0.1:6379:ACLDelUser") {
		t.Errorf("call log missing ACLDelUser, got %v", f.Calls())
	}
}

func TestACLSaveSnapshotsUsersPerNode(t *testing.T) {
	f := fake.NewFactory()
	ctx := context.Background()

	c := f.NewClient("10.0.0.1:6379", "", nil)
	if err := c.ACLSetUser(ctx, "app", "on", ">pw"); err != nil {
		t.Fatal(err)
	}
	if len(f.SavedUsers("10.0.0.1:6379")) != 0 {
		t.Error("an unsaved user must not count as durable state")
	}

	if err := c.ACLSave(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.SavedUsers("10.0.0.1:6379")["app"]; !ok {
		t.Errorf("ACLSave did not persist the user, got %v", f.SavedUsers("10.0.0.1:6379"))
	}
	if len(f.SavedUsers("10.0.0.2:6379")) != 0 {
		t.Error("saving on one node must not persist anything on another")
	}
	if !slices.Contains(f.Calls(), "10.0.0.1:6379:ACLSave") {
		t.Errorf("call log missing ACLSave, got %v", f.Calls())
	}

	if err := c.ACLDelUser(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.SavedUsers("10.0.0.1:6379")["app"]; !ok {
		t.Error("an unsaved deletion must not change durable state")
	}
	if err := c.ACLSave(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.SavedUsers("10.0.0.1:6379")["app"]; ok {
		t.Error("deleted user still durable after ACLSave")
	}
}

func TestSetMasterDrivesRolesAndSentinel(t *testing.T) {
	f := fake.NewFactory()
	ctx := context.Background()
	f.SetMaster("10.0.0.1:6379")

	pool := f.NewSentinelPool([]string{"s0:26379", "s1:26379", "s2:26379"}, "", nil)
	addr, err := pool.GetMasterAddrFromPool(ctx, "mymaster")
	if err != nil {
		t.Fatal(err)
	}
	if addr != "10.0.0.1:6379" {
		t.Errorf("GetMasterAddrFromPool = %q, want 10.0.0.1:6379", addr)
	}

	isMaster, err := f.NewClient("10.0.0.1:6379", "", nil).IsMaster(ctx)
	if err != nil || !isMaster {
		t.Errorf("configured master reports IsMaster = %v, %v", isMaster, err)
	}

	replica := f.NewClient("10.0.0.2:6379", "", nil)
	isMaster, err = replica.IsMaster(ctx)
	if err != nil || isMaster {
		t.Errorf("other node reports IsMaster = %v, %v; want replica", isMaster, err)
	}
	info, err := replica.GetReplicationInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info["role"] != "slave" || info["master_host"] != "10.0.0.1" || info["master_port"] != "6379" {
		t.Errorf("replica replication info %v does not point at the master", info)
	}
}

func TestSlaveOfReassignsRole(t *testing.T) {
	f := fake.NewFactory()
	ctx := context.Background()
	f.SetMaster("10.0.0.1:6379")

	c := f.NewClient("10.0.0.1:6379", "", nil)
	if err := c.SlaveOf(ctx, "10.0.0.9", "6379"); err != nil {
		t.Fatal(err)
	}
	info, err := c.GetReplicationInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info["role"] != "slave" || info["master_host"] != "10.0.0.9" {
		t.Errorf("after SlaveOf, replication info = %v", info)
	}
}

func TestSentinelPoolWithoutMasterErrors(t *testing.T) {
	f := fake.NewFactory()
	pool := f.NewSentinelPool([]string{"s0:26379"}, "", nil)
	if _, err := pool.GetMasterAddrFromPool(context.Background(), "mymaster"); err == nil {
		t.Error("expected an error when no master is configured")
	}
}

func TestSetKeyspacePopulatesKeyspaceInfoAndDBSize(t *testing.T) {
	f := fake.NewFactory()
	ctx := context.Background()

	f.SetKeyspace("10.0.0.1:6379", map[int]int64{0: 42, 2: 5})

	c := f.NewClient("10.0.0.1:6379", "", nil)
	info, err := c.GetKeyspaceInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info["db0"] != "keys=42,expires=0,avg_ttl=0" || info["db2"] != "keys=5,expires=0,avg_ttl=0" {
		t.Errorf("unexpected keyspace info: %v", info)
	}

	n, err := c.DBSize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 42 {
		t.Errorf("DBSize = %d, want 42 (DB 0 only, like the real command)", n)
	}
}

func TestSetErrorInjectsPerAddressPerMethod(t *testing.T) {
	f := fake.NewFactory()
	ctx := context.Background()

	boom := errors.New("boom")
	f.SetError("10.0.0.1:6379", "Ping", boom)

	if err := f.NewClient("10.0.0.1:6379", "", nil).Ping(ctx); !errors.Is(err, boom) {
		t.Errorf("Ping on the failing node = %v, want the injected error", err)
	}
	if err := f.NewClient("10.0.0.2:6379", "", nil).Ping(ctx); err != nil {
		t.Errorf("Ping on another node = %v, want nil", err)
	}
}

func TestConfigSetAndShutdownNoSaveAreRecorded(t *testing.T) {
	f := fake.NewFactory()
	ctx := context.Background()

	c := f.NewClient("10.0.0.1:6379", "", nil)
	if err := c.ConfigSet(ctx, "appendonly", "yes"); err != nil {
		t.Fatal(err)
	}
	if err := c.ShutdownNoSave(ctx); err != nil {
		t.Fatal(err)
	}

	calls := f.Calls()
	for _, want := range []string{"10.0.0.1:6379:ConfigSet", "10.0.0.1:6379:ShutdownNoSave"} {
		if !slices.Contains(calls, want) {
			t.Errorf("call log missing %q, got %v", want, calls)
		}
	}
	if got := f.Configs("10.0.0.1:6379")["appendonly"]; got != "yes" {
		t.Errorf("ConfigSet value not stored, got %q", got)
	}
}

func TestSetMasterOptionAllRecordsOptionAcrossPool(t *testing.T) {
	f := fake.NewFactory()
	ctx := context.Background()

	p := f.NewSentinelPool([]string{"s0:26379", "s1:26379"}, "", nil)
	if err := p.SetMasterOptionAll(ctx, "m", "down-after-milliseconds", "600000"); err != nil {
		t.Fatal(err)
	}
	if got := f.MasterOptions()["down-after-milliseconds"]; got != "600000" {
		t.Errorf("MasterOptions = %v, want down-after-milliseconds=600000", f.MasterOptions())
	}
	if !slices.Contains(f.Calls(), "s0:26379,s1:26379:SetMasterOptionAll") {
		t.Errorf("call log missing SetMasterOptionAll, got %v", f.Calls())
	}
}

func TestSetErrorReachesSentinelPoolMethods(t *testing.T) {
	f := fake.NewFactory()
	ctx := context.Background()

	boom := errors.New("sentinel down")
	f.SetError("s0:26379,s1:26379", "SetMasterOptionAll", boom)

	p := f.NewSentinelPool([]string{"s0:26379", "s1:26379"}, "", nil)
	if err := p.SetMasterOptionAll(ctx, "m", "down-after-milliseconds", "600000"); !errors.Is(err, boom) {
		t.Errorf("SetMasterOptionAll = %v, want the injected error", err)
	}
}
