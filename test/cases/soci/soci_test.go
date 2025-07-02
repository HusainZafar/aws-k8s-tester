//go:build e2e

package soci

import (
	"context"
	_ "embed"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	fwext "github.com/aws/aws-k8s-tester/internal/e2e"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	//go:embed manifests/image-pull-job.yaml
	imagePullJobTemplate []byte

	//go:embed manifests/soci-verify-job.yaml
	sociVerifyJobTemplate []byte

	//go:embed manifests/collect-metrics-pod.yaml
	metricsCollectorTemplate []byte
)

const (
	ImagePullJobName             = "image-pull-job"
	SociSnapshotterVerifyJobName = "soci-snapshotter-verify-job"
	MetricsCollectorPodName      = "metrics-collector"
)

type ManifestVars struct {
	NodeType  string
	TestImage string
	JobName   string
}

type testContext struct {
	clientset *kubernetes.Clientset
	cfg       *envconf.Config
	t         *testing.T
}

type Metrics struct {
	CPUPercent    float64
	MemoryPercent float64
	DiskIO        float64
}

type ImagePullMetrics struct {
	Metrics
	ImageName    string
	PullDuration time.Duration
}

func newTestContext(t *testing.T, cfg *envconf.Config) (*testContext, error) {
	clientset, err := kubernetes.NewForConfig(cfg.Client().RESTConfig())
	if err != nil {
		return nil, fmt.Errorf("error creating clientset: %v", err)
	}

	return &testContext{
		clientset: clientset,
		cfg:       cfg,
		t:         t,
	}, nil
}

// TestSOCI verifies SOCI configuration and performance
func TestSOCI(t *testing.T) {
	// Define the test
	sociTest := features.New("soci-test").
		WithLabel("suite", "soci").
		WithLabel("feature", "fast-container-image-pull").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			testCtx, err := newTestContext(t, cfg)
			if err != nil {
				t.Fatal(err)
			}
			return context.WithValue(ctx, "testCtx", testCtx)
		}).
		Assess("SOCI configuration and performance", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			testCtx := ctx.Value("testCtx").(*testContext)
			metrics, sociEnabled := runSociTest(ctx, t, testCtx)

			t.Logf("SOCI Configuration Status: %s", statusToString(sociEnabled))
			t.Logf("Image Pull Metrics:")
			t.Logf("  Image: %s", metrics.ImageName)
			t.Logf("  Pull Duration: %v", metrics.PullDuration)
			t.Logf("  Max CPU: %.2f%%", metrics.CPUPercent)
			t.Logf("  Max Memory: %.2f%%", metrics.MemoryPercent)
			t.Logf("  Max Disk I/O: %.2f", metrics.DiskIO)
			return ctx
		}).
		Feature()

	testenv := env.NewWithConfig(envconf.New())
	testenv.Test(t, sociTest)
}

// Helper function to format status as string with emoji
func statusToString(status bool) string {
	if status {
		return "Enabled"
	}
	return "Not enabled"
}

// Helper function to extract value from Prometheus metrics
func getMetricValue(metricFamilies map[string]*dto.MetricFamily, metricName string, labelValues ...string) float64 {
	family, ok := metricFamilies[metricName]
	if !ok {
		return 0
	}

	for _, metric := range family.Metric {
		// If no label values provided, just return the first metric's value
		if len(labelValues) == 0 {
			if metric.Gauge != nil {
				return metric.Gauge.GetValue()
			} else if metric.Counter != nil {
				return metric.Counter.GetValue()
			} else if metric.Untyped != nil {
				return metric.Untyped.GetValue()
			}
		}

		// Otherwise match all label values
		if len(labelValues) > 0 && len(metric.Label) >= len(labelValues)/2 {
			allMatch := true
			for i := 0; i < len(labelValues); i += 2 {
				labelName := labelValues[i]
				labelValue := labelValues[i+1]

				matched := false
				for _, label := range metric.Label {
					if label.GetName() == labelName && label.GetValue() == labelValue {
						matched = true
						break
					}
				}

				if !matched {
					allMatch = false
					break
				}
			}

			if allMatch {
				if metric.Gauge != nil {
					return metric.Gauge.GetValue()
				} else if metric.Counter != nil {
					return metric.Counter.GetValue()
				} else if metric.Untyped != nil {
					return metric.Untyped.GetValue()
				}
			}
		}
	}

	return 0
}

