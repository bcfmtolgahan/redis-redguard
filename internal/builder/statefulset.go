package builder

import (
	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// managedRunAsID is the uid and gid the managed Redis and Sentinel processes
// run as. It is deliberately not the redis user baked into the image: nothing
// the pod touches is owned by that uid either, and fsGroup is what grants
// access to the data volume and the mounted TLS material.
const managedRunAsID int64 = 1000

// managedPodSecurityContext is the pod-level half of the restricted Pod
// Security Standard. fsGroup also sets the group owner of every mounted volume,
// which is how a uid that owns nothing in the image reads the PVC and the TLS
// secrets.
func managedPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot: boolPtr(true),
		RunAsUser:    int64Ptr(managedRunAsID),
		RunAsGroup:   int64Ptr(managedRunAsID),
		FSGroup:      int64Ptr(managedRunAsID),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// managedContainerSecurityContext is the container-level half. The root
// filesystem is read-only because both init scripts and both servers write only
// under /data; /tmp is supplied separately as an emptyDir.
func managedContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(true),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

// tmpVolume backs /tmp, the one writable path outside /data that busybox and
// redis-cli can fall back to once the root filesystem is read-only.
func tmpVolume() corev1.Volume {
	return corev1.Volume{
		Name:         "tmp",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
}

// BuildRedisStatefulSet creates a StatefulSet for Redis servers
func BuildRedisStatefulSet(rs *redisv1alpha1.RedisSentinel) *appsv1.StatefulSet {
	labels := buildLabels(rs, "redis")

	// Default resource requests if not specified
	resources := rs.Spec.RedisConfig.Resources
	if resources.Requests == nil {
		resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		}
	}

	// Storage size
	storageSize := resource.MustParse("1Gi")
	if rs.Spec.RedisConfig.Storage != nil {
		storageSize = rs.Spec.RedisConfig.Storage.Size
	}

	// Image
	image := "redis:7-alpine"
	if rs.Spec.RedisConfig.Image != "" {
		image = rs.Spec.RedisConfig.Image
	}

	// Environment variables
	env := []corev1.EnvVar{}
	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "data",
			MountPath: "/data",
		},
		{
			Name:      "config",
			MountPath: "/etc/redis",
		},
		{
			Name:      "tmp",
			MountPath: "/tmp",
		},
	}
	volumes := []corev1.Volume{
		{
			Name: "config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: rs.Name + "-redis-config",
					},
					DefaultMode: int32Ptr(0755),
				},
			},
		},
		tmpVolume(),
	}

	probes := redisProbeCLI(rs)

	// Add auth if configured
	if rs.Spec.RedisConfig.Auth != nil && rs.Spec.RedisConfig.Auth.SecretName != "" {
		env = append(env, corev1.EnvVar{
			Name: "REDIS_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: rs.Spec.RedisConfig.Auth.SecretName,
					},
					Key: "password",
				},
			},
		})
	}

	// Add TLS volumes and mounts if enabled
	tlsEnabled := rs.Spec.TLS != nil && rs.Spec.TLS.Enabled
	if tlsEnabled && rs.Spec.TLS.CertificateSecretRef != "" {
		// TLS certificate volume
		volumes = append(volumes, corev1.Volume{
			Name: "tls-certs",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  rs.Spec.TLS.CertificateSecretRef,
					DefaultMode: int32Ptr(0440),
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "tls-certs",
			MountPath: "/etc/redis/tls",
			ReadOnly:  true,
		})

		// CA certificate volume if specified (separate from cert/key secret)
		if rs.Spec.TLS.CASecretRef != "" && rs.Spec.TLS.CASecretRef != rs.Spec.TLS.CertificateSecretRef {
			volumes = append(volumes, corev1.Volume{
				Name: "tls-ca",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName:  rs.Spec.TLS.CASecretRef,
						DefaultMode: int32Ptr(0440),
					},
				},
			})
			// Mount CA certificate at /etc/redis/tls/ca.crt
			// This assumes the secret contains a key named "ca.crt"
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				Name:      "tls-ca",
				MountPath: "/etc/redis/tls/ca.crt",
				SubPath:   "ca.crt",
				ReadOnly:  true,
			})
		}
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rs.Name + "-redis",
			Namespace: rs.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: rs.Name + "-redis-headless",
			Replicas:    &rs.Spec.RedisConfig.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			PodManagementPolicy: appsv1.ParallelPodManagement,
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					SecurityContext: managedPodSecurityContext(),
					Containers: []corev1.Container{
						{
							Name:            "redis",
							Image:           image,
							SecurityContext: managedContainerSecurityContext(),
							Command: []string{
								"/bin/sh",
								"-c",
								"/etc/redis/init.sh",
							},
							Env: env,
							Ports: []corev1.ContainerPort{
								{
									Name:          "redis",
									ContainerPort: 6379,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							Resources: resources,
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: probes.livenessCommand(),
									},
								},
								InitialDelaySeconds: 15,
								PeriodSeconds:       10,
								TimeoutSeconds:      5,
								FailureThreshold:    3,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: probes.readinessCommand(),
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       5,
								TimeoutSeconds:      3,
								FailureThreshold:    3,
							},
							VolumeMounts: volumeMounts,
						},
					},
					Volumes: volumes,
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "data",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteOnce,
						},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: storageSize,
							},
						},
					},
				},
			},
		},
	}

	// Add storage class if specified
	if rs.Spec.RedisConfig.Storage != nil && rs.Spec.RedisConfig.Storage.StorageClassName != "" {
		sts.Spec.VolumeClaimTemplates[0].Spec.StorageClassName = &rs.Spec.RedisConfig.Storage.StorageClassName
	}

	return sts
}

