package builder

import (
	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
	}

	// Build probe command - with or without auth
	probeCommand := buildRedisProbeCommand(rs.Spec.RedisConfig.Auth != nil && rs.Spec.RedisConfig.Auth.SecretName != "")

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
					DefaultMode: int32Ptr(0400),
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
						DefaultMode: int32Ptr(0400),
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

	// Update probe command for TLS if enabled
	if tlsEnabled {
		probeCommand = buildRedisProbeCommandWithTLS(rs.Spec.RedisConfig.Auth != nil && rs.Spec.RedisConfig.Auth.SecretName != "")
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
					SecurityContext: &corev1.PodSecurityContext{
						FSGroup:      int64Ptr(1000),
						RunAsUser:    int64Ptr(1000),
						RunAsNonRoot: boolPtr(true),
					},
					Containers: []corev1.Container{
						{
							Name:  "redis",
							Image: image,
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
										Command: probeCommand,
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
										Command: probeCommand,
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

	// Build probe command - Sentinel probe (Sentinel itself doesn't require auth to PING)
	var sentinelProbeCommand []string
	if tlsEnabled {
		sentinelProbeCommand = buildSentinelProbeCommandWithTLS(false)
	} else {
		sentinelProbeCommand = buildSentinelProbeCommand(false)
	}

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
	}

	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "config",
			MountPath: "/etc/sentinel",
		},
	}

	// Add TLS volumes and mounts if enabled
	if tlsEnabled && rs.Spec.TLS.CertificateSecretRef != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "tls-certs",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  rs.Spec.TLS.CertificateSecretRef,
					DefaultMode: int32Ptr(0400),
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
						DefaultMode: int32Ptr(0400),
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

	return &appsv1.StatefulSet{
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
					SecurityContext: &corev1.PodSecurityContext{
						FSGroup:      int64Ptr(1000),
						RunAsUser:    int64Ptr(1000),
						RunAsNonRoot: boolPtr(true),
					},
					Containers: []corev1.Container{
						{
							Name:  "sentinel",
							Image: "redis:7-alpine",
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
		},
	}
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

// buildRedisProbeCommand creates the probe command for Redis with optional auth
func buildRedisProbeCommand(authEnabled bool) []string {
	if authEnabled {
		// Use REDISCLI_AUTH environment variable to avoid password in command line
		return []string{
			"sh",
			"-c",
			"REDISCLI_AUTH=$REDIS_PASSWORD redis-cli -h $(hostname) ping | grep -q PONG",
		}
	}
	return []string{
		"sh",
		"-c",
		"redis-cli -h $(hostname) ping | grep -q PONG",
	}
}

// buildSentinelProbeCommand creates the probe command for Sentinel with optional auth
func buildSentinelProbeCommand(authEnabled bool) []string {
	if authEnabled {
		return []string{
			"sh",
			"-c",
			"REDISCLI_AUTH=$REDIS_PASSWORD redis-cli -h $(hostname) -p 26379 ping | grep -q PONG",
		}
	}
	return []string{
		"sh",
		"-c",
		"redis-cli -h $(hostname) -p 26379 ping | grep -q PONG",
	}
}

// buildRedisProbeCommandWithTLS creates the probe command for Redis with TLS
func buildRedisProbeCommandWithTLS(authEnabled bool) []string {
	if authEnabled {
		return []string{
			"sh",
			"-c",
			"REDISCLI_AUTH=$REDIS_PASSWORD redis-cli -h $(hostname) --tls --cert /etc/redis/tls/tls.crt --key /etc/redis/tls/tls.key --cacert /etc/redis/tls/ca.crt ping | grep -q PONG",
		}
	}
	return []string{
		"sh",
		"-c",
		"redis-cli -h $(hostname) --tls --cert /etc/redis/tls/tls.crt --key /etc/redis/tls/tls.key --cacert /etc/redis/tls/ca.crt ping | grep -q PONG",
	}
}

// buildSentinelProbeCommandWithTLS creates the probe command for Sentinel with TLS
func buildSentinelProbeCommandWithTLS(authEnabled bool) []string {
	if authEnabled {
		return []string{
			"sh",
			"-c",
			"REDISCLI_AUTH=$REDIS_PASSWORD redis-cli -h $(hostname) -p 26379 --tls --cert /etc/sentinel/tls/tls.crt --key /etc/sentinel/tls/tls.key --cacert /etc/sentinel/tls/ca.crt ping | grep -q PONG",
		}
	}
	return []string{
		"sh",
		"-c",
		"redis-cli -h $(hostname) -p 26379 --tls --cert /etc/sentinel/tls/tls.crt --key /etc/sentinel/tls/tls.key --cacert /etc/sentinel/tls/ca.crt ping | grep -q PONG",
	}
}
