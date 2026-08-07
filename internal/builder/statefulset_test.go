package builder

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

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
		StorageClassName: "gp3",
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

func TestBuildRedisStatefulSet_TLSEnabled(t *testing.T) {
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
	if vol.Secret == nil || vol.Secret.SecretName != "redis-tls" {
		t.Errorf("tls-certs volume secret = %+v, want secretName redis-tls", vol.Secret)
	}
	// 0440 with fsGroup 1000: the secret files are root:1000, and the redis
	// process (uid 1000, supplementary gid 1000) reads via the group bit.
	// 0400 leaves the key readable by root only and TLS never comes up.
	if vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != 0440 {
		t.Errorf("tls-certs defaultMode = %v, want 0440 so the non-root redis user can read the key", vol.Secret.DefaultMode)
	}
	if !hasVolumeMount(c, "tls-certs", "/etc/redis/tls") {
		t.Errorf("tls-certs not mounted at /etc/redis/tls: %v", c.VolumeMounts)
	}

	caVol, ok := findVolume(sts.Spec.Template.Spec.Volumes, "tls-ca")
	if !ok {
		t.Fatalf("CASecretRef set but no tls-ca volume, got %v", sts.Spec.Template.Spec.Volumes)
	}
	if caVol.Secret == nil || caVol.Secret.SecretName != "redis-ca" {
		t.Errorf("tls-ca volume secret = %+v, want secretName redis-ca", caVol.Secret)
	}
	if caVol.Secret.DefaultMode == nil || *caVol.Secret.DefaultMode != 0440 {
		t.Errorf("tls-ca defaultMode = %v, want 0440 so the non-root redis user can read it", caVol.Secret.DefaultMode)
	}
	if !hasVolumeMount(c, "tls-ca", "/etc/redis/tls/ca.crt") {
		t.Errorf("tls-ca not mounted at /etc/redis/tls/ca.crt: %v", c.VolumeMounts)
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

// When the CA lives in the same Secret as the cert, no second volume is needed.
func TestBuildRedisStatefulSet_TLSSharedCASecretHasNoSecondVolume(t *testing.T) {
	rs := testSentinel()
	rs.Spec.TLS = &redisv1alpha1.TLSConfig{
		Enabled:              true,
		CertificateSecretRef: "redis-tls",
		CASecretRef:          "redis-tls",
	}

	sts := BuildRedisStatefulSet(rs)

	if _, ok := findVolume(sts.Spec.Template.Spec.Volumes, "tls-ca"); ok {
		t.Error("CA shares the cert Secret but a separate tls-ca volume was created")
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

func TestBuildSentinelStatefulSet_StorageClassFollowsRedis(t *testing.T) {
	rs := testSentinel()
	rs.Spec.RedisConfig.Storage = &redisv1alpha1.StorageSpec{
		Size:             resource.MustParse("10Gi"),
		StorageClassName: "gp3",
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
	if vol, ok := findVolume(sts.Spec.Template.Spec.Volumes, "tls-certs"); !ok || vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != 0440 {
		t.Errorf("tls-certs defaultMode = %+v, want 0440 so the non-root redis user can read the key", vol.Secret)
	}
	if !hasVolumeMount(c, "tls-ca", "/etc/sentinel/tls/ca.crt") {
		t.Errorf("tls-ca not mounted at /etc/sentinel/tls/ca.crt: %v", c.VolumeMounts)
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
