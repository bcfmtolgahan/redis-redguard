/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"crypto/tls"
	"flag"
	"os"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	"github.com/redguard/redguard/internal/controller"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")

	// Set at link time with -X main.version. Logged at startup so a running pod
	// can be identified without inspecting the image it came from.
	version = "dev"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(redisv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// splitCommaList parses a comma-separated flag value, dropping empty entries.
func splitCommaList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// managedByOperator selects the objects the operator itself creates; every one
// of them is built with this label.
var managedByOperator = labels.SelectorFromSet(labels.Set{
	"app.kubernetes.io/managed-by": "redguard-operator",
})

// cacheOptions scopes the manager's informers. An unscoped manager keeps a
// cluster-wide copy of every Pod, ConfigMap, Service and StatefulSet, which is
// what makes the operator run out of memory on a large cluster.
//
// watchNamespaces empty means every namespace. Types the operator only ever
// reads back from itself are additionally filtered by label. Secrets are not:
// the auth password, the TLS certificates and the S3 credentials belong to the
// user and carry no operator label, so filtering them would hide them from
// every read and from the watch that drives credential rotation.
func cacheOptions(watchNamespaces []string) cache.Options {
	opts := cache.Options{
		// Server-side field ownership is a large share of a cached object and
		// nothing here reads it.
		DefaultTransform: cache.TransformStripManagedFields(),
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}:                   {Label: managedByOperator},
			&corev1.ConfigMap{}:             {Label: managedByOperator},
			&corev1.Service{}:               {Label: managedByOperator},
			&appsv1.StatefulSet{}:           {Label: managedByOperator},
			&networkingv1.NetworkPolicy{}:   {Label: managedByOperator},
			&policyv1.PodDisruptionBudget{}: {Label: managedByOperator},
		},
	}

	if len(watchNamespaces) > 0 {
		opts.DefaultNamespaces = make(map[string]cache.Config, len(watchNamespaces))
		for _, ns := range watchNamespaces {
			opts.DefaultNamespaces[ns] = cache.Config{}
		}
	}
	return opts
}

// managerConfig carries the parsed flags the manager itself needs.
type managerConfig struct {
	metrics         metricsserver.Options
	webhookServer   webhook.Server
	probeAddr       string
	leaderElect     bool
	watchNamespaces []string
}

func managerOptions(cfg managerConfig) ctrl.Options {
	return ctrl.Options{
		Scheme:                 scheme,
		Metrics:                cfg.metrics,
		WebhookServer:          cfg.webhookServer,
		HealthProbeBindAddress: cfg.probeAddr,
		LeaderElection:         cfg.leaderElect,
		LeaderElectionID:       "f4acadcf.redguard.io",
		// Nothing runs after mgr.Start returns, so stepping down is safe here
		// and the successor takes over in seconds instead of waiting out the
		// full lease.
		LeaderElectionReleaseOnCancel: true,
		Cache:                         cacheOptions(cfg.watchNamespaces),
	}
}

// zapOptions binds the logging flags to fs. The zero value is production
// logging: JSON, info level, stacktraces only from error upwards. --zap-devel
// is the opt-in to console output at debug level.
func zapOptions(fs *flag.FlagSet) *zap.Options {
	opts := &zap.Options{}
	opts.BindFlags(fs)
	return opts
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var allowedBackupBuckets string
	var allowedBackupEndpoints string
	var watchNamespace string
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&allowedBackupBuckets, "allowed-backup-buckets", "",
		"Comma-separated S3 buckets a RedisBackup or RedisRestore may target with the operator's "+
			"IAM role (s3.useIAMRole). Empty disables the IAM-role path entirely; the CRs must then "+
			"supply s3.credentialsSecretRef. CRs using credentialsSecretRef are not restricted.")
	flag.StringVar(&allowedBackupEndpoints, "allowed-backup-endpoints", "",
		"Comma-separated custom S3 endpoints permitted for IAM-role backups and restores, for "+
			"example a VPC endpoint URL. Empty permits only the default AWS endpoint.")
	flag.StringVar(&watchNamespace, "watch-namespace", "",
		"Comma-separated namespaces the operator watches and acts on. Empty watches every "+
			"namespace, which means an informer over the whole cluster; set this on a large "+
			"cluster or when the operator should not see other tenants.")
	opts := zapOptions(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(opts)))

	setupLog.Info("starting redguard", "version", version)

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	watchNamespaces := splitCommaList(watchNamespace)
	if len(watchNamespaces) == 0 {
		setupLog.Info("watching every namespace")
	} else {
		setupLog.Info("watching selected namespaces", "namespaces", watchNamespaces)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), managerOptions(managerConfig{
		metrics:         metricsServerOptions,
		webhookServer:   webhookServer,
		probeAddr:       probeAddr,
		leaderElect:     enableLeaderElection,
		watchNamespaces: watchNamespaces,
	}))
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.RedisSentinelReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("redissentinel-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RedisSentinel")
		os.Exit(1)
	}
	if err := (&controller.RedisUserReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RedisUser")
		os.Exit(1)
	}
	if err := (&controller.RedisBackupReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		RESTConfig:       mgr.GetConfig(),
		Recorder:         mgr.GetEventRecorderFor("redisbackup-controller"),
		AllowedBuckets:   splitCommaList(allowedBackupBuckets),
		AllowedEndpoints: splitCommaList(allowedBackupEndpoints),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RedisBackup")
		os.Exit(1)
	}
	if err := (&controller.RedisRestoreReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		RESTConfig: mgr.GetConfig(),
		Recorder:   mgr.GetEventRecorderFor("redisrestore-controller"),
		// The same destination policy as the backup side: a restore under the
		// operator's IAM role reads any object that identity can reach, so it
		// must pass the identical allowlists.
		AllowedBuckets:   splitCommaList(allowedBackupBuckets),
		AllowedEndpoints: splitCommaList(allowedBackupEndpoints),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RedisRestore")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
