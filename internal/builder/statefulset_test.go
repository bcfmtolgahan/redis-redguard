package builder

import (
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// Redis StatefulSet
// ---------------------------------------------------------------------------

func TestBuildRedisStatefulSet_Metadata(t *testing.T) {
	rs := testSentinel()
	sts := BuildRedisStatefulSet(rs)

	if got, want := sts.Name, "test-rs-redis"; got != want {
		t.Errorf("StatefulSet name = %q, want %q", got, want)
	}
	if got, want := sts.Namespace, "default"; got != want {
		t.Errorf("StatefulSet namespace = %q, want %q", got, want)
	}
	if got, want := sts.Spec.ServiceName, "test-rs-redis-headless"; got != want {
		t.Errorf("serviceName = %q, want %q", got, want)
	}
	if got, want := sts.Spec.PodManagementPolicy, appsv1.ParallelPodManagement; got != want {
		t.Errorf("podManagementPolicy = %q, want %q", got, want)
	}
	if got, want := sts.Spec.UpdateStrategy.Type, appsv1.RollingUpdateStatefulSetStrategyType; got != want {
		t.Errorf("updateStrategy = %q, want %q", got, want)
	}
}

func TestBuildRedisStatefulSet_ReplicaCount(t *testing.T) {
	for _, replicas := range []int32{1, 3, 5} {
		rs := testSentinel()
		rs.Spec.RedisConfig.Replicas = replicas

		sts := BuildRedisStatefulSet(rs)

		if sts.Spec.Replicas == nil {
			t.Fatalf("replicas=%d: Spec.Replicas is nil", replicas)
		}
		if *sts.Spec.Replicas != replicas {
			t.Errorf("Spec.Replicas = %d, want %d", *sts.Spec.Replicas, replicas)
		}
	}
}

// TestBuildRedisStatefulSet_SelectorMatchesPodLabels guards the most common
// StatefulSet authoring bug: a selector that does not match the pod template.
func TestBuildRedisStatefulSet_SelectorMatchesPodLabels(t *testing.T) {
	sts := BuildRedisStatefulSet(testSentinel())

	if sts.Spec.Selector == nil {
		t.Fatal("Spec.Selector is nil")
	}
	if len(sts.Spec.Selector.MatchLabels) == 0 {
		t.Fatal("Spec.Selector.MatchLabels is empty; would select every pod")
	}
	podLabels := sts.Spec.Template.Labels
	for k, v := range sts.Spec.Selector.MatchLabels {
		if podLabels[k] != v {
			t.Errorf("selector wants %s=%s but pod template carries %v", k, v, podLabels)
		}
	}
	if c := podLabels["app.kubernetes.io/component"]; c != "redis" {
		t.Errorf("pod component label = %q, want %q", c, "redis")
	}
}

func TestBuildRedisStatefulSet_VolumeClaimTemplate(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Storage = &redisv1alpha1.StorageSpec{
		Size:             resource.MustParse("10Gi"),
		StorageClassName: ptr.To("gp3"),
	}

	sts := BuildRedisStatefulSet(rs)

	if len(sts.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("got %d volumeClaimTemplates, want 1", len(sts.Spec.VolumeClaimTemplates))
	}
	vct := sts.Spec.VolumeClaimTemplates[0]

	if vct.Name != "data" {
		t.Errorf("volumeClaimTemplate name = %q, want %q", vct.Name, "data")
	}
	if len(vct.Spec.AccessModes) != 1 || vct.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("accessModes = %v, want [ReadWriteOnce]", vct.Spec.AccessModes)
	}
	gotSize := vct.Spec.Resources.Requests[corev1.ResourceStorage]
	wantSize := resource.MustParse("10Gi")
	if gotSize.Cmp(wantSize) != 0 {
		t.Errorf("storage request = %s, want %s", gotSize.String(), wantSize.String())
	}
	if vct.Spec.StorageClassName == nil || *vct.Spec.StorageClassName != "gp3" {
		t.Errorf("storageClassName = %v, want %q", vct.Spec.StorageClassName, "gp3")
	}

	// The claim template must be mounted at /data by the redis container.
	if !hasVolumeMount(sts.Spec.Template.Spec.Containers[0], "data", "/data") {
		t.Errorf("container does not mount the `data` claim at /data: %v",
			sts.Spec.Template.Spec.Containers[0].VolumeMounts)
	}
}

func TestBuildRedisStatefulSet_DefaultStorage(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Storage = nil

	vct := BuildRedisStatefulSet(rs).Spec.VolumeClaimTemplates[0]

	gotSize := vct.Spec.Resources.Requests[corev1.ResourceStorage]
	wantSize := resource.MustParse("1Gi")
	if gotSize.Cmp(wantSize) != 0 {
		t.Errorf("default storage request = %s, want %s", gotSize.String(), wantSize.String())
	}
	if vct.Spec.StorageClassName != nil {
		t.Errorf("storageClassName = %q, want nil (cluster default)", *vct.Spec.StorageClassName)
	}
}

