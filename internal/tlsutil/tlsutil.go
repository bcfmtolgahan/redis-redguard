// Package tlsutil builds the client-side *tls.Config the controllers need to
// reach a RedisSentinel cluster whose spec enables TLS. With tls.enabled the
// pods listen on TLS only, so a controller that dials plaintext is blind;
// errors here must surface instead of degrading to a plaintext connection.
package tlsutil

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1alpha1 "github.com/bcfmtolgahan/redis-redguard/api/v1alpha1"
)

// Key names inside the referenced Secrets, matching what the statefulset
// builder mounts and what redis.conf points at.
const (
	certKey = "tls.crt"
	keyKey  = "tls.key"
	caKey   = "ca.crt"
)

// BuildClientTLSConfig returns the TLS settings for every Redis and Sentinel
// connection made on behalf of rs. It returns (nil, nil) when TLS is disabled
// so callers can pass the result straight to the client factory. When TLS is
// enabled, a missing Secret or key is an error. ServerName is left empty
// (callers dial pod IPs; crypto/tls fills it from the dialed address) and
// InsecureSkipVerify is never set: without a CA reference the returned config
// has no RootCAs, so the system trust store verifies the server.
func BuildClientTLSConfig(ctx context.Context, c client.Client, rs *redisv1alpha1.RedisSentinel) (*tls.Config, error) {
	spec := rs.Spec.TLS
	if spec == nil || !spec.Enabled {
		return nil, nil
	}
	if spec.CertificateSecretRef == "" {
		return nil, fmt.Errorf("spec.tls.enabled is true but spec.tls.certificateSecretRef is empty")
	}

	certSecret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: spec.CertificateSecretRef, Namespace: rs.Namespace}, certSecret); err != nil {
		return nil, fmt.Errorf("get TLS certificate secret %q: %w", spec.CertificateSecretRef, err)
	}
	certPEM, ok := certSecret.Data[certKey]
	if !ok {
		return nil, fmt.Errorf("TLS certificate secret %q has no %q key", spec.CertificateSecretRef, certKey)
	}
	keyPEM, ok := certSecret.Data[keyKey]
	if !ok {
		return nil, fmt.Errorf("TLS certificate secret %q has no %q key", spec.CertificateSecretRef, keyKey)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse key pair from secret %q: %w", spec.CertificateSecretRef, err)
	}

	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}

	if spec.CASecretRef != "" {
		caSecret := certSecret
		if spec.CASecretRef != spec.CertificateSecretRef {
			caSecret = &corev1.Secret{}
			if err := c.Get(ctx, types.NamespacedName{Name: spec.CASecretRef, Namespace: rs.Namespace}, caSecret); err != nil {
				return nil, fmt.Errorf("get TLS CA secret %q: %w", spec.CASecretRef, err)
			}
		}
		caPEM, ok := caSecret.Data[caKey]
		if !ok {
			return nil, fmt.Errorf("TLS CA secret %q has no %q key", spec.CASecretRef, caKey)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("TLS CA secret %q: %q contains no valid certificate", spec.CASecretRef, caKey)
		}
		cfg.RootCAs = pool
	}

	return cfg, nil
}
