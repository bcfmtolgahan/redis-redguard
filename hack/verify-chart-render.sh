#!/usr/bin/env bash
# Assert the runtime flags and pod settings the operator depends on survive a
# chart render. Needs helm, no cluster.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART="${1:-$REPO_ROOT/charts/redguard}"
rc=0

render() {
	helm template redguard "$CHART" --show-only templates/deployment.yaml "$@" 2>&1
}

fail() {
	echo "verify-chart-render: $1"
	rc=1
}

default=$(render)
if [ $? -ne 0 ]; then
	echo "verify-chart-render: default render failed:"
	echo "$default"
	exit 1
fi

# Two managers reconciling the same clusters fight over master role labels and
# StatefulSet updates, so operator.replicas is only safe behind a leader lease.
case "$default" in
*--leader-elect*) ;;
*) fail "the rendered deployment does not pass --leader-elect" ;;
esac

# A reconcile can block on failover convergence and the leader lease is handed
# back during shutdown; the kubelet must not SIGKILL before either finishes.
grace=$(printf '%s\n' "$default" | sed -n 's/^[[:space:]]*terminationGracePeriodSeconds:[[:space:]]*\([0-9]*\).*/\1/p' | head -1)
if [ -z "$grace" ]; then
	fail "the rendered deployment sets no terminationGracePeriodSeconds"
elif [ "$grace" -lt 60 ]; then
	fail "terminationGracePeriodSeconds is ${grace}, below the 60s a blocked reconcile plus lease release needs"
fi

# Empty means all namespaces, which is what the flag's absence already means.
case "$default" in
*--watch-namespace*) fail "an empty operator.watchNamespaces must not render --watch-namespace" ;;
esac

# --zap-devel is debug-level console logging with stacktraces on warnings.
case "$default" in
*--zap-devel*) fail "the default render enables development logging" ;;
esac

devel=$(render --set operator.devLogging=true)
case "$devel" in
*--zap-devel*) ;;
*) fail "operator.devLogging does not reach the manager as --zap-devel" ;;
esac

# Turning leader election off is only legitimate for a single replica, and the
# rendered args have to actually follow the value.
single=$(render --set operator.leaderElect=false)
case "$single" in
*--leader-elect*) fail "operator.leaderElect=false still renders --leader-elect" ;;
esac

scoped=$(render --set 'operator.watchNamespaces={team-a,team-b}')
case "$scoped" in
*--watch-namespace=team-a,team-b*) ;;
*) fail "operator.watchNamespaces does not reach the manager as --watch-namespace" ;;
esac

# Scaling up with leader election off is the failure this guard exists for, so
# the render has to refuse it rather than ship two active managers.
if render --set operator.replicas=2 --set operator.leaderElect=false >/dev/null 2>&1; then
	fail "replicas=2 with leaderElect=false rendered instead of failing"
fi

if [ $rc -ne 0 ]; then
	echo "verify-chart-render: FAILED"
fi
exit $rc