func TestBuildRedisStatefulSet_InitScriptMounted(t *testing.T) {
	sts := BuildRedisStatefulSet(testSentinel())
	c := sts.Spec.Template.Spec.Containers[0]

	wantCmd := []string{"/bin/sh", "-c", "/etc/redis/init.sh"}
	if strings.Join(c.Command, " ") != strings.Join(wantCmd, " ") {
		t.Errorf("container command = %v, want %v", c.Command, wantCmd)
	}
	if !hasVolumeMount(c, "config", "/etc/redis") {
		t.Errorf("config volume not mounted at /etc/redis: %v", c.VolumeMounts)
	}

	vol, ok := findVolume(sts.Spec.Template.Spec.Volumes, "config")
	if !ok {
		t.Fatalf("no `config` volume in %v", sts.Spec.Template.Spec.Volumes)
	}
	if vol.ConfigMap == nil {
		t.Fatal("`config` volume is not backed by a ConfigMap")
	}
	if got, want := vol.ConfigMap.Name, "test-rs-redis-config"; got != want {
		t.Errorf("config volume ConfigMap = %q, want %q", got, want)
	}
	// init.sh must be executable when projected.
	if vol.ConfigMap.DefaultMode == nil || *vol.ConfigMap.DefaultMode != 0755 {
		t.Errorf("config volume defaultMode = %v, want 0755 so init.sh is executable", vol.ConfigMap.DefaultMode)
	}
	// The ConfigMap the volume names must be the one the builder actually creates.
	if cmName := BuildRedisConfigMap(testSentinel()).Name; cmName != vol.ConfigMap.Name {
		t.Errorf("StatefulSet mounts ConfigMap %q but BuildRedisConfigMap creates %q", vol.ConfigMap.Name, cmName)
	}
}

func TestBuildRedisStatefulSet_Probes(t *testing.T) {
	c := BuildRedisStatefulSet(testSentinel()).Spec.Template.Spec.Containers[0]

	for name, probe := range map[string]*corev1.Probe{
		"liveness":  c.LivenessProbe,
		"readiness": c.ReadinessProbe,
	} {
		if probe == nil {
			t.Errorf("%s probe is nil", name)
			continue
		}
		if probe.Exec == nil || len(probe.Exec.Command) == 0 {
			t.Errorf("%s probe has no exec command", name)
			continue
		}
		cmd := strings.Join(probe.Exec.Command, " ")
		if !strings.Contains(cmd, "redis-cli") || !strings.Contains(cmd, "ping") {
			t.Errorf("%s probe command does not ping redis: %q", name, cmd)
		}
		if probe.PeriodSeconds == 0 || probe.TimeoutSeconds == 0 || probe.FailureThreshold == 0 {
			t.Errorf("%s probe has unset timings: period=%d timeout=%d failureThreshold=%d",
				name, probe.PeriodSeconds, probe.TimeoutSeconds, probe.FailureThreshold)
		}
	}
}

func TestBuildRedisStatefulSet_Ports(t *testing.T) {
	c := BuildRedisStatefulSet(testSentinel()).Spec.Template.Spec.Containers[0]

	if len(c.Ports) != 1 {
		t.Fatalf("got %d container ports, want 1: %v", len(c.Ports), c.Ports)
	}
	if c.Ports[0].ContainerPort != 6379 || c.Ports[0].Name != "redis" {
		t.Errorf("port = %+v, want name=redis containerPort=6379", c.Ports[0])
	}
}

func TestBuildRedisStatefulSet_AuthProjectsPasswordFromSecret(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: "redis-pass"}

	c := BuildRedisStatefulSet(rs).Spec.Template.Spec.Containers[0]

	var found *corev1.EnvVar
	for i := range c.Env {
		if c.Env[i].Name == "REDIS_PASSWORD" {
			found = &c.Env[i]
		}
	}
	if found == nil {
		t.Fatalf("no REDIS_PASSWORD env var, got %v", c.Env)
	}
	if found.Value != "" {
		t.Errorf("REDIS_PASSWORD has a literal value %q; it must come from a secretKeyRef", found.Value)
	}
	if found.ValueFrom == nil || found.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("REDIS_PASSWORD is not sourced from a secretKeyRef: %+v", found.ValueFrom)
	}
	if got, want := found.ValueFrom.SecretKeyRef.Name, "redis-pass"; got != want {
		t.Errorf("secretKeyRef name = %q, want %q", got, want)
	}
	if got, want := found.ValueFrom.SecretKeyRef.Key, "password"; got != want {
		t.Errorf("secretKeyRef key = %q, want %q", got, want)
	}

	// The probe must authenticate via the env var, never an inline password.
	cmd := strings.Join(c.LivenessProbe.Exec.Command, " ")
	if !strings.Contains(cmd, "REDISCLI_AUTH=$REDIS_PASSWORD") {
		t.Errorf("auth-enabled probe does not use REDISCLI_AUTH: %q", cmd)
	}
	if strings.Contains(cmd, "-a ") {
		t.Errorf("probe passes the password on the command line (visible in ps): %q", cmd)
	}
}

func TestBuildRedisStatefulSet_NoAuthHasNoPasswordEnv(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Auth = nil

	c := BuildRedisStatefulSet(rs).Spec.Template.Spec.Containers[0]

	for _, e := range c.Env {
		if e.Name == "REDIS_PASSWORD" {
			t.Errorf("auth disabled but REDIS_PASSWORD env var is present: %+v", e)
		}
	}
	if cmd := strings.Join(c.LivenessProbe.Exec.Command, " "); strings.Contains(cmd, "REDISCLI_AUTH") {
		t.Errorf("auth disabled but probe uses REDISCLI_AUTH: %q", cmd)
	}
}

func TestBuildRedisStatefulSet_TLSDisabled(t *testing.T) {
	rs := testSentinel()
	rs.Spec.TLS = nil

	sts := BuildRedisStatefulSet(rs)
	c := sts.Spec.Template.Spec.Containers[0]

	if _, ok := findVolume(sts.Spec.Template.Spec.Volumes, "tls-certs"); ok {
		t.Error("TLS disabled but a tls-certs volume was created")
	}
	if cmd := strings.Join(c.LivenessProbe.Exec.Command, " "); strings.Contains(cmd, "--tls") {
		t.Errorf("TLS disabled but probe uses --tls: %q", cmd)
	}
}

