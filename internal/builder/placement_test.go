package builder

import (
	"maps"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisv1alpha1 "github.com/bcfmtolgahan/redis-redguard/api/v1alpha1"
)

// bothStatefulSets returns the two pod owners keyed by the component whose
// placement block feeds them, so every placement assertion covers both.
func bothStatefulSets(rs *redisv1alpha1.RedisSentinel) map[string]*appsv1.StatefulSet {
	return map[string]*appsv1.StatefulSet{
		"redis":    BuildRedisStatefulSet(rs),
		"sentinel": BuildSentinelStatefulSet(rs),
	}
}

// TestDefaultAntiAffinitySpreadsEachComponent pins the failure the operator
// exists to prevent: with no affinity at all the scheduler may put all three
// Redis pods and all three Sentinels on one node, so one node failure takes
// out the cluster that is advertised as highly available.
func TestDefaultAntiAffinitySpreadsEachComponent(t *testing.T) {
	rs := testSentinel()

	for component, sts := range bothStatefulSets(rs) {
		affinity := sts.Spec.Template.Spec.Affinity
		if affinity == nil || affinity.PodAntiAffinity == nil {
			t.Fatalf("%s: pod template has no podAntiAffinity; every pod may land on one node", component)
		}

		terms := affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution
		if len(terms) != 1 {
			t.Fatalf("%s: got %d preferred anti-affinity terms, want 1", component, len(terms))
		}
		if got, want := terms[0].PodAffinityTerm.TopologyKey, "kubernetes.io/hostname"; got != want {
			t.Errorf("%s: topologyKey = %q, want %q", component, got, want)
		}

		selector := terms[0].PodAffinityTerm.LabelSelector
		if selector == nil {
			t.Fatalf("%s: anti-affinity term has no labelSelector, so it repels nothing", component)
		}
		want := buildLabels(rs, component)
		if !maps.Equal(selector.MatchLabels, want) {
			t.Errorf("%s: anti-affinity selector = %v, want %v", component, selector.MatchLabels, want)
		}
		if !maps.Equal(selector.MatchLabels, sts.Spec.Template.Labels) {
			t.Errorf("%s: anti-affinity selector %v does not match the pods it is meant to spread %v",
				component, selector.MatchLabels, sts.Spec.Template.Labels)
		}
	}
}

// TestDefaultAntiAffinityIsPreferredNotRequired keeps a single-node cluster
// schedulable. A required term would leave two of three pods Pending forever on
// kind or a laptop, which is where the operator is first tried.
func TestDefaultAntiAffinityIsPreferredNotRequired(t *testing.T) {
	for component, sts := range bothStatefulSets(testSentinel()) {
		affinity := sts.Spec.Template.Spec.Affinity
		if affinity == nil || affinity.PodAntiAffinity == nil {
			t.Fatalf("%s: pod template has no podAntiAffinity", component)
		}
		pa := affinity.PodAntiAffinity
		if len(pa.RequiredDuringSchedulingIgnoredDuringExecution) != 0 {
			t.Errorf("%s: default anti-affinity is required, so a single-node cluster cannot schedule the pods: %v",
				component, pa.RequiredDuringSchedulingIgnoredDuringExecution)
		}
	}
}

// TestUserPodAntiAffinityReplacesTheDefault covers the operator who wants a
// hard guarantee, or a different topology key than the node.
func TestUserPodAntiAffinityReplacesTheDefault(t *testing.T) {
	hard := &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				TopologyKey:   "topology.kubernetes.io/zone",
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"custom": "yes"}},
			}},
		},
	}

	rs := testSentinel()
	rs.Spec.RedisConfig.Affinity = hard.DeepCopy()
	rs.Spec.SentinelConfig.Affinity = hard.DeepCopy()

	for component, sts := range bothStatefulSets(rs) {
		got := sts.Spec.Template.Spec.Affinity
		if !reflect.DeepEqual(got, hard) {
			t.Errorf("%s: affinity = %+v, want the spec verbatim %+v", component, got, hard)
		}
	}
}

// TestNodeAffinityAloneKeepsTheDefaultSpreading: pinning pods to a node pool is
// unrelated to spreading them inside it, so setting one must not silently drop
// the other.
func TestNodeAffinityAloneKeepsTheDefaultSpreading(t *testing.T) {
	nodeOnly := &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      "workload",
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{"redis"},
					}},
				}},
			},
		},
	}

	rs := testSentinel()
	rs.Spec.RedisConfig.Affinity = nodeOnly.DeepCopy()
	rs.Spec.SentinelConfig.Affinity = nodeOnly.DeepCopy()

	for component, sts := range bothStatefulSets(rs) {
		affinity := sts.Spec.Template.Spec.Affinity
		if affinity == nil {
			t.Fatalf("%s: the affinity from the spec was dropped entirely", component)
		}
		if affinity.NodeAffinity == nil {
			t.Errorf("%s: the node affinity from the spec was dropped", component)
		}
		if affinity.PodAntiAffinity == nil ||
			len(affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
			t.Errorf("%s: setting nodeAffinity silently removed the default spreading: %+v", component, affinity)
		}
	}
}