// BuildSentinelStatefulSet creates a StatefulSet for Sentinel instances
func BuildSentinelStatefulSet(rs *redisv1alpha1.RedisSentinel) *appsv1.StatefulSet {
	labels := buildLabels(rs, "sentinel")

	// Default resource requests
	resources := rs.Spec.SentinelConfig.Resources
	if resources.Requests == nil {
		resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		}
	}

	// Check if auth is enabled
	authEnabled := rs.Spec.RedisConfig.Auth != nil && rs.Spec.RedisConfig.Auth.SecretName != ""

	// Check if TLS is enabled
	tlsEnabled := rs.Spec.TLS != nil && rs.Spec.TLS.Enabled

	// Sentinel has no replication link of its own, so readiness can only assert
	// that it answers on 26379; the master it monitors is asserted by the Redis
	// pods' own readiness.
	sentinelProbeCommand := sentinelProbeCLI(rs).livenessCommand()

	// Environment variables
	env := []corev1.EnvVar{}
	if authEnabled {
		env = append(env, corev1.EnvVar{
			Name: "REDIS_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: rs.Spec.RedisConfig.Auth.SecretName,
					},
					Key: "password",
				},
			},
		})
	}

	// Build volumes and volume mounts
	volumes := []corev1.Volume{
		{
			Name: "config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: rs.Name + "-sentinel-config",
					},
					DefaultMode: int32Ptr(0755),
				},
			},
		},
		tmpVolume(),
	}

	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "config",
			MountPath: "/etc/sentinel",
		},
		{
			Name:      "sentinel-data",
			MountPath: "/data",
		},
		{
			Name:      "tmp",
			MountPath: "/tmp",
		},
	}

	// Add TLS volumes and mounts if enabled
	if tlsEnabled && rs.Spec.TLS.CertificateSecretRef != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "tls-certs",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  rs.Spec.TLS.CertificateSecretRef,
					DefaultMode: int32Ptr(0440),
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "tls-certs",
			MountPath: "/etc/sentinel/tls",
			ReadOnly:  true,
		})

		// CA certificate volume if specified (separate from cert/key secret)
		if rs.Spec.TLS.CASecretRef != "" && rs.Spec.TLS.CASecretRef != rs.Spec.TLS.CertificateSecretRef {
			volumes = append(volumes, corev1.Volume{
				Name: "tls-ca",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName:  rs.Spec.TLS.CASecretRef,
						DefaultMode: int32Ptr(0440),
					},
				},
			})
			// Mount CA certificate at /etc/sentinel/tls/ca.crt
			// This assumes the secret contains a key named "ca.crt"
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				Name:      "tls-ca",
				MountPath: "/etc/sentinel/tls/ca.crt",
				SubPath:   "ca.crt",
				ReadOnly:  true,
			})
		}
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rs.Name + "-sentinel",
			Namespace: rs.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: rs.Name + "-sentinel-headless",
			Replicas:    &rs.Spec.SentinelConfig.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			PodManagementPolicy: appsv1.ParallelPodManagement,
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					SecurityContext: managedPodSecurityContext(),
					Containers: []corev1.Container{
						{
							Name:            "sentinel",
							Image:           "redis:7-alpine",
							SecurityContext: managedContainerSecurityContext(),
							Command: []string{
								"/bin/sh",
								"-c",
								"/etc/sentinel/init.sh",
							},
							Env: env,
							Ports: []corev1.ContainerPort{
								{
									Name:          "sentinel",
									ContainerPort: 26379,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							Resources: resources,
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: sentinelProbeCommand,
									},
								},
								InitialDelaySeconds: 15,
								PeriodSeconds:       10,
								TimeoutSeconds:      5,
								FailureThreshold:    3,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: sentinelProbeCommand,
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       5,
								TimeoutSeconds:      3,
								FailureThreshold:    3,
							},
							VolumeMounts: volumeMounts,
						},
					},
					Volumes: volumes,
				},
			},
			// Sentinel rewrites its config with the learned master, replicas
			// and failover epoch; without durable state a restarted pod would
			// re-seed from the template and re-monitor the bootstrap master.
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "sentinel-data",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteOnce,
						},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							},
						},
					},
				},
			},
		},
	}

	// The state is one small file; only the storage class follows Redis.
	if rs.Spec.RedisConfig.Storage != nil && rs.Spec.RedisConfig.Storage.StorageClassName != "" {
		sts.Spec.VolumeClaimTemplates[0].Spec.StorageClassName = &rs.Spec.RedisConfig.Storage.StorageClassName
	}

	return sts
}