// TestBuildRedisStatefulSet_TLSSeparateCASecret covers the layout that used to
// make every pod unstartable: a CA in its own Secret was subPath-mounted at
// /etc/redis/tls/ca.crt, inside the directory the certificate volume already
// occupies, which the kubelet refuses ("not a directory", StartError exit 128).
// Both Secrets are projected into one volume instead, so every consumer keeps
// the same /etc/redis/tls paths regardless of the Secret layout.
func TestBuildRedisStatefulSet_TLSSeparateCASecret(t *testing.T) {
	rs := testSentinel()
	rs.Spec.TLS = &redisv1alpha1.TLSConfig{
		Enabled:              true,
		CertificateSecretRef: "redis-tls",
		CASecretRef:          "redis-ca",
	}

	sts := BuildRedisStatefulSet(rs)
	c := sts.Spec.Template.Spec.Containers[0]

	vol, ok := findVolume(sts.Spec.Template.Spec.Volumes, "tls-certs")
	if !ok {
		t.Fatalf("no tls-certs volume, got %v", sts.Spec.Template.Spec.Volumes)
	}
	if vol.Projected == nil {
		t.Fatalf("tls-certs = %+v, want a projected volume combining both Secrets", vol.VolumeSource)
	}
	// 0440 with fsGroup 1000: the secret files are root:1000, and the redis
	// process (uid 1000, supplementary gid 1000) reads via the group bit.
	// 0400 leaves the key readable by root only and TLS never comes up.
	if vol.Projected.DefaultMode == nil || *vol.Projected.DefaultMode != 0440 {
		t.Errorf("tls-certs defaultMode = %v, want 0440 so the non-root redis user can read the key", vol.Projected.DefaultMode)
	}
	if len(vol.Projected.Sources) != 2 ||
		vol.Projected.Sources[0].Secret == nil || vol.Projected.Sources[0].Secret.Name != "redis-tls" ||
		vol.Projected.Sources[1].Secret == nil || vol.Projected.Sources[1].Secret.Name != "redis-ca" {
		t.Fatalf("tls-certs sources = %+v, want the cert Secret and the CA Secret", vol.Projected.Sources)
	}
	// Pinned items keep a bundle Secret's own ca.crt from colliding with the
	// CA Secret's copy, which the kubelet rejects as a duplicate path.
	wantItems := map[int][]string{0: {"tls.crt", "tls.key"}, 1: {"ca.crt"}}
	for i, keys := range wantItems {
		var got []string
		for _, item := range vol.Projected.Sources[i].Secret.Items {
			got = append(got, item.Key)
		}
		if !slices.Equal(got, keys) {
			t.Errorf("source %d projects keys %v, want %v", i, got, keys)
		}
	}

	if !hasVolumeMount(c, "tls-certs", "/etc/redis/tls") {
		t.Errorf("tls-certs not mounted at /etc/redis/tls: %v", c.VolumeMounts)
	}
	if _, ok := findVolume(sts.Spec.Template.Spec.Volumes, "tls-ca"); ok {
		t.Error("a separate tls-ca volume exists; its subPath mount is what the kubelet refuses")
	}
	for _, m := range c.VolumeMounts {
		if m.SubPath != "" {
			t.Errorf("mount %s uses subPath %q inside another volume's directory", m.Name, m.SubPath)
		}
	}

	cmd := strings.Join(c.LivenessProbe.Exec.Command, " ")
	if !strings.Contains(cmd, "--tls") {
		t.Errorf("TLS enabled but probe does not use --tls: %q", cmd)
	}
	if !strings.Contains(cmd, "--cacert /etc/redis/tls/ca.crt") {
		t.Errorf("CA secret configured but probe does not pass --cacert: %q", cmd)
	}
}

// Without a CA secret nothing mounts ca.crt, so a probe that passes --cacert
// points at a file that does not exist and fails on every tick.
func TestBuildRedisStatefulSet_TLSWithoutCAOmitsCacert(t *testing.T) {
	rs := testSentinel()
	rs.Spec.TLS = &redisv1alpha1.TLSConfig{
		Enabled:              true,
		CertificateSecretRef: "redis-tls",
	}

	c := BuildRedisStatefulSet(rs).Spec.Template.Spec.Containers[0]

	cmd := strings.Join(c.LivenessProbe.Exec.Command, " ")
	if !strings.Contains(cmd, "--tls") {
		t.Errorf("TLS enabled but probe does not use --tls: %q", cmd)
	}
	if strings.Contains(cmd, "--cacert") {
		t.Errorf("no CA secret configured but probe passes --cacert: %q", cmd)
	}
}