// TestEmptyPodAntiAffinityOptsOutOfSpreading gives the user a way to say "no
// anti-affinity at all", which is otherwise unreachable once a default exists.
func TestEmptyPodAntiAffinityOptsOutOfSpreading(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{}}
	rs.Spec.SentinelConfig.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{}}

	for component, sts := range bothStatefulSets(rs) {
		affinity := sts.Spec.Template.Spec.Affinity
		if affinity == nil || affinity.PodAntiAffinity == nil {
			t.Fatalf("%s: the affinity from the spec was dropped entirely", component)
		}
		pa := affinity.PodAntiAffinity
		if len(pa.PreferredDuringSchedulingIgnoredDuringExecution) != 0 ||
			len(pa.RequiredDuringSchedulingIgnoredDuringExecution) != 0 {
			t.Errorf("%s: an explicitly empty podAntiAffinity was overwritten by the default: %+v", component, pa)
		}
	}
}

// TestPlacementReachesThePodSpec covers the fields that are pure passthrough.
// HANDBOOK documented a podAntiAffinity field under redisConfig that the API
// never had, so anything a user wrote there was pruned without a word.
func TestPlacementReachesThePodSpec(t *testing.T) {
	nodeSelector := map[string]string{"disktype": "ssd"}
	tolerations := []corev1.Toleration{{
		Key:      "dedicated",
		Operator: corev1.TolerationOpEqual,
		Value:    "redis",
		Effect:   corev1.TaintEffectNoSchedule,
	}}
	spread := []corev1.TopologySpreadConstraint{{
		MaxSkew:           1,
		TopologyKey:       "topology.kubernetes.io/zone",
		WhenUnsatisfiable: corev1.ScheduleAnyway,
		LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "redis"}},
	}}

	rs := testSentinel()
	rs.Spec.RedisConfig.NodeSelector = nodeSelector
	rs.Spec.RedisConfig.Tolerations = tolerations
	rs.Spec.RedisConfig.TopologySpreadConstraints = spread
	rs.Spec.RedisConfig.PriorityClassName = "redis-critical"
	rs.Spec.SentinelConfig.NodeSelector = nodeSelector
	rs.Spec.SentinelConfig.Tolerations = tolerations
	rs.Spec.SentinelConfig.TopologySpreadConstraints = spread
	rs.Spec.SentinelConfig.PriorityClassName = "sentinel-critical"

	for component, sts := range bothStatefulSets(rs) {
		spec := sts.Spec.Template.Spec
		if !maps.Equal(spec.NodeSelector, nodeSelector) {
			t.Errorf("%s: nodeSelector = %v, want %v", component, spec.NodeSelector, nodeSelector)
		}
		if !reflect.DeepEqual(spec.Tolerations, tolerations) {
			t.Errorf("%s: tolerations = %v, want %v", component, spec.Tolerations, tolerations)
		}
		if !reflect.DeepEqual(spec.TopologySpreadConstraints, spread) {
			t.Errorf("%s: topologySpreadConstraints = %v, want %v", component, spec.TopologySpreadConstraints, spread)
		}
	}

	if got := BuildRedisStatefulSet(rs).Spec.Template.Spec.PriorityClassName; got != "redis-critical" {
		t.Errorf("redis priorityClassName = %q, want %q", got, "redis-critical")
	}
	if got := BuildSentinelStatefulSet(rs).Spec.Template.Spec.PriorityClassName; got != "sentinel-critical" {
		t.Errorf("sentinel priorityClassName = %q, want %q", got, "sentinel-critical")
	}
}

// TestRedisPlacementDoesNotReachSentinel: the two components are scheduled
// independently, and a node pool sized for Redis is rarely where the sentinels
// belong. Reading the wrong block would strand one of them Pending.
func TestRedisPlacementDoesNotReachSentinel(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.NodeSelector = map[string]string{"pool": "redis"}
	rs.Spec.RedisConfig.PriorityClassName = "redis-critical"
	rs.Spec.RedisConfig.Tolerations = []corev1.Toleration{{Key: "redis", Operator: corev1.TolerationOpExists}}

	spec := BuildSentinelStatefulSet(rs).Spec.Template.Spec
	if len(spec.NodeSelector) != 0 {
		t.Errorf("sentinel pods inherited the Redis nodeSelector: %v", spec.NodeSelector)
	}
	if spec.PriorityClassName != "" {
		t.Errorf("sentinel pods inherited the Redis priorityClassName: %q", spec.PriorityClassName)
	}
	if len(spec.Tolerations) != 0 {
		t.Errorf("sentinel pods inherited the Redis tolerations: %v", spec.Tolerations)
	}
}

// TestPlacementDoesNotChangeTheConfigHash: placement is not part of either
// server's configuration file, so editing it must not look like a config
// change. It still replaces the pods, which is the point of moving them.
func TestPlacementDoesNotChangeTheConfigHash(t *testing.T) {
	before := BuildRedisStatefulSet(testSentinel()).Spec.Template.Annotations[ConfigHashAnnotation]

	rs := testSentinel()
	rs.Spec.RedisConfig.NodeSelector = map[string]string{"pool": "redis"}
	after := BuildRedisStatefulSet(rs).Spec.Template.Annotations[ConfigHashAnnotation]

	if before != after {
		t.Errorf("config hash changed on a placement-only edit: %s then %s", before, after)
	}
}
