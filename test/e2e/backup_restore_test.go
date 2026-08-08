//go:build e2e
// +build e2e

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

package e2e

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The round trip runs in a namespace of its own with a cluster of its own: a
// restore flushes and restarts the master it targets, which no other container
// in this suite may be exposed to whatever order Ginkgo runs them in.
const (
	backupNamespace   = "redguard-backup-e2e"
	backupClusterName = "redis-bk"

	bkRedisReplicas    = 2
	bkSentinelReplicas = 3

	// minioImage is pinned so a registry-side "latest" move cannot change what
	// this suite tested. The credentials are throwaway: they exist only inside
	// the per-run kind cluster.
	minioName      = "minio"
	minioImage     = "minio/minio:RELEASE.2025-09-07T16-13-09Z"
	minioAccessKey = "redguard-e2e"
	minioSecretKey = "redguard-e2e-secret"
	minioBucket    = "redguard-backups"

	// s3CredsSecret is the credentialsSecretRef path: the operator signs with
	// these tenant credentials, so the IAM-role destination allowlist does not
	// apply and no AWS identity is involved.
	s3CredsSecret = "minio-credentials"
	backupPrefix  = "e2e"

	roundTripBackup  = "round-trip"
	minutelyBackup   = "minutely"
	refusedRestore   = "refused"
	roundTripRestore = "round-trip-restore"
	deniedBackup     = "iam-denied"

	// The dataset spans two databases: a backup or restore that only handled
	// DB 0 would still pass any single-database check.
	db0KeyCount  = 20
	datasetTotal = db0KeyCount + 2
)

var (
	bkRedisSelector    = componentSelector(backupClusterName, "redis")
	bkSentinelSelector = componentSelector(backupClusterName, "sentinel")

	minioEndpoint = fmt.Sprintf("http://%s.%s.svc.cluster.local:9000", minioName, backupNamespace)

	// roundTripObjectPrefix is the exact layout the operator documents:
	// <prefix>/<namespace>/<cluster>/<crName>/backup-<timestamp>.rdb[.gz].
	roundTripObjectPrefix = fmt.Sprintf("%s/%s/%s/%s/",
		backupPrefix, backupNamespace, backupClusterName, roundTripBackup)
	minutelyObjectPrefix = fmt.Sprintf("%s/%s/%s/%s/",
		backupPrefix, backupNamespace, backupClusterName, minutelyBackup)

	// forwardAddr matches either address family kubectl binds; the port is the
	// same on both, and the dial below always uses 127.0.0.1.
	forwardAddr = regexp.MustCompile(`Forwarding from (?:127\.0\.0\.1|\[::1\]):(\d+)`)
)

// startMinioForward opens a kubectl port-forward to the MinIO Service on a
// kernel-chosen local port and returns that port with a stop function. The
// suite runs S3 assertions from the test process because that is where a
// downloaded object can be decompressed and inspected byte by byte; nothing
// inside the cluster ships an S3 client to do it there.
func startMinioForward() (string, func(), error) {
	// Command pins the kubeconfig; Verify keeps the same discipline run()
	// applies to every other read-only kubectl invocation.
	if err := activeCluster.Verify(); err != nil {
		return "", nil, err
	}
	cmd := activeCluster.Command("kubectl", "port-forward",
		"-n", backupNamespace, "svc/"+minioName, ":9000")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	stop := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}

	ports := make(chan string, 1)
	failed := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if m := forwardAddr.FindStringSubmatch(scanner.Text()); m != nil {
				ports <- m[1]
				// Keep draining, or the forwarder blocks on a full pipe once
				// it starts logging handled connections.
				for scanner.Scan() {
				}
				return
			}
		}
		failed <- fmt.Errorf("port-forward exited before publishing a local port: %s",
			strings.TrimSpace(stderr.String()))
	}()

	select {
	case port := <-ports:
		return port, stop, nil
	case err := <-failed:
		stop()
		return "", nil, err
	case <-time.After(30 * time.Second):
		stop()
		return "", nil, fmt.Errorf("timed out waiting for kubectl port-forward to publish a local port")
	}
}