// When the CA lives in the same Secret as the cert, a plain secret volume
// serves the whole directory and no projection is needed.
func TestBuildRedisStatefulSet_TLSSharedCASecretMountsOneVolume(t *testing.T) {
	rs := testSentinel()
	rs.Spec.TLS = &redisv1alpha1.TLSConfig{
		Enabled:              true,
		CertificateSecretRef: "redis-tls",
		CASecretRef:          "redis-tls",
	}

	sts := BuildRedisStatefulSet(rs)
	c := sts.Spec.Template.Spec.Containers[0]

	if _, ok := findVolume(sts.Spec.Template.Spec.Volumes, "tls-ca"); ok {
		t.Error("CA shares the cert Secret but a separate tls-ca volume was created")
	}
	vol, ok := findVolume(sts.Spec.Template.Spec.Volumes, "tls-certs")
	if !ok || vol.Secret == nil || vol.Secret.SecretName != "redis-tls" {
		t.Fatalf("tls-certs = %+v, want a plain secret volume for the bundle Secret", vol.VolumeSource)
	}
	if vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != 0440 {
		t.Errorf("tls-certs defaultMode = %v, want 0440 so the non-root redis user can read the key", vol.Secret.DefaultMode)
	}
	if !hasVolumeMount(c, "tls-certs", "/etc/redis/tls") {
		t.Errorf("tls-certs not mounted at /etc/redis/tls: %v", c.VolumeMounts)
	}
	if cmd := strings.Join(c.LivenessProbe.Exec.Command, " "); !strings.Contains(cmd, "--cacert /etc/redis/tls/ca.crt") {
		t.Errorf("CA secret configured but probe does not pass --cacert: %q", cmd)
	}
}

// TestStatefulSets_StartupProbeCoversInitWait: both init scripts may wait up to
// 60s (30 attempts x 2s) for sentinels or master DNS before the server ever
// execs, but liveness counts from container start and its 45s budget would kill
// every such boot mid-init. The startup probe makes the liveness clock start at
// the first successful answer instead of at container start, so the two budgets
// no longer race; its own window must dominate the 60s init wait with room for
// the dataset load that follows exec.
func TestStatefulSets_StartupProbeCoversInitWait(t *testing.T) {
	rs := testSentinel()

	for name, sts := range map[string]*appsv1.StatefulSet{
		"redis":    BuildRedisStatefulSet(rs),
		"sentinel": BuildSentinelStatefulSet(rs),
	} {
		c := sts.Spec.Template.Spec.Containers[0]
		probe := c.StartupProbe
		if probe == nil {
			t.Errorf("%s: no startup probe, so liveness races the init script's 60s sentinel wait", name)
			continue
		}
		if probe.Exec == nil || len(probe.Exec.Command) == 0 {
			t.Errorf("%s: startup probe has no exec command", name)
			continue
		}
		if got, want := strings.Join(probe.Exec.Command, "\x00"), strings.Join(c.LivenessProbe.Exec.Command, "\x00"); got != want {
			t.Errorf("%s: startup probe runs %q, want the liveness command %q so both assert the same thing", name, got, want)
		}
		if budget := probe.PeriodSeconds * probe.FailureThreshold; budget < 120 {
			t.Errorf("%s: startup budget %ds does not dominate the init script's 60s retry loop", name, budget)
		}
	}
}

func TestBuildRedisStatefulSet_Image(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Image = "redis:7.2-alpine"
	if got := BuildRedisStatefulSet(rs).Spec.Template.Spec.Containers[0].Image; got != "redis:7.2-alpine" {
		t.Errorf("image = %q, want %q", got, "redis:7.2-alpine")
	}

	rs.Spec.RedisConfig.Image = ""
	if got := BuildRedisStatefulSet(rs).Spec.Template.Spec.Containers[0].Image; got != "redis:7-alpine" {
		t.Errorf("default image = %q, want %q", got, "redis:7-alpine")
	}
}

func TestBuildRedisStatefulSet_DefaultResources(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Resources = corev1.ResourceRequirements{}

	req := BuildRedisStatefulSet(rs).Spec.Template.Spec.Containers[0].Resources.Requests

	cpu := req[corev1.ResourceCPU]
	mem := req[corev1.ResourceMemory]
	if wantCPU := resource.MustParse("100m"); cpu.Cmp(wantCPU) != 0 {
		t.Errorf("default cpu request = %s, want 100m", cpu.String())
	}
	if wantMem := resource.MustParse("128Mi"); mem.Cmp(wantMem) != 0 {
		t.Errorf("default memory request = %s, want 128Mi", mem.String())
	}
}

func TestBuildRedisStatefulSet_ExplicitResourcesWin(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
	}

	res := BuildRedisStatefulSet(rs).Spec.Template.Spec.Containers[0].Resources

	cpu := res.Requests[corev1.ResourceCPU]
	if wantCPU := resource.MustParse("500m"); cpu.Cmp(wantCPU) != 0 {
		t.Errorf("cpu request = %s, want 500m", cpu.String())
	}
	limit := res.Limits[corev1.ResourceMemory]
	if wantLimit := resource.MustParse("4Gi"); limit.Cmp(wantLimit) != 0 {
		t.Errorf("memory limit = %s, want 4Gi", limit.String())
	}
}

// ---------------------------------------------------------------------------
// Sentinel StatefulSet
// ---------------------------------------------------------------------------

func TestBuildSentinelStatefulSet_Metadata(t *testing.T) {
	rs := testSentinel()
	sts := BuildSentinelStatefulSet(rs)

	if got, want := sts.Name, "test-rs-sentinel"; got != want {
		t.Errorf("StatefulSet name = %q, want %q", got, want)
	}
	if got, want := sts.Spec.ServiceName, "test-rs-sentinel-headless"; got != want {
		t.Errorf("serviceName = %q, want %q", got, want)
	}
	if c := sts.Spec.Template.Labels["app.kubernetes.io/component"]; c != "sentinel" {
		t.Errorf("pod component label = %q, want %q", c, "sentinel")
	}
}

