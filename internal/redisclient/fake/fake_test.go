package fake_test

import (
	"context"
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
