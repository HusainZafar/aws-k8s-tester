package e2e

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	apimachinerywait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/e2e-framework/klient/k8s"
)

// PodRunning checks if a pod is in the Running phase
func (c *ConditionExtension) PodRunning(pod k8s.Object) apimachinerywait.ConditionWithContextFunc {
	return func(ctx context.Context) (done bool, err error) {
		if err := c.resources.Get(ctx, pod.GetName(), pod.GetNamespace(), pod); err != nil {
			return false, err
		}

		p := pod.(*corev1.Pod)
		if p.Status.Phase == corev1.PodRunning {
			return true, nil
		}

		return false, nil
	}
}

// GetPodLogs is a helper function to get logs from a pod
func GetPodLogs(restConfig *rest.Config, pod k8s.Object) (string, error) {
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return "", fmt.Errorf("failed to create clientset: %w", err)
	}

	p := pod.(*corev1.Pod)

	// Get the first container name
	containerName := ""
	if len(p.Spec.Containers) > 0 {
		containerName = p.Spec.Containers[0].Name
	}

	req := clientset.CoreV1().Pods(p.Namespace).GetLogs(p.Name, &corev1.PodLogOptions{
		Container: containerName,
	})

	stream, err := req.Stream(context.TODO())
	if err != nil {
		return "", fmt.Errorf("error getting log stream: %w", err)
	}
	defer stream.Close()

	data, err := io.ReadAll(stream)
	if err != nil {
		return "", fmt.Errorf("error reading logs: %w", err)
	}

	return string(data), nil
}
