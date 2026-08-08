// Package clusteraccess resolves how the operator reaches a RedisSentinel
// cluster: the sentinel addresses to dial and the password the cluster
// currently accepts. It is shared by the controllers and the sentinel event
// watcher so both sides dial with the same view; a second resolution path is
// how one of them ends up presenting stale credentials.
package clusteraccess

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1alpha1 "github.com/bcfmtolgahan/redis-redguard/api/v1alpha1"
)

// appliedPasswordKey is the key inside the applied-credentials Secret holding
// the password every node currently accepts.
const appliedPasswordKey = "password"

// SentinelAddresses is the stable per-pod DNS of every Sentinel in the set.
func SentinelAddresses(rs *redisv1alpha1.RedisSentinel) []string {
	addrs := make([]string, rs.Spec.SentinelConfig.Replicas)
	for i := int32(0); i < rs.Spec.SentinelConfig.Replicas; i++ {
		addrs[i] = fmt.Sprintf("%s-sentinel-%d.%s-sentinel-headless.%s.svc.cluster.local:26379",
			rs.Name, i, rs.Name, rs.Namespace)
	}
	return addrs
}

// MasterName is the name the sentinels monitor this cluster's master under.
func MasterName(rs *redisv1alpha1.RedisSentinel) string {
	return rs.Name + "-master"
}

// AppliedAuthSecretName is the operator-owned Secret recording the password
// the cluster currently accepts.
func AppliedAuthSecretName(rs *redisv1alpha1.RedisSentinel) string {
	return rs.Name + "-auth-state"
}

// AdminPassword retrieves the password the cluster currently accepts. The
// applied-credentials Secret is authoritative once it exists: during a pending
// rotation the user Secret already holds the new password while every node
// still requires the previous one, and the operator must keep authenticating
// throughout.
func AdminPassword(ctx context.Context, c client.Reader, rs *redisv1alpha1.RedisSentinel) string {
	if rs.Spec.RedisConfig.Auth == nil || rs.Spec.RedisConfig.Auth.SecretName == "" {
		return ""
	}

	applied := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{
		Name:      AppliedAuthSecretName(rs),
		Namespace: rs.Namespace,
	}, applied); err == nil {
		if password, ok := applied.Data[appliedPasswordKey]; ok {
			return string(password)
		}
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{
		Name:      rs.Spec.RedisConfig.Auth.SecretName,
		Namespace: rs.Namespace,
	}, secret); err != nil {
		return ""
	}

	return string(secret.Data["password"])
}