// withMinIO runs fn against the in-cluster MinIO through a fresh port-forward.
// One forward per call: a long-lived tunnel would die unnoticed between specs
// and fail a later assertion for a reason that has nothing to do with backups.
func withMinIO(fn func(ctx context.Context, client *s3.Client) error) error {
	port, stop, err := startMinioForward()
	if err != nil {
		return err
	}
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// The client is built directly from options, not LoadDefaultConfig: the
	// assertions must speak to this MinIO and never pick up ambient AWS
	// configuration from the developer's or the runner's environment.
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String("http://127.0.0.1:" + port),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(minioAccessKey, minioSecretKey, ""),
	})
	return fn(ctx, client)
}

type s3ObjectInfo struct {
	Key  string
	Size int64
}

// listBucketObjects returns every object under prefix in key order.
func listBucketObjects(ctx context.Context, client *s3.Client, prefix string) ([]s3ObjectInfo, error) {
	var objects []s3ObjectInfo
	var token *string
	for {
		page, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(minioBucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Contents {
			objects = append(objects, s3ObjectInfo{
				Key:  aws.ToString(obj.Key),
				Size: aws.ToInt64(obj.Size),
			})
		}
		if !aws.ToBool(page.IsTruncated) {
			break
		}
		token = page.NextContinuationToken
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	return objects, nil
}

func fetchBucketObject(ctx context.Context, client *s3.Client, key string) ([]byte, error) {
	result, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(minioBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = result.Body.Close() }()
	return io.ReadAll(result.Body)
}

// crCondition is the slice of metav1.Condition the specs assert on.
type crCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type backupCRStatus struct {
	Phase          string        `json:"phase"`
	BackupLocation string        `json:"backupLocation"`
	BackupSize     int64         `json:"backupSize"`
	KeyCount       *int64        `json:"keyCount"`
	BackupCount    int32         `json:"backupCount"`
	LastBackupTime string        `json:"lastBackupTime"`
	Conditions     []crCondition `json:"conditions"`
}

func (s backupCRStatus) condition(name string) (crCondition, bool) {
	for _, c := range s.Conditions {
		if c.Type == name {
			return c, true
		}
	}
	return crCondition{}, false
}

func getBackupCRStatus(namespace, name string) (backupCRStatus, error) {
	out, err := kubectl("get", "redisbackup", name, "-n", namespace, "-o", "json")
	if err != nil {
		return backupCRStatus{}, err
	}
	var cr struct {
		Status backupCRStatus `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &cr); err != nil {
		return backupCRStatus{}, fmt.Errorf("parse RedisBackup %s: %w", name, err)
	}
	return cr.Status, nil
}

type restoreCRStatus struct {
	Phase                         string `json:"phase"`
	Message                       string `json:"message"`
	DatasetReplaced               bool   `json:"datasetReplaced"`
	ExpectedKeyCount              int64  `json:"expectedKeyCount"`
	RestoredKeyCount              int64  `json:"restoredKeyCount"`
	RestoredFrom                  string `json:"restoredFrom"`
	QuiescedDownAfterMilliseconds int32  `json:"quiescedDownAfterMilliseconds"`
}

func getRestoreCRStatus(namespace, name string) (restoreCRStatus, error) {
	out, err := kubectl("get", "redisrestore", name, "-n", namespace, "-o", "json")
	if err != nil {
		return restoreCRStatus{}, err
	}
	var cr struct {
		Status restoreCRStatus `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &cr); err != nil {
		return restoreCRStatus{}, fmt.Errorf("parse RedisRestore %s: %w", name, err)
	}
	return cr.Status, nil
}

// keyspaceKeyTotal sums the keys= fields of an INFO keyspace reply, which
// lists one dbN line per database holding keys. DBSIZE would only count the
// selected database and miss everything the dataset puts in DB 1.
func keyspaceKeyTotal(info string) (int, error) {
	total := 0
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "db") {
			continue
		}
		_, fields, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		for _, field := range strings.Split(fields, ",") {
			if v, ok := strings.CutPrefix(field, "keys="); ok {
				n, err := strconv.Atoi(v)
				if err != nil {
					return 0, fmt.Errorf("parse keyspace line %q: %w", line, err)
				}
				total += n
			}
		}
	}
	return total, nil
}