func updateMaxMetrics(max *Metrics, current *Metrics) {
	max.CPUPercent = math.Max(max.CPUPercent, current.CPUPercent)
	max.MemoryPercent = math.Max(max.MemoryPercent, current.MemoryPercent)
	max.DiskIO = math.Max(max.DiskIO, current.DiskIO)
}

func parseMetrics(metricFamilies map[string]*dto.MetricFamily) *Metrics {
	metrics := &Metrics{}

	// Extract CPU metrics
	idleValue := getMetricValue(metricFamilies, "node_cpu_seconds_total", "mode", "idle")
	totalValue := getMetricValue(metricFamilies, "node_cpu_seconds_total")
	if totalValue > 0 {
		metrics.CPUPercent = (1 - idleValue/totalValue) * 100
	}

	// Extract memory metrics
	if memTotal := getMetricValue(metricFamilies, "node_memory_MemTotal_bytes"); memTotal > 0 {
		memFree := getMetricValue(metricFamilies, "node_memory_MemFree_bytes")
		memBuffers := getMetricValue(metricFamilies, "node_memory_Buffers_bytes")
		memCached := getMetricValue(metricFamilies, "node_memory_Cached_bytes")

		memUsed := memTotal - memFree - memBuffers - memCached
		if memTotal > 0 {
			metrics.MemoryPercent = (memUsed / memTotal) * 100
		}
	}

	// Extract disk I/O metrics
	diskRead := getMetricValue(metricFamilies, "node_disk_read_bytes_total")
	diskWrite := getMetricValue(metricFamilies, "node_disk_written_bytes_total")
	metrics.DiskIO = (diskRead + diskWrite) / (1024 * 1024)

	return metrics
}

func runSociTest(ctx context.Context, t *testing.T, testCtx *testContext) (*ImagePullMetrics, bool) {
	// First run image pull test
	metrics, err := runImagePullTest(ctx, t, testCtx)
	if err != nil {
		t.Fatalf("Image pull test failed: %v", err)
	}

	// Then run SOCI verification
	sociEnabled, err := verifySociConfig(ctx, t, testCtx)
	if err != nil {
		t.Logf("SOCI verification failed: %v", err)
	}

	return metrics, sociEnabled
}

