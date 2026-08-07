package builder

import (
	"maps"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	policyv1 "k8s.io/api/policy/v1"
)

// TestPodDisruptionBudgetsBoundVoluntaryDisruption pins the gap a node drain
// exposes: without a budget the eviction API takes every Redis pod at once, so
// draining one node of a co-located cluster ends the cluster.
func TestPodDisruptionBudgetsBoundVoluntaryDisruption(t *testing.T) {
	rs := testSentinel()

	cases := map[string]struct {
		pdb *policyv1.PodDisruptionBudget
		sts *appsv1.StatefulSet
	}{
		"redis": {
			pdb: BuildRedisPodDisruptionBudget(rs),
			sts: BuildRedisStatefulSet(rs),
		},
		"sentinel": {
			pdb: BuildSentinelPodDisruptionBudget(rs),
			sts: BuildSentinelStatefulSet(rs),
		},
	}

	for component, tc := range cases {
		pdb, sts := tc.pdb, tc.sts

		if got, want := pdb.Name, "test-rs-"+component+"-pdb"; got != want {
			t.Errorf("%s: PDB name = %q, want %q", component, got, want)
		}
		if got, want := pdb.Namespace, rs.Namespace; got != want {
			t.Errorf("%s: PDB namespace = %q, want %q", component, got, want)
		}

		if pdb.Spec.MinAvailable != nil {
			t.Errorf("%s: minAvailable is set alongside maxUnavailable; only one may be", component)
		}
		if pdb.Spec.MaxUnavailable == nil {
			t.Fatalf("%s: maxUnavailable is nil, so the budget constrains nothing", component)
		}
		if got := pdb.Spec.MaxUnavailable.IntValue(); got != 1 {
			t.Errorf("%s: maxUnavailable = %d, want 1", component, got)
		}

		if pdb.Spec.Selector == nil {
			t.Fatalf("%s: PDB has no selector", component)
		}
		if !maps.Equal(pdb.Spec.Selector.MatchLabels, sts.Spec.Template.Labels) {
			t.Errorf("%s: PDB selects %v but the pods carry %v; the budget guards nothing",
				component, pdb.Spec.Selector.MatchLabels, sts.Spec.Template.Labels)
		}

		// A pod that is not Ready serves no traffic and counts for no quorum.
		// Under the default policy it is undisruptible while the budget is
		// exhausted, which turns a drain into an indefinite hang.
		if pdb.Spec.UnhealthyPodEvictionPolicy == nil ||
			*pdb.Spec.UnhealthyPodEvictionPolicy != policyv1.AlwaysAllow {
			t.Errorf("%s: unhealthyPodEvictionPolicy = %v, want AlwaysAllow so a broken pod cannot block a drain",
				component, pdb.Spec.UnhealthyPodEvictionPolicy)
		}
	}
}

// TestPodDisruptionBudgetsAreDisjoint: one budget covering both components
// would let a drain take a Redis pod and a Sentinel together and count that as
// a single disruption.
func TestPodDisruptionBudgetsAreDisjoint(t *testing.T) {
	rs := testSentinel()
	redis := BuildRedisPodDisruptionBudget(rs).Spec.Selector.MatchLabels
	sentinel := BuildSentinelPodDisruptionBudget(rs).Spec.Selector.MatchLabels

	if maps.Equal(redis, sentinel) {
		t.Fatalf("both PDBs select the same pods: %v", redis)
	}
	if redis["app.kubernetes.io/component"] != "redis" {
		t.Errorf("redis PDB component selector = %q", redis["app.kubernetes.io/component"])
	}
	if sentinel["app.kubernetes.io/component"] != "sentinel" {
		t.Errorf("sentinel PDB component selector = %q", sentinel["app.kubernetes.io/component"])
	}
	if redis["app.kubernetes.io/instance"] != rs.Name || sentinel["app.kubernetes.io/instance"] != rs.Name {
		t.Errorf("a PDB selects pods of another RedisSentinel: redis=%v sentinel=%v", redis, sentinel)
	}
}

// TestPodDisruptionBudgetsFollowTheReplicaCount documents the one budget that
// protects nothing: a single-replica Redis has no availability to preserve, and
// blocking its eviction would wedge every node drain for no gain.
func TestPodDisruptionBudgetsFollowTheReplicaCount(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Replicas = 1

	pdb := BuildRedisPodDisruptionBudget(rs)
	if got := pdb.Spec.MaxUnavailable.IntValue(); got != 1 {
		t.Errorf("maxUnavailable = %d for a single replica, want 1", got)
	}
}

// TestPodDisruptionBudgetLabels keeps the objects discoverable by the same
// selector as everything else the operator owns for this CR.
func TestPodDisruptionBudgetLabels(t *testing.T) {
	rs := testSentinel()
	for component, pdb := range map[string]*policyv1.PodDisruptionBudget{
		"redis":    BuildRedisPodDisruptionBudget(rs),
		"sentinel": BuildSentinelPodDisruptionBudget(rs),
	} {
		if !maps.Equal(pdb.Labels, buildLabels(rs, component)) {
			t.Errorf("%s: PDB labels = %v, want %v", component, pdb.Labels, buildLabels(rs, component))
		}
	}
}
