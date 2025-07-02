//go:build e2e

package soci

import (
	"context"
	_ "embed"
	"flag"
	"log"
	"os"
	"os/signal"
	"testing"
	"time"

	fwext "github.com/aws/aws-k8s-tester/internal/e2e"
	"github.com/aws/aws-k8s-tester/test/manifests"
	"github.com/aws/aws-sdk-go-v2/aws"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

var (
	testenv                            env.Environment
	nodeType                           *string
	nodeCount                          int
	sociTestImage                      *string
	instanceTypes                      *string
	sociMaxConcurrentDownloads         *string
	sociMaxConcurrentDownloadsPerImage *string
	sociMaxConcurrentUnpacksPerImage   *string
)

// checkNodeTypes verifies the node types and counts nodes with the specified type
func checkNodeTypes(ctx context.Context, config *envconf.Config) (context.Context, error) {
	clientset, err := kubernetes.NewForConfig(config.Client().RESTConfig())
	if err != nil {
		return ctx, err
	}

	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return ctx, err
	}

	if *nodeType != "" {
		for _, v := range nodes.Items {
			if v.Labels["node.kubernetes.io/instance-type"] == *nodeType {
				nodeCount++
				// Check for SOCI labels or annotations if applicable
			}
		}
	} else {
		log.Printf("No node type specified. Using the node type %s in the node groups.", nodes.Items[0].Labels["node.kubernetes.io/instance-type"])
		nodeType = aws.String(nodes.Items[0].Labels["node.kubernetes.io/instance-type"])
		nodeCount = len(nodes.Items)
	}

	return ctx, nil
}

// Helper function to deploy DaemonSet + Wait for Ready
func deployDaemonSet(name, namespace string) env.Func {
	return func(ctx context.Context, config *envconf.Config) (context.Context, error) {
		log.Printf("Waiting for %s daemonset to be ready.", name)
		daemonset := appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		}
		err := wait.For(
			fwext.NewConditionExtension(config.Client().Resources()).DaemonSetReady(&daemonset),
			wait.WithTimeout(5*time.Minute),
		)
		if err != nil {
			return ctx, err
		}
		log.Printf("%s daemonset is ready.", name)
		return ctx, nil
	}
}

func TestMain(m *testing.M) {
	nodeType = flag.String("nodeType", "", "node type for the tests")
	sociTestImage = flag.String("sociTestImage", "", "test image for SOCI tests")
	instanceTypes = flag.String("instanceTypes", "", "comma-separated list of instance types to try for SOCI tests")
	sociMaxConcurrentDownloads = flag.String("soci-max-concurrent-downloads", "-1", "Max concurrent downloads for SOCI snapshotter")
	sociMaxConcurrentDownloadsPerImage = flag.String("soci-max-concurrent-downloads-per-image", "8", "Max concurrent downloads per image for SOCI snapshotter")
	sociMaxConcurrentUnpacksPerImage = flag.String("soci-max-concurrent-unpacks-per-image", "4", "Max concurrent unpacks per image for SOCI snapshotter")

	cfg, err := envconf.NewFromFlags()
	if err != nil {
		log.Fatalf("failed to initialize test environment: %v", err)
	}
	testenv = env.NewWithConfig(cfg)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	testenv = testenv.WithContext(ctx)

	// List of manifests to deploy
	deploymentManifests := [][]byte{
		manifests.NodeExporterManifest,
	}

	// Set up functions for the SOCI tests
	setUpFunctions := []env.Func{
		// Apply Node Exporter manifest
		func(ctx context.Context, config *envconf.Config) (context.Context, error) {
			log.Println("Applying Node Exporter manifest.")
			err := fwext.ApplyManifests(config.Client().RESTConfig(), deploymentManifests...)
			if err != nil {
				return ctx, err
			}
			log.Println("Successfully applied Node Exporter manifest.")
			return ctx, nil
		},
		// Wait for Node Exporter to be ready
		deployDaemonSet("node-exporter", "kube-system"),
		// Check node types after Node Exporter is ready
		checkNodeTypes,
	}
	testenv.Setup(setUpFunctions...)

	testenv.Finish(
		func(ctx context.Context, config *envconf.Config) (context.Context, error) {
			log.Println("Cleaning up test resources...")
			// Delete the Node Exporter
			err := fwext.DeleteManifests(config.Client().RESTConfig(), deploymentManifests...)
			if err != nil {
				log.Printf("Error deleting manifests: %v", err)
			}
			return ctx, nil
		},
	)

	os.Exit(testenv.Run(m))
}
