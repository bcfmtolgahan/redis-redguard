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
	"flag"
	"io"
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// typeName is the element type behind a client.Object cache key, e.g. "Pod".
func typeName(obj client.Object) string {
	return reflect.TypeOf(obj).Elem().Name()
}

func TestSplitCommaList(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{in: "", want: nil},
		{in: "  ", want: nil},
		{in: "one", want: []string{"one"}},
		{in: "one, two ,,three", want: []string{"one", "two", "three"}},
	} {
		if got := splitCommaList(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("splitCommaList(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestCacheDefaultsToEveryNamespace keeps the flag's empty value meaning
// cluster-wide; an accidental empty-string entry would watch the "" namespace
// and the operator would see nothing at all.
func TestCacheDefaultsToEveryNamespace(t *testing.T) {
	if ns := cacheOptions(nil).DefaultNamespaces; len(ns) != 0 {
		t.Errorf("empty --watch-namespace must leave DefaultNamespaces unset, got %v", ns)
	}
}

// TestCacheIsScopedToTheWatchedNamespaces is what keeps the operator's
// informers off every namespace of a large cluster.
func TestCacheIsScopedToTheWatchedNamespaces(t *testing.T) {
	opts := cacheOptions(splitCommaList("team-a, team-b"))

	if len(opts.DefaultNamespaces) != 2 {
		t.Fatalf("want 2 watched namespaces, got %v", opts.DefaultNamespaces)
	}
	for _, ns := range []string{"team-a", "team-b"} {
		if _, ok := opts.DefaultNamespaces[ns]; !ok {
			t.Errorf("namespace %q is not cached: %v", ns, opts.DefaultNamespaces)
		}
	}
}

// TestOperatorOwnedTypesAreLabelScoped covers the types the operator only ever
// reads back from itself. Without a selector each one is a cluster-wide
// informer over every Pod, ConfigMap and Service in the cluster.
func TestOperatorOwnedTypesAreLabelScoped(t *testing.T) {
	want := []string{"Pod", "ConfigMap", "Service", "StatefulSet", "NetworkPolicy", "PodDisruptionBudget"}

	byName := map[string]labels.Selector{}
	for obj, byObject := range cacheOptions(nil).ByObject {
		byName[typeName(obj)] = byObject.Label
	}

	for _, kind := range want {
		selector, ok := byName[kind]
		if !ok {
			t.Errorf("%s has no ByObject entry, so its informer covers the whole cluster", kind)
			continue
		}
		if selector == nil || selector.Empty() {
			t.Errorf("%s is cached without a label selector", kind)
			continue
		}
		if selector.Matches(labels.Set{}) {
			t.Errorf("%s selector %q matches unlabelled objects", kind, selector)
		}
		if !selector.Matches(labels.Set{"app.kubernetes.io/managed-by": "redguard-operator"}) {
			t.Errorf("%s selector %q does not match the operator's own objects", kind, selector)
		}
	}
}

// TestSecretsAreNotLabelScoped pins the one type that must stay unfiltered.
// The auth password, the TLS certificates and the S3 credentials are created by
// the user and carry no operator label; a selector on Secrets would hide them
// from the cache and every cluster would come up unauthenticated or fail TLS.
func TestSecretsAreNotLabelScoped(t *testing.T) {
	opts := cacheOptions(nil)

	if opts.DefaultLabelSelector != nil {
		t.Fatalf("a default label selector applies to Secrets too: %v", opts.DefaultLabelSelector)
	}
	for obj, byObject := range opts.ByObject {
		if typeName(obj) != typeName(&corev1.Secret{}) {
			continue
		}
		if byObject.Label != nil && !byObject.Label.Empty() {
			t.Fatalf("Secrets are label-scoped by %q; user-created secrets carry no operator label", byObject.Label)
		}
	}
}

// TestManagerOptionsReleaseTheLeaderLease keeps failover between replicas at
// the length of a shutdown rather than the full lease duration.
func TestManagerOptionsReleaseTheLeaderLease(t *testing.T) {
	opts := managerOptions(managerConfig{leaderElect: true})

	if !opts.LeaderElectionReleaseOnCancel {
		t.Error("LeaderElectionReleaseOnCancel is off: a restarting leader parks the lease for its full duration")
	}
	if !opts.LeaderElection {
		t.Error("--leader-elect did not reach the manager")
	}
	if opts.LeaderElectionID == "" {
		t.Error("LeaderElectionID is empty")
	}
}

// TestLoggingDefaultsToProduction keeps debug-level console logging with
// stacktraces on warnings behind an explicit flag.
func TestLoggingDefaultsToProduction(t *testing.T) {
	parse := func(args ...string) bool {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		opts := zapOptions(fs)
		if err := fs.Parse(args); err != nil {
			t.Fatalf("parse %v: %v", args, err)
		}
		return opts.Development
	}

	if parse() {
		t.Error("development logging is on by default")
	}
	if !parse("--zap-devel") {
		t.Error("--zap-devel does not turn development logging on")
	}
}