func TestBuildSentinelStatefulSet_ReplicaCount(t *testing.T) {
	for _, replicas := range []int32{3, 5} {
		rs := testSentinel()
		rs.Spec.SentinelConfig.Replicas = replicas

		sts := BuildSentinelStatefulSet(rs)

		if sts.Spec.Replicas == nil || *sts.Spec.Replicas != replicas {
			t.Errorf("Spec.Replicas = %v, want %d", sts.Spec.Replicas, replicas)
		}
	}
}

// Sentinel rewrites its config with the learned master, replicas and failover
// epoch. Without a PVC that state dies with the container and a restarted
// sentinel re-monitors the bootstrap master.
func TestBuildSentinelStatefulSet_PersistsState(t *testing.T) {
	sts := BuildSentinelStatefulSet(testSentinel())

	if len(sts.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("got %d volumeClaimTemplates, want 1: %v",
			len(sts.Spec.VolumeClaimTemplates), sts.Spec.VolumeClaimTemplates)
	}
	vct := sts.Spec.VolumeClaimTemplates[0]

	if vct.Name != "sentinel-data" {
		t.Errorf("volumeClaimTemplate name = %q, want %q", vct.Name, "sentinel-data")
	}
	if len(vct.Spec.AccessModes) != 1 || vct.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("accessModes = %v, want [ReadWriteOnce]", vct.Spec.AccessModes)
	}
	gotSize := vct.Spec.Resources.Requests[corev1.ResourceStorage]
	if wantSize := resource.MustParse("1Gi"); gotSize.Cmp(wantSize) != 0 {
		t.Errorf("storage request = %s, want %s", gotSize.String(), wantSize.String())
	}
	if !hasVolumeMount(sts.Spec.Template.Spec.Containers[0], "sentinel-data", "/data") {
		t.Errorf("container does not mount the sentinel-data claim at /data: %v",
			sts.Spec.Template.Spec.Containers[0].VolumeMounts)
	}
}

// Sentinel state is derived: the master, the replicas and the failover epoch
// are relearned from a live cluster. Kept past the CR it is worse than absent,
// because a RedisSentinel recreated under the same name rebinds the old claims
// and the sentinels come up monitoring a dead master's IP with a stale quorum,
// with no repair path. Delete-and-recreate is what the CRD's immutability
// messages tell a user to do after a storage change, so this is reachable.
func TestBuildSentinelStatefulSet_PVCsDoNotOutliveTheCR(t *testing.T) {
	policy := BuildSentinelStatefulSet(testSentinel()).Spec.PersistentVolumeClaimRetentionPolicy

	if policy == nil {
		t.Fatal("persistentVolumeClaimRetentionPolicy is nil; recreating the cluster would rebind the old sentinel state")
	}
	if policy.WhenDeleted != appsv1.DeletePersistentVolumeClaimRetentionPolicyType {
		t.Errorf("whenDeleted = %q, want %q", policy.WhenDeleted, appsv1.DeletePersistentVolumeClaimRetentionPolicyType)
	}
	if policy.WhenScaled != appsv1.DeletePersistentVolumeClaimRetentionPolicyType {
		t.Errorf("whenScaled = %q, want %q", policy.WhenScaled, appsv1.DeletePersistentVolumeClaimRetentionPolicyType)
	}
}

// The Redis claims hold the dataset, the AOF and the ACL file. They are the
// user's data, not derived state, so they keep the default retain behaviour and
// survive both a delete and a scale-down.
func TestBuildRedisStatefulSet_PVCsOutliveTheCR(t *testing.T) {
	if policy := BuildRedisStatefulSet(testSentinel()).Spec.PersistentVolumeClaimRetentionPolicy; policy != nil {
		t.Errorf("persistentVolumeClaimRetentionPolicy = %+v, want nil (retain); the data claims must not be reaped with the CR", policy)
	}
}

func TestBuildSentinelStatefulSet_StorageClassFollowsRedis(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Storage = &redisv1alpha1.StorageSpec{
		Size:             resource.MustParse("10Gi"),
		StorageClassName: ptr.To("gp3"),
	}

	vct := BuildSentinelStatefulSet(rs).Spec.VolumeClaimTemplates[0]

	if vct.Spec.StorageClassName == nil || *vct.Spec.StorageClassName != "gp3" {
		t.Errorf("storageClassName = %v, want %q", vct.Spec.StorageClassName, "gp3")
	}
	// The size does not follow Redis: sentinel state is a single small file.
	gotSize := vct.Spec.Resources.Requests[corev1.ResourceStorage]
	if wantSize := resource.MustParse("1Gi"); gotSize.Cmp(wantSize) != 0 {
		t.Errorf("storage request = %s, want %s", gotSize.String(), wantSize.String())
	}
}

func TestBuildSentinelStatefulSet_NoStorageClassByDefault(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Storage = nil

	vct := BuildSentinelStatefulSet(rs).Spec.VolumeClaimTemplates[0]

	if vct.Spec.StorageClassName != nil {
		t.Errorf("storageClassName = %q, want nil (cluster default)", *vct.Spec.StorageClassName)
	}
}

func TestBuildSentinelStatefulSet_SelectorMatchesPodLabels(t *testing.T) {
	sts := BuildSentinelStatefulSet(testSentinel())

	if sts.Spec.Selector == nil || len(sts.Spec.Selector.MatchLabels) == 0 {
		t.Fatal("sentinel StatefulSet has no selector labels")
	}
	for k, v := range sts.Spec.Selector.MatchLabels {
		if sts.Spec.Template.Labels[k] != v {
			t.Errorf("selector wants %s=%s but pod template carries %v", k, v, sts.Spec.Template.Labels)
		}
	}
}