// Helper functions
func int32Ptr(i int32) *int32 {
	return &i
}

func int64Ptr(i int64) *int64 {
	return &i
}

func boolPtr(b bool) *bool {
	return &b
}

// probeCLI describes how a probe reaches the server in its own container.
// A probe may issue more than one redis-cli call and every call has to carry
// the same flags: one without them answers NOAUTH or fails the TLS handshake,
// and the pod never becomes ready.
type probeCLI struct {
	// port is empty for the Redis default of 6379.
	port string
	// tlsDir is where the TLS material for this container is mounted.
	tlsDir string
	auth   bool
	tls    bool
	// cacert is false unless a CA secret is mounted. Pointing --cacert at a
	// file that is not there fails every probe; without it redis-cli verifies
	// against the system trust store.
	cacert bool
}

// call renders a single redis-cli invocation. The password travels in
// REDISCLI_AUTH because argv is readable by every process in the pod.
func (p probeCLI) call(args string) string {
	cmd := "redis-cli -h $(hostname)"
	if p.port != "" {
		cmd += " -p " + p.port
	}
	if p.tls {
		cmd += " --tls --cert " + p.tlsDir + "/tls.crt --key " + p.tlsDir + "/tls.key"
		if p.cacert {
			cmd += " --cacert " + p.tlsDir + "/ca.crt"
		}
	}
	if p.auth {
		// A bare assignment prefix is not field-split, so a password
		// containing spaces or globs survives unquoted.
		cmd = "REDISCLI_AUTH=$REDIS_PASSWORD " + cmd
	}
	return cmd + " " + args
}

// livenessCommand checks only that the server answers. It must stay blind to
// replication state: a replica whose link is down has to leave the Service, and
// restarting it would not reconnect it any faster.
func (p probeCLI) livenessCommand() []string {
	return []string{"sh", "-c", p.call("ping") + " | grep -q PONG"}
}

// readinessCommand additionally requires a replication state that can serve
// correct reads: a master, or a replica that finished its initial sync and
// still has the link up. A replica that only answers PONG returns stale or
// empty data for as long as it is in the Service endpoints.
func (p probeCLI) readinessCommand() []string {
	script := p.call("ping") + " | grep -q PONG || exit 1\n" +
		p.call("info replication") + " | grep -Eq '^(role:master|master_link_status:up)'\n"
	return []string{"sh", "-c", script}
}

// redisProbeCLI builds the invocation for the Redis container.
func redisProbeCLI(rs *redisv1alpha1.RedisSentinel) probeCLI {
	p := probeCLI{
		tlsDir: "/etc/redis/tls",
		auth:   rs.Spec.RedisConfig.Auth != nil && rs.Spec.RedisConfig.Auth.SecretName != "",
	}
	if rs.Spec.TLS != nil && rs.Spec.TLS.Enabled {
		p.tls = true
		p.cacert = rs.Spec.TLS.CASecretRef != ""
	}
	return p
}

// sentinelProbeCLI builds the invocation for the Sentinel container. Sentinel
// carries requirepass on 26379 whenever Redis auth is configured, so an
// unauthenticated PING answers NOAUTH and every probe would fail.
func sentinelProbeCLI(rs *redisv1alpha1.RedisSentinel) probeCLI {
	p := redisProbeCLI(rs)
	p.port = "26379"
	p.tlsDir = "/etc/sentinel/tls"
	return p
}