func nodeKeyTotal(namespace, pod string) (int, error) {
	info, err := redisCLI(namespace, pod, "info", "keyspace")
	if err != nil {
		return 0, err
	}
	return keyspaceKeyTotal(info)
}

// expectDatasetOnNode asserts one pod serves the full dataset, spot-checking a
// value in each database rather than trusting the counts alone.
func expectDatasetOnNode(g Gomega, pod string) {
	total, err := nodeKeyTotal(backupNamespace, pod)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(total).To(Equal(datasetTotal), "%s holds %d keys, expected %d", pod, total, datasetTotal)

	value, err := redisCLI(backupNamespace, pod, "get", "rg-e2e:key-7")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(value).To(Equal("value-7"), "%s serves a wrong value for a DB 0 key", pod)

	value, err = redisCLI(backupNamespace, pod, "-n", "1", "get", "rg-e2e:extra-a")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(value).To(Equal("one"), "%s lost the DB 1 part of the dataset", pod)
}

// expectHealthyBackupCluster asserts the fixture cluster converged: one
// master, every replica linked to it, and the CR reporting Running.
func expectHealthyBackupCluster(g Gomega) {
	states, err := replicationStates(backupNamespace, bkRedisSelector)
	g.Expect(err).NotTo(HaveOccurred())
	masters, replicas := splitByRole(states)
	g.Expect(masters).To(HaveLen(1), "exactly one master expected: %v", states)
	g.Expect(replicas).To(HaveLen(bkRedisReplicas-1), "replica missing: %v", states)
	for _, replica := range replicas {
		g.Expect(replica.MasterLinkStatus).To(Equal("up"), "replica lost its link: %s", replica)
		g.Expect(replica.MasterHost).To(Equal(masters[0].IP), "replica follows a stale master: %s", replica)
	}

	status, err := getSentinelStatus(backupNamespace, backupClusterName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(status.Phase).To(Equal("Running"), "status: %+v", status)
	available, message := status.condition("Available")
	g.Expect(available).To(Equal("True"), "Available condition: %s", message)
}

// currentBackupMaster returns the pod currently serving role:master.
func currentBackupMaster() (replicaState, error) {
	states, err := replicationStates(backupNamespace, bkRedisSelector)
	if err != nil {
		return replicaState{}, err
	}
	masters, _ := splitByRole(states)
	if len(masters) != 1 {
		return replicaState{}, fmt.Errorf("expected exactly one master, got %v", states)
	}
	return masters[0], nil
}

// applyManifestFile writes content under dir and applies it to the cluster.
func applyManifestFile(dir, name, content string) error {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	_, err := kubectl("apply", "-f", path)
	return err
}

// minioManifest is a plain Deployment and Service rather than a chart
// dependency: the suite must control the exact image and the exact security
// context, because the namespace enforces the restricted standard.
func minioManifest() string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %[1]s
  template:
    metadata:
      labels:
        app: %[1]s
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        runAsGroup: 1000
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: minio
          image: %[3]s
          args: ["server", "/data"]
          env:
            - name: MINIO_ROOT_USER
              value: %[4]s
            - name: MINIO_ROOT_PASSWORD
              value: %[5]s
          ports:
            - containerPort: 9000
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
          readinessProbe:
            httpGet:
              path: /minio/health/ready
              port: 9000
            initialDelaySeconds: 5
            periodSeconds: 5
          resources:
            requests:
              cpu: 50m
              memory: 128Mi
            limits:
              memory: 512Mi
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  selector:
    app: %[1]s
  ports:
    - port: 9000
      targetPort: 9000
`, minioName, backupNamespace, minioImage, minioAccessKey, minioSecretKey)
}

func backupClusterManifest() string {
	return fmt.Sprintf(`apiVersion: redis.redguard.io/v1alpha1
kind: RedisSentinel
metadata:
  name: %s
  namespace: %s
spec:
  redisConfig:
    replicas: %d
    storage:
      size: 1Gi
  sentinelConfig:
    replicas: %d
    quorum: 2
    downAfterMilliseconds: 5000
`, backupClusterName, backupNamespace, bkRedisReplicas, bkSentinelReplicas)
}

// oneTimeBackupManifest is a backup with no schedule: it runs exactly once.
func oneTimeBackupManifest() string {
	return fmt.Sprintf(`apiVersion: redis.redguard.io/v1alpha1
kind: RedisBackup
metadata:
  name: %s
  namespace: %s
spec:
  redisClusterRef: %s
  s3:
    bucket: %s
    region: us-east-1
    endpoint: %s
    prefix: %s
    credentialsSecretRef: %s
  retentionPolicy: 7
  compression: true
`, roundTripBackup, backupNamespace, backupClusterName,
		minioBucket, minioEndpoint, backupPrefix, s3CredsSecret)
}

// minutelyBackupManifest fires every minute with retentionPolicy 1, the
// fastest way to watch retention prune a real object.
func minutelyBackupManifest() string {
	return fmt.Sprintf(`apiVersion: redis.redguard.io/v1alpha1
kind: RedisBackup
metadata:
  name: %s
  namespace: %s
spec:
  redisClusterRef: %s
  schedule: "* * * * *"
  s3:
    bucket: %s
    region: us-east-1
    endpoint: %s
    prefix: %s
    credentialsSecretRef: %s
  retentionPolicy: 1
  compression: true
`, minutelyBackup, backupNamespace, backupClusterName,
		minioBucket, minioEndpoint, backupPrefix, s3CredsSecret)
}

// restoreManifest never sets force: the refusal spec needs it absent, and the
// round-trip spec proves an emptied cluster does not need it either.
func restoreManifest(name, objectKey string) string {
	return fmt.Sprintf(`apiVersion: redis.redguard.io/v1alpha1
kind: RedisRestore
metadata:
  name: %s
  namespace: %s
spec:
  redisClusterRef: %s
  backupSource:
    s3:
      bucket: %s
      region: us-east-1
      endpoint: %s
      credentialsSecretRef: %s
    backupPath: %s
    compressed: true
  force: false
`, name, backupNamespace, backupClusterName, minioBucket, minioEndpoint, s3CredsSecret, objectKey)
}

// deniedBackupManifest signs with the operator's own identity (useIAMRole) and
// names a bucket the operator was not started with. The chart install in
// BeforeSuite passes no --allowed-backup-buckets, so the IAM-role path is
// disabled and every bucket is outside the allowlist.
func deniedBackupManifest() string {
	return fmt.Sprintf(`apiVersion: redis.redguard.io/v1alpha1
kind: RedisBackup
metadata:
  name: %s
  namespace: %s
spec:
  redisClusterRef: %s
  s3:
    bucket: bucket-the-operator-never-allowed
    region: us-east-1
    useIAMRole: true
  retentionPolicy: 7
`, deniedBackup, backupNamespace, backupClusterName)
}

// dumpBackupFixtures adds the backup CRs and the object store to the failure
// report; dumpCluster covers the Redis side.
func dumpBackupFixtures() {
	report := func(title string, args ...string) {
		out, err := kubectl(args...)
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "\n--- %s (failed: %v) ---\n%s\n", title, err, out)
			return
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "\n--- %s ---\n%s\n", title, out)
	}
	report("redisbackups", "get", "redisbackup", "-n", backupNamespace, "-o", "yaml")
	report("redisrestores", "get", "redisrestore", "-n", backupNamespace, "-o", "yaml")
	report("minio pods", "get", "pods", "-n", backupNamespace, "-l", "app="+minioName, "-o", "wide")
	report("minio logs", "logs", "-n", backupNamespace, "deployment/"+minioName, "--tail=100")
}

var _ = Describe("Backup and restore", Ordered, func() {
	var workDir string

	// rtObjectKey is the object the round-trip backup wrote; the retention,
	// refusal and restore specs all assert against this one object.
	var rtObjectKey string

	BeforeAll(func() {
		var err error
		workDir, err = os.MkdirTemp("", "redguard-e2e-backup")
		Expect(err).NotTo(HaveOccurred())

		By("creating the namespace under the restricted Pod Security Standard")
		_, err = kubectl("create", "namespace", backupNamespace)
		Expect(err).NotTo(HaveOccurred())
		Expect(enforceRestrictedPodSecurity(backupNamespace)).To(Succeed())

		By("deploying MinIO from a pinned plain manifest")
		Expect(applyManifestFile(workDir, "minio.yaml", minioManifest())).To(Succeed())

		By("creating the S3 credentials Secret the backups reference")
		_, err = kubectl("create", "secret", "generic", s3CredsSecret, "-n", backupNamespace,
			"--from-literal=accessKeyId="+minioAccessKey,
			"--from-literal=secretAccessKey="+minioSecretKey)
		Expect(err).NotTo(HaveOccurred())

		By("applying the RedisSentinel the round trip runs against")
		Expect(applyManifestFile(workDir, "cluster.yaml", backupClusterManifest())).To(Succeed())
	})

	AfterAll(func() {
		// Restores first: their finalizer sweeps staged payloads off the Redis
		// pods, which must still exist for the sweep to reach them.
		By("deleting the restore and backup CRs while the cluster can still be swept")
		_, _ = kubectl("delete", "redisrestore", "--all", "-n", backupNamespace, "--timeout=2m")
		_, _ = kubectl("delete", "redisbackup", "--all", "-n", backupNamespace, "--timeout=2m")

		By("deleting the cluster")
		_, _ = kubectl("delete", "redissentinel", backupClusterName, "-n", backupNamespace,
			"--ignore-not-found", "--timeout=5m")

		By("removing the namespace, which takes MinIO, the Secrets and the volumes with it")
		_, _ = kubectl("delete", "namespace", backupNamespace, "--ignore-not-found", "--timeout=5m")

		if workDir != "" {
			_ = os.RemoveAll(workDir)
		}
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpCluster(backupNamespace, backupClusterName)
			dumpBackupFixtures()
		}
	})

	It("forms the cluster and the object store the round trip runs against", func() {
		By("waiting for MinIO to become ready")
		Eventually(func(g Gomega) {
			pods, err := listPods(backupNamespace, "app="+minioName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(readyPods(pods)).To(HaveLen(1), "minio pods: %v", pods)
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("creating the bucket through the S3 API")
		Eventually(func(g Gomega) {
			g.Expect(withMinIO(func(ctx context.Context, client *s3.Client) error {
				_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
					Bucket: aws.String(minioBucket),
				})
				// A retried Eventually pass may find its own earlier attempt
				// succeeded; both shapes mean the bucket exists.
				var owned *s3types.BucketAlreadyOwnedByYou
				var exists *s3types.BucketAlreadyExists
				if errors.As(err, &owned) || errors.As(err, &exists) {
					return nil
				}
				return err
			})).To(Succeed())
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting for the cluster to reach Running with one master")
		Eventually(func(g Gomega) {
			redis, err := listPods(backupNamespace, bkRedisSelector)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(readyPods(redis)).To(HaveLen(bkRedisReplicas), "ready redis pods: %v", redis)

			sentinels, err := listPods(backupNamespace, bkSentinelSelector)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(readyPods(sentinels)).To(HaveLen(bkSentinelReplicas), "ready sentinel pods: %v", sentinels)

			expectHealthyBackupCluster(g)
		}, 5*time.Minute, 10*time.Second).Should(Succeed())
	})

	It("backs up the populated cluster to an object at the documented key layout", func() {
		master, err := currentBackupMaster()
		Expect(err).NotTo(HaveOccurred())

		By("writing a dataset across two databases and waiting for the replica to acknowledge it")
		populate := fmt.Sprintf(
			`for i in $(seq 1 %d); do redis-cli set rg-e2e:key-$i value-$i >/dev/null || exit 1; done && `+
				`redis-cli -n 1 set rg-e2e:extra-a one >/dev/null && `+
				`redis-cli -n 1 set rg-e2e:extra-b two >/dev/null && `+
				`redis-cli wait 1 10000`, db0KeyCount)
		out, err := kubectl("exec", "-n", backupNamespace, master.Pod, "--", "sh", "-c", populate)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).To(Equal("1"),
			"the replica did not acknowledge the dataset, so the backup could snapshot a partial copy: %q", out)

		total, err := nodeKeyTotal(backupNamespace, master.Pod)
		Expect(err).NotTo(HaveOccurred())
		Expect(total).To(Equal(datasetTotal), "the cluster does not hold the dataset this spec just wrote")

		By("creating a one-time RedisBackup")
		Expect(applyManifestFile(workDir, "backup-round-trip.yaml", oneTimeBackupManifest())).To(Succeed())

		By("waiting for the backup to report Completed")
		Eventually(func(g Gomega) {
			status, err := getBackupCRStatus(backupNamespace, roundTripBackup)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status.Phase).To(Equal("Completed"), "status: %+v", status)
		}, 4*time.Minute, 5*time.Second).Should(Succeed())

		status, err := getBackupCRStatus(backupNamespace, roundTripBackup)
		Expect(err).NotTo(HaveOccurred())

		By("checking the recorded key count matches what the cluster held")
		Expect(status.KeyCount).NotTo(BeNil(),
			"no key count was recorded, so a restore of this backup cannot verify the dataset")
		Expect(*status.KeyCount).To(Equal(int64(datasetTotal)),
			"the recorded key count does not match the dataset the cluster held")

		By("checking exactly one object exists at <prefix>/<namespace>/<cluster>/<crName>/backup-*.rdb.gz")
		var objects []s3ObjectInfo
		Expect(withMinIO(func(ctx context.Context, client *s3.Client) error {
			listed, err := listBucketObjects(ctx, client, roundTripObjectPrefix)
			objects = listed
			return err
		})).To(Succeed())
		Expect(objects).To(HaveLen(1), "objects under %s: %v", roundTripObjectPrefix, objects)
		Expect(objects[0].Key).To(MatchRegexp(
			"^"+regexp.QuoteMeta(roundTripObjectPrefix)+`backup-\d{8}-\d{6}\.rdb\.gz$`),
			"the object does not follow the documented key layout")
		Expect(status.BackupLocation).To(Equal("s3://"+minioBucket+"/"+objects[0].Key),
			"status.backupLocation does not name the object that exists")
		Expect(objects[0].Size).To(Equal(status.BackupSize),
			"status.backupSize does not match the stored object")

		By("downloading the object and checking it is valid gzip wrapping an RDB")
		var raw []byte
		Expect(withMinIO(func(ctx context.Context, client *s3.Client) error {
			fetched, err := fetchBucketObject(ctx, client, objects[0].Key)
			raw = fetched
			return err
		})).To(Succeed())
		Expect(int64(len(raw))).To(Equal(status.BackupSize))

		gz, err := gzip.NewReader(bytes.NewReader(raw))
		Expect(err).NotTo(HaveOccurred(), "the stored object is not gzip")
		// Reading to the end also checks the gzip footer: a truncated upload
		// that kept a valid header would still fail here, as it would at
		// restore time.
		payload, err := io.ReadAll(gz)
		Expect(err).NotTo(HaveOccurred(), "the gzip stream is truncated or corrupt")
		Expect(gz.Close()).To(Succeed())
		Expect(len(payload)).To(BeNumerically(">", 5), "the decompressed payload is empty")
		Expect(string(payload[:5])).To(Equal("REDIS"),
			"the decompressed payload does not start with the RDB magic")

		rtObjectKey = objects[0].Key
	})

	It("prunes to the retention count without touching a sibling backup's objects", func() {
		Expect(rtObjectKey).NotTo(BeEmpty(), "the round-trip spec did not record its object key")

		By("creating an every-minute RedisBackup with retentionPolicy 1 on the same cluster")
		Expect(applyManifestFile(workDir, "backup-minutely.yaml", minutelyBackupManifest())).To(Succeed())

		By("waiting for at least two runs, so retention has had an older object to delete")
		Eventually(func(g Gomega) {
			status, err := getBackupCRStatus(backupNamespace, minutelyBackup)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status.BackupCount).To(BeNumerically(">=", 2), "status: %+v", status)
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("checking only the newest object survives under this backup's prefix")
		// Status is read inside the poll: a run can complete between a status
		// read and a listing, and the pair must describe the same run.
		Eventually(func(g Gomega) {
			status, err := getBackupCRStatus(backupNamespace, minutelyBackup)
			g.Expect(err).NotTo(HaveOccurred())

			var objects []s3ObjectInfo
			g.Expect(withMinIO(func(ctx context.Context, client *s3.Client) error {
				listed, err := listBucketObjects(ctx, client, minutelyObjectPrefix)
				objects = listed
				return err
			})).To(Succeed())
			g.Expect(objects).To(HaveLen(1), "retention kept the wrong number of objects: %v", objects)
			g.Expect("s3://"+minioBucket+"/"+objects[0].Key).To(Equal(status.BackupLocation),
				"the surviving object is not the newest run's")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("checking the sibling backup's object was not pruned")
		var rtObjects []s3ObjectInfo
		Expect(withMinIO(func(ctx context.Context, client *s3.Client) error {
			listed, err := listBucketObjects(ctx, client, roundTripObjectPrefix)
			rtObjects = listed
			return err
		})).To(Succeed())
		Expect(rtObjects).To(HaveLen(1),
			"the minutely backup's retention deleted the round-trip backup's object: %v", rtObjects)
		Expect(rtObjects[0].Key).To(Equal(rtObjectKey))

		By("deleting the scheduled backup so no run overlaps the restore specs")
		_, err := kubectl("delete", "redisbackup", minutelyBackup, "-n", backupNamespace, "--timeout=2m")
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses a forceless restore into a cluster that still holds data", func() {
		Expect(rtObjectKey).NotTo(BeEmpty(), "the round-trip spec did not record its object key")

		master, err := currentBackupMaster()
		Expect(err).NotTo(HaveOccurred())
		total, err := nodeKeyTotal(backupNamespace, master.Pod)
		Expect(err).NotTo(HaveOccurred())
		Expect(total).To(Equal(datasetTotal), "the cluster no longer holds the dataset this spec relies on")

		By("applying a RedisRestore without spec.force")
		Expect(applyManifestFile(workDir, "restore-refused.yaml",
			restoreManifest(refusedRestore, rtObjectKey))).To(Succeed())

		By("waiting for the refusal")
		Eventually(func(g Gomega) {
			status, err := getRestoreCRStatus(backupNamespace, refusedRestore)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status.Phase).To(Equal("Failed"), "status: %+v", status)
			g.Expect(status.Message).To(ContainSubstring("existing data"),
				"the restore failed for a reason other than the safety gate: %s", status.Message)
			g.Expect(status.Message).To(ContainSubstring("spec.force=true"),
				"the refusal does not tell the user how to proceed deliberately: %s", status.Message)
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		status, err := getRestoreCRStatus(backupNamespace, refusedRestore)
		Expect(err).NotTo(HaveOccurred())
		Expect(status.DatasetReplaced).To(BeFalse(),
			"the gate refused the restore after the dataset was already replaced")

		By("checking the existing data is untouched on every node")
		pods, err := listPods(backupNamespace, bkRedisSelector)
		Expect(err).NotTo(HaveOccurred())
		ready := readyPods(pods)
		Expect(ready).To(HaveLen(bkRedisReplicas), "pods: %v", pods)
		for _, p := range ready {
			expectDatasetOnNode(Default, p.Name)
		}

		By("checking the cluster was not destabilised by the refused restore")
		Consistently(expectHealthyBackupCluster, 15*time.Second, 5*time.Second).Should(Succeed())
	})

	It("restores the backup into a flushed cluster on every node and leaves it healthy", func() {
		Expect(rtObjectKey).NotTo(BeEmpty(), "the round-trip spec did not record its object key")

		By("flushing the dataset and waiting for the replica to acknowledge the flush")
		master, err := currentBackupMaster()
		Expect(err).NotTo(HaveOccurred())
		reply, err := redisCLI(backupNamespace, master.Pod, "flushall")
		Expect(err).NotTo(HaveOccurred())
		Expect(reply).To(Equal("OK"))
		acked, err := redisCLI(backupNamespace, master.Pod, "wait", "1", "10000")
		Expect(err).NotTo(HaveOccurred())
		Expect(acked).To(Equal("1"), "the flush did not reach the replica")

		total, err := nodeKeyTotal(backupNamespace, master.Pod)
		Expect(err).NotTo(HaveOccurred())
		Expect(total).To(BeZero(), "the flush left keys behind, so the restore gate would refuse")

		By("applying a forceless RedisRestore against the emptied cluster")
		Expect(applyManifestFile(workDir, "restore-round-trip.yaml",
			restoreManifest(roundTripRestore, rtObjectKey))).To(Succeed())

		By("waiting for the restore to report Completed")
		Eventually(func(g Gomega) {
			status, err := getRestoreCRStatus(backupNamespace, roundTripRestore)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status.Phase).To(Equal("Completed"), "status: %+v", status)
		}, 8*time.Minute, 10*time.Second).Should(Succeed())

		status, err := getRestoreCRStatus(backupNamespace, roundTripRestore)
		Expect(err).NotTo(HaveOccurred())
		Expect(status.RestoredKeyCount).To(Equal(int64(datasetTotal)),
			"the restore loaded a different number of keys than the backup held")
		Expect(status.ExpectedKeyCount).To(Equal(int64(datasetTotal)),
			"the producing RedisBackup's key count never reached the verification")
		Expect(status.RestoredFrom).To(Equal(rtObjectKey))
		Expect(status.QuiescedDownAfterMilliseconds).To(BeZero(),
			"the sentinels were left quiesced, so a real master failure would go undetected")

		By("checking every node serves the restored dataset")
		Eventually(func(g Gomega) {
			pods, err := listPods(backupNamespace, bkRedisSelector)
			g.Expect(err).NotTo(HaveOccurred())
			ready := readyPods(pods)
			g.Expect(ready).To(HaveLen(bkRedisReplicas), "pods: %v", pods)
			for _, p := range ready {
				expectDatasetOnNode(g, p.Name)
			}
		}, 4*time.Minute, 10*time.Second).Should(Succeed())

		By("checking the cluster converged back to one master with a linked replica")
		Eventually(expectHealthyBackupCluster, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("checking durability was switched back on after the verified load")
		newMaster, err := currentBackupMaster()
		Expect(err).NotTo(HaveOccurred())
		appendonly, err := redisCLI(backupNamespace, newMaster.Pod, "config", "get", "appendonly")
		Expect(err).NotTo(HaveOccurred())
		Expect(appendonly).To(ContainSubstring("yes"),
			"the master still runs with AOF off, so the restored data has no durability")
	})

	It("rejects an IAM-role backup whose bucket the operator was not started with", func() {
		By("applying a RedisBackup that signs with the operator's identity for a disallowed bucket")
		Expect(applyManifestFile(workDir, "backup-denied.yaml", deniedBackupManifest())).To(Succeed())

		// The reason distinguishes the policy gate from a run that failed:
		// an attempted AWS call would surface as BackupFailed, never as
		// DestinationNotAllowed.
		By("waiting for the refusal with the condition the docs describe")
		Eventually(func(g Gomega) {
			status, err := getBackupCRStatus(backupNamespace, deniedBackup)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status.Phase).To(Equal("Failed"), "status: %+v", status)

			ready, found := status.condition("Ready")
			g.Expect(found).To(BeTrue(), "no Ready condition on the refused backup: %+v", status)
			g.Expect(ready.Status).To(Equal("False"))
			g.Expect(ready.Reason).To(Equal("DestinationNotAllowed"),
				"the backup failed for a reason other than the destination policy: %+v", ready)
			g.Expect(ready.Message).To(ContainSubstring("--allowed-backup-buckets"),
				"the refusal does not point at the operator flag that governs it: %s", ready.Message)
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("checking no run ever started")
		status, err := getBackupCRStatus(backupNamespace, deniedBackup)
		Expect(err).NotTo(HaveOccurred())
		Expect(status.LastBackupTime).To(BeEmpty(), "a run completed despite the refusal")
		Expect(status.BackupLocation).To(BeEmpty(), "an object location was recorded despite the refusal")

		By("checking the refusal was surfaced as a warning event")
		events, err := warningEvents(backupNamespace, "DestinationNotAllowed")
		Expect(err).NotTo(HaveOccurred())
		Expect(events).To(ContainSubstring("Warning"), "no DestinationNotAllowed event was recorded")
	})
})