func runImagePullTest(ctx context.Context, t *testing.T, testCtx *testContext) (*ImagePullMetrics, error) {
	// Find a target node
	nodes, err := testCtx.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %v", err)
	}

	var targetNodeName string
	for _, node := range nodes.Items {
		if *nodeType == "" || node.Labels["node.kubernetes.io/instance-type"] == *nodeType {
			targetNodeName = node.Name
			if *nodeType == "" {
				*nodeType = node.Labels["node.kubernetes.io/instance-type"]
			}
			break
		}
	}

	if targetNodeName == "" {
		t.Fatalf("No node found with instance type: %s", *nodeType)
	}

	// Start metrics collection pod BEFORE running the job
	t.Log("Starting metrics collection...")
	metricsCollectorManifest, err := fwext.RenderManifests(metricsCollectorTemplate, struct {
		NodeName string
	}{
		NodeName: targetNodeName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to render metrics collector manifest: %v", err)
	}

	err = fwext.ApplyManifests(testCtx.cfg.Client().RESTConfig(), metricsCollectorManifest)
	if err != nil {
		return nil, err
	}
	defer fwext.DeleteManifests(testCtx.cfg.Client().RESTConfig(), metricsCollectorManifest)

	err = wait.For(func(ctx context.Context) (bool, error) {
		pod, err := testCtx.clientset.CoreV1().Pods("default").Get(ctx, MetricsCollectorPodName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		return pod.Status.Phase == corev1.PodRunning, nil
	}, wait.WithTimeout(30*time.Second), wait.WithInterval(time.Second))
	if err != nil {
		t.Fatal("Metrics collector failed to start")
	}

	imagePullJob, jobManifest, err := createJobAndWaitForCompletion(ctx, testCtx, ImagePullJobName, imagePullJobTemplate, ManifestVars{
		TestImage: *sociTestImage,
		NodeType:  *nodeType,
		JobName:   ImagePullJobName,
	})
	if err != nil {
		return nil, err
	}
	defer deleteJob(ctx, testCtx, jobManifest)

	// Get all logs from the metrics collector
	logsReq := testCtx.clientset.CoreV1().Pods("default").GetLogs(MetricsCollectorPodName, &corev1.PodLogOptions{})
	logsBytes, err := logsReq.DoRaw(ctx)
	if err != nil {
		t.Logf("Error getting metrics logs: %v", err)
	}

	// Parse the metrics and find maximum values
	maxMetrics := &Metrics{}
	// Split logs by the separator we added
	metricsSamples := strings.Split(string(logsBytes), "---")

	for _, sample := range metricsSamples {
		if sample == "" {
			continue
		}

		parser := expfmt.TextParser{}
		metricFamilies, err := parser.TextToMetricFamilies(strings.NewReader(sample))
		if err != nil {
			continue
		}

		currentMetrics := parseMetrics(metricFamilies)
		updateMaxMetrics(maxMetrics, currentMetrics)
	}

	var pullDuration time.Duration
	var imageName string

	// Get image name from job spec
	if len(imagePullJob.Spec.Template.Spec.Containers) > 0 {
		imageName = imagePullJob.Spec.Template.Spec.Containers[0].Image
	}

	// Calculate duration from job start and completion time
	if imagePullJob.Status.StartTime != nil && imagePullJob.Status.CompletionTime != nil {
		pullDuration = imagePullJob.Status.CompletionTime.Sub(imagePullJob.Status.StartTime.Time)
		t.Logf("Job duration: %v", pullDuration)
	} else {
		t.Fatal("Could not determine pull duration")
	}

	return &ImagePullMetrics{
		Metrics: Metrics{
			CPUPercent:    maxMetrics.CPUPercent,
			MemoryPercent: maxMetrics.MemoryPercent,
			DiskIO:        maxMetrics.DiskIO,
		},
		ImageName:    imageName,
		PullDuration: pullDuration,
	}, nil
}

func verifySociConfig(ctx context.Context, t *testing.T, testCtx *testContext) (bool, error) {
	jobObject, jobManifest, err := createJobAndWaitForCompletion(ctx, testCtx, SociSnapshotterVerifyJobName, sociVerifyJobTemplate, ManifestVars{
		NodeType: *nodeType,
		JobName:  SociSnapshotterVerifyJobName,
	})
	if err != nil {
		return false, err
	}
	defer deleteJob(ctx, testCtx, jobManifest)

	// Check job logs for SOCI status
	jobLogs, err := fwext.GetJobLogs(testCtx.cfg.Client().RESTConfig(), jobObject)
	if err != nil {
		t.Logf("Error getting job logs: %v", err)
	}

	// Parse job logs for SOCI status
	sociConfigEnabled := strings.Contains(jobLogs, "SOCI_CONFIG_STATUS: Found")
	sociProcessRunning := strings.Contains(jobLogs, "SOCI_PROCESS_STATUS: Running")
	containerdConfigured := strings.Contains(jobLogs, "CONTAINERD_CONFIG_STATUS: SOCI enabled")

	// Log detailed status
	t.Logf("SOCI Component Status:")
	t.Logf("  Config: %s", statusToString(sociConfigEnabled))
	t.Logf("  Process: %s", statusToString(sociProcessRunning))
	t.Logf("  Containerd Integration: %s", statusToString(containerdConfigured))

	// Overall SOCI status
	sociEnabled := sociConfigEnabled && sociProcessRunning && containerdConfigured

	return sociEnabled, nil
}

func deleteJob(ctx context.Context, testCtx *testContext, jobManifest []byte) error {
	return fwext.DeleteManifests(testCtx.cfg.Client().RESTConfig(), jobManifest)
}

func createJobAndWaitForCompletion(ctx context.Context, testCtx *testContext, jobName string, jobTemplate []byte, manifestVars ManifestVars) (*batchv1.Job, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	// Render job manifest
	jobManifest, err := fwext.RenderManifests(jobTemplate, manifestVars)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to render manifest: %v", err)
	}

	// Apply the job
	err = fwext.ApplyManifests(testCtx.cfg.Client().RESTConfig(), jobManifest)
	if err != nil {
		return nil, jobManifest, fmt.Errorf("failed to apply manifest: %v", err)
	}

	// Wait for job completion
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: "default",
		},
	}

	err = wait.For(
		fwext.NewConditionExtension(testCtx.cfg.Client().Resources()).JobSucceeded(job),
		wait.WithContext(ctx),
	)
	if err != nil {
		return nil, jobManifest, fmt.Errorf("job failed: %v", err)
	}

	// Get the final job object
	job, err = testCtx.clientset.BatchV1().Jobs("default").Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		return nil, jobManifest, fmt.Errorf("failed to get job: %v", err)
	}

	return job, jobManifest, nil
}