func TestBuildSentinelStatefulSet_InitScriptMountedAndPort(t *testing.T) {
	sts := BuildSentinelStatefulSet(testSentinel())
	c := sts.Spec.Template.Spec.Containers[0]

	if want := "/etc/sentinel/init.sh"; !strings.Contains(strings.Join(c.Command, " "), want) {
		t.Errorf("container command = %v, want it to run %q", c.Command, want)
	}
	if !hasVolumeMount(c, "config", "/etc/sentinel") {
		t.Errorf("config volume not mounted at /etc/sentinel: %v", c.VolumeMounts)
	}
	vol, ok := findVolume(sts.Spec.Template.Spec.Volumes, "config")
	if !ok || vol.ConfigMap == nil {
		t.Fatalf("no ConfigMap-backed config volume: %v", sts.Spec.Template.Spec.Volumes)
	}
	if got, want := vol.ConfigMap.Name, BuildSentinelConfigMap(testSentinel()).Name; got != want {
		t.Errorf("config volume ConfigMap = %q, want %q", got, want)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 26379 {
		t.Errorf("ports = %v, want a single 26379 port", c.Ports)
	}
}

func TestBuildSentinelStatefulSet_Probes(t *testing.T) {
	c := BuildSentinelStatefulSet(testSentinel()).Spec.Template.Spec.Containers[0]

	for name, probe := range map[string]*corev1.Probe{
		"liveness":  c.LivenessProbe,
		"readiness": c.ReadinessProbe,
	} {
		if probe == nil || probe.Exec == nil {
			t.Errorf("%s probe missing or has no exec handler", name)
			continue
		}
		cmd := strings.Join(probe.Exec.Command, " ")
		if !strings.Contains(cmd, "-p 26379") || !strings.Contains(cmd, "ping") {
			t.Errorf("%s probe does not ping sentinel on 26379: %q", name, cmd)
		}
	}
}

// TestBuildSentinelStatefulSet_ProbesAuthenticate pairs with requirepass on
// 26379: an unauthenticated PING answers NOAUTH, so a probe without
// REDISCLI_AUTH marks every sentinel unready and the StatefulSet never rolls.
func TestBuildSentinelStatefulSet_ProbesAuthenticate(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: "redis-pass"}

	for _, tc := range []struct {
		name string
		tls  *redisv1alpha1.TLSConfig
	}{
		{name: "plain"},
		{name: "tls", tls: &redisv1alpha1.TLSConfig{Enabled: true, CertificateSecretRef: "sentinel-tls"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := rs.DeepCopy()
			rs.Spec.TLS = tc.tls

			c := BuildSentinelStatefulSet(rs).Spec.Template.Spec.Containers[0]

			for name, probe := range map[string]*corev1.Probe{
				"liveness":  c.LivenessProbe,
				"readiness": c.ReadinessProbe,
			} {
				cmd := strings.Join(probe.Exec.Command, " ")
				if !strings.Contains(cmd, "REDISCLI_AUTH=$REDIS_PASSWORD") {
					t.Errorf("%s probe does not authenticate against a password-protected sentinel: %q", name, cmd)
				}
			}
		})
	}
}

func TestBuildSentinelStatefulSet_ProbesOmitAuthWhenUnset(t *testing.T) {
	c := BuildSentinelStatefulSet(testSentinel()).Spec.Template.Spec.Containers[0]

	if cmd := strings.Join(c.ReadinessProbe.Exec.Command, " "); strings.Contains(cmd, "REDISCLI_AUTH") {
		t.Errorf("no auth configured but probe references REDISCLI_AUTH: %q", cmd)
	}
}

func TestBuildSentinelStatefulSet_TLS(t *testing.T) {
	rs := testSentinel()
	rs.Spec.TLS = &redisv1alpha1.TLSConfig{
		Enabled:              true,
		CertificateSecretRef: "sentinel-tls",
		CASecretRef:          "sentinel-ca",
	}

	sts := BuildSentinelStatefulSet(rs)
	c := sts.Spec.Template.Spec.Containers[0]

	if !hasVolumeMount(c, "tls-certs", "/etc/sentinel/tls") {
		t.Errorf("tls-certs not mounted at /etc/sentinel/tls: %v", c.VolumeMounts)
	}
	vol, ok := findVolume(sts.Spec.Template.Spec.Volumes, "tls-certs")
	if !ok || vol.Projected == nil {
		t.Fatalf("tls-certs = %+v, want a projected volume combining both Secrets", vol.VolumeSource)
	}
	if vol.Projected.DefaultMode == nil || *vol.Projected.DefaultMode != 0440 {
		t.Errorf("tls-certs defaultMode = %v, want 0440 so the non-root redis user can read the key", vol.Projected.DefaultMode)
	}
	if _, ok := findVolume(sts.Spec.Template.Spec.Volumes, "tls-ca"); ok {
		t.Error("a separate tls-ca volume exists; its subPath mount is what the kubelet refuses")
	}
	for _, m := range c.VolumeMounts {
		if m.SubPath != "" {
			t.Errorf("mount %s uses subPath %q inside another volume's directory", m.Name, m.SubPath)
		}
	}
	cmd := strings.Join(c.LivenessProbe.Exec.Command, " ")
	if !strings.Contains(cmd, "--tls") {
		t.Errorf("TLS enabled but sentinel probe does not use --tls: %q", cmd)
	}
	if !strings.Contains(cmd, "--cacert /etc/sentinel/tls/ca.crt") {
		t.Errorf("CA secret configured but sentinel probe does not pass --cacert: %q", cmd)
	}
}

func TestBuildSentinelStatefulSet_TLSWithoutCAOmitsCacert(t *testing.T) {
	rs := testSentinel()
	rs.Spec.TLS = &redisv1alpha1.TLSConfig{
		Enabled:              true,
		CertificateSecretRef: "sentinel-tls",
	}

	c := BuildSentinelStatefulSet(rs).Spec.Template.Spec.Containers[0]

	cmd := strings.Join(c.LivenessProbe.Exec.Command, " ")
	if !strings.Contains(cmd, "--tls") {
		t.Errorf("TLS enabled but sentinel probe does not use --tls: %q", cmd)
	}
	if strings.Contains(cmd, "--cacert") {
		t.Errorf("no CA secret configured but sentinel probe passes --cacert: %q", cmd)
	}
}

func TestBuildSentinelStatefulSet_AuthProjectsPasswordFromSecret(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: "redis-pass"}

	c := BuildSentinelStatefulSet(rs).Spec.Template.Spec.Containers[0]

	if len(c.Env) != 1 || c.Env[0].Name != "REDIS_PASSWORD" {
		t.Fatalf("env = %v, want a single REDIS_PASSWORD entry", c.Env)
	}
	ref := c.Env[0].ValueFrom
	if ref == nil || ref.SecretKeyRef == nil || ref.SecretKeyRef.Name != "redis-pass" || ref.SecretKeyRef.Key != "password" {
		t.Errorf("REDIS_PASSWORD source = %+v, want secretKeyRef redis-pass/password", ref)
	}
}

// ---------------------------------------------------------------------------
// shared
// ---------------------------------------------------------------------------

func TestStatefulSets_PodSecurityContext(t *testing.T) {
	rs := testSentinel()

	for name, sts := range map[string]*appsv1.StatefulSet{
		"redis":    BuildRedisStatefulSet(rs),
		"sentinel": BuildSentinelStatefulSet(rs),
	} {
		sc := sts.Spec.Template.Spec.SecurityContext
		if sc == nil {
			t.Errorf("%s: pod securityContext is nil", name)
			continue
		}
		if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			t.Errorf("%s: runAsNonRoot = %v, want true", name, sc.RunAsNonRoot)
		}
		if sc.RunAsUser == nil || *sc.RunAsUser != 1000 {
			t.Errorf("%s: runAsUser = %v, want 1000", name, sc.RunAsUser)
		}
		if sc.FSGroup == nil || *sc.FSGroup != 1000 {
			t.Errorf("%s: fsGroup = %v, want 1000", name, sc.FSGroup)
		}
	}
}

// TestManagedPodsMeetRestrictedPSS pins every field the restricted Pod Security
// Standard checks on a pod the operator builds. A namespace enforcing
// "restricted" rejects the StatefulSet's pods outright if any of them is
// missing, and the cluster then never forms.
func TestManagedPodsMeetRestrictedPSS(t *testing.T) {
	rs := testSentinel()

	for name, sts := range map[string]*appsv1.StatefulSet{
		"redis":    BuildRedisStatefulSet(rs),
		"sentinel": BuildSentinelStatefulSet(rs),
	} {
		spec := sts.Spec.Template.Spec

		psc := spec.SecurityContext
		if psc == nil {
			t.Errorf("%s: pod securityContext is nil", name)
			continue
		}
		if psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
			t.Errorf("%s: pod runAsNonRoot = %v, want true", name, psc.RunAsNonRoot)
		}
		if psc.RunAsUser == nil || *psc.RunAsUser == 0 {
			t.Errorf("%s: pod runAsUser = %v, want a non-zero uid", name, psc.RunAsUser)
		}
		if psc.RunAsGroup == nil || *psc.RunAsGroup == 0 {
			t.Errorf("%s: pod runAsGroup = %v, want a non-zero gid", name, psc.RunAsGroup)
		}
		if psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("%s: pod seccompProfile = %+v, want type RuntimeDefault", name, psc.SeccompProfile)
		}
		if spec.HostNetwork || spec.HostPID || spec.HostIPC {
			t.Errorf("%s: pod shares a host namespace", name)
		}

		if len(spec.Containers) == 0 {
			t.Errorf("%s: pod has no containers", name)
			continue
		}
		for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
			sc := c.SecurityContext
			if sc == nil {
				t.Errorf("%s/%s: container securityContext is nil", name, c.Name)
				continue
			}
			if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
				t.Errorf("%s/%s: allowPrivilegeEscalation = %v, want false", name, c.Name, sc.AllowPrivilegeEscalation)
			}
			if sc.Privileged != nil && *sc.Privileged {
				t.Errorf("%s/%s: privileged = true", name, c.Name)
			}
			if sc.Capabilities == nil || !slices.Contains(sc.Capabilities.Drop, corev1.Capability("ALL")) {
				t.Errorf("%s/%s: capabilities = %+v, want drop [ALL]", name, c.Name, sc.Capabilities)
			}
			if sc.Capabilities != nil && len(sc.Capabilities.Add) > 0 {
				t.Errorf("%s/%s: capabilities.add = %v, want none", name, c.Name, sc.Capabilities.Add)
			}
			if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
				t.Errorf("%s/%s: readOnlyRootFilesystem = %v, want true", name, c.Name, sc.ReadOnlyRootFilesystem)
			}
		}

		for _, v := range spec.Volumes {
			if v.HostPath != nil {
				t.Errorf("%s: volume %s is a hostPath, which restricted forbids", name, v.Name)
			}
		}
	}
}

// TestManagedPodsMountEveryWritablePath is the other half of
// readOnlyRootFilesystem: the init scripts and the servers write to /data, and
// /tmp is the only path outside it that busybox and redis-cli fall back to, so
// both have to be real mounts or the container cannot start.
func TestManagedPodsMountEveryWritablePath(t *testing.T) {
	rs := testSentinel()

	for _, tc := range []struct {
		name       string
		sts        *appsv1.StatefulSet
		dataVolume string
	}{
		{"redis", BuildRedisStatefulSet(rs), "data"},
		{"sentinel", BuildSentinelStatefulSet(rs), "sentinel-data"},
	} {
		spec := tc.sts.Spec.Template.Spec
		c := spec.Containers[0]

		if !hasVolumeMount(c, tc.dataVolume, "/data") {
			t.Errorf("%s: /data is not backed by volume %q: %v", tc.name, tc.dataVolume, c.VolumeMounts)
		}
		if !hasVolumeMount(c, "tmp", "/tmp") {
			t.Errorf("%s: /tmp is not mounted, so a read-only root leaves no scratch path: %v",
				tc.name, c.VolumeMounts)
		}
		v, ok := findVolume(spec.Volumes, "tmp")
		if !ok {
			t.Errorf("%s: no tmp volume: %v", tc.name, spec.Volumes)
			continue
		}
		if v.EmptyDir == nil {
			t.Errorf("%s: tmp volume = %+v, want an emptyDir", tc.name, v.VolumeSource)
		}
	}
}

// TestManagedPodsMountSecretsReadableByRuntimeUser guards the interaction
// between the security context and the mounted credentials: the pod runs as a
// uid that owns nothing in the image, so every secret file has to be reachable
// through fsGroup. A mode without the group bit makes redis-server exit on the
// unreadable key.
func TestManagedPodsMountSecretsReadableByRuntimeUser(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Auth = &redisv1alpha1.AuthConfig{SecretName: "redis-pass"}
	rs.Spec.TLS = &redisv1alpha1.TLSConfig{
		Enabled:              true,
		CertificateSecretRef: "redis-tls",
		CASecretRef:          "redis-ca",
	}

	for name, sts := range map[string]*appsv1.StatefulSet{
		"redis":    BuildRedisStatefulSet(rs),
		"sentinel": BuildSentinelStatefulSet(rs),
	} {
		spec := sts.Spec.Template.Spec
		psc := spec.SecurityContext
		if psc == nil || psc.FSGroup == nil {
			t.Errorf("%s: fsGroup is unset, so mounted secrets stay owned by root", name)
			continue
		}
		gid := *psc.FSGroup
		if psc.RunAsGroup != nil && *psc.RunAsGroup != gid {
			t.Errorf("%s: runAsGroup = %d and fsGroup = %d; mounted files are group-owned by fsGroup",
				name, *psc.RunAsGroup, gid)
		}

		for _, v := range spec.Volumes {
			switch {
			case v.Secret != nil:
				if v.Secret.DefaultMode == nil {
					t.Errorf("%s: secret volume %s has no defaultMode; 0644 leaks the key to every uid",
						name, v.Name)
					continue
				}
				if *v.Secret.DefaultMode&0o040 == 0 {
					t.Errorf("%s: secret volume %s mode %#o is not group-readable, so uid %d cannot read it",
						name, v.Name, *v.Secret.DefaultMode, *psc.RunAsUser)
				}
				if *v.Secret.DefaultMode&0o004 != 0 {
					t.Errorf("%s: secret volume %s mode %#o is world-readable", name, v.Name, *v.Secret.DefaultMode)
				}
			case v.Projected != nil:
				if v.Projected.DefaultMode == nil {
					t.Errorf("%s: projected volume %s has no defaultMode; 0644 leaks the key to every uid",
						name, v.Name)
					continue
				}
				if *v.Projected.DefaultMode&0o040 == 0 {
					t.Errorf("%s: projected volume %s mode %#o is not group-readable, so uid %d cannot read it",
						name, v.Name, *v.Projected.DefaultMode, *psc.RunAsUser)
				}
				if *v.Projected.DefaultMode&0o004 != 0 {
					t.Errorf("%s: projected volume %s mode %#o is world-readable", name, v.Name, *v.Projected.DefaultMode)
				}
			case v.ConfigMap != nil:
				if v.ConfigMap.DefaultMode == nil || *v.ConfigMap.DefaultMode&0o050 != 0o050 {
					t.Errorf("%s: configMap volume %s mode %v is not group-executable, so the init script cannot run",
						name, v.Name, v.ConfigMap.DefaultMode)
				}
			}
		}

		// The password reaches the process through the environment, never
		// through a file, so no mode governs it.
		c := spec.Containers[0]
		var fromSecret bool
		for _, e := range c.Env {
			if e.Name == "REDIS_PASSWORD" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				fromSecret = true
			}
		}
		if !fromSecret {
			t.Errorf("%s: REDIS_PASSWORD is not projected from the auth secret: %v", name, c.Env)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func findVolume(vols []corev1.Volume, name string) (corev1.Volume, bool) {
	for _, v := range vols {
		if v.Name == name {
			return v, true
		}
	}
	return corev1.Volume{}, false
}

func hasVolumeMount(c corev1.Container, name, path string) bool {
	for _, m := range c.VolumeMounts {
		if m.Name == name && m.MountPath == path {
			return true
		}
	}
	return false
}
