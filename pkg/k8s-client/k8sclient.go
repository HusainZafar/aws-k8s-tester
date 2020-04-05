/*
Copyright 2018 The Kubernetes Authors.

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

// Package k8sclient implements various k8s utils.
package k8sclient

import (
	"net"
	"strings"
	"time"

	"go.uber.org/zap"
	apiv1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
)

/*
https://github.com/kubernetes/perf-tests/blob/master/clusterloader2/pkg/framework/client/objects.go
https://github.com/kubernetes/kubernetes/blob/master/cmd/kubeadm/app/util/apiclient/wait.go#L49
*/

const (
	// Parameters for retrying with exponential backoff.
	retryBackoffInitialDuration = 100 * time.Millisecond
	retryBackoffFactor          = 3
	retryBackoffJitter          = 0
	retryBackoffSteps           = 6

	// DefaultNamespaceDeletionInterval is the default namespace deletion interval.
	DefaultNamespaceDeletionInterval = 15 * time.Second
	// DefaultNamespaceDeletionTimeout is the default namespace deletion timeout.
	DefaultNamespaceDeletionTimeout = 10 * time.Minute
)

// RetryWithExponentialBackOff a utility for retrying the given function with exponential backoff.
func RetryWithExponentialBackOff(fn wait.ConditionFunc) error {
	backoff := wait.Backoff{
		Duration: retryBackoffInitialDuration,
		Factor:   retryBackoffFactor,
		Jitter:   retryBackoffJitter,
		Steps:    retryBackoffSteps,
	}
	return wait.ExponentialBackoff(backoff, fn)
}

// IsRetryableAPIError verifies whether the error is retryable.
func IsRetryableAPIError(err error) bool {
	// These errors may indicate a transient error that we can retry in tests.
	if apierrs.IsInternalError(err) || apierrs.IsTimeout(err) || apierrs.IsServerTimeout(err) ||
		apierrs.IsTooManyRequests(err) || utilnet.IsProbableEOF(err) || utilnet.IsConnectionReset(err) ||
		// Retryable resource-quotas conflict errors may be returned in some cases, e.g. https://github.com/kubernetes/kubernetes/issues/67761
		isResourceQuotaConflictError(err) ||
		// Our client is using OAuth2 where 401 (unauthorized) can mean that our token has expired and we need to retry with a new one.
		apierrs.IsUnauthorized(err) {
		return true
	}
	// If the error sends the Retry-After header, we respect it as an explicit confirmation we should retry.
	if _, shouldRetry := apierrs.SuggestsClientDelay(err); shouldRetry {
		return true
	}
	return false
}

func isResourceQuotaConflictError(err error) bool {
	apiErr, ok := err.(apierrs.APIStatus)
	if !ok {
		return false
	}
	if apiErr.Status().Reason != metav1.StatusReasonConflict {
		return false
	}
	return apiErr.Status().Details != nil && apiErr.Status().Details.Kind == "resourcequotas"
}

// IsRetryableNetError determines whether the error is a retryable net error.
func IsRetryableNetError(err error) bool {
	if netError, ok := err.(net.Error); ok {
		return netError.Temporary() || netError.Timeout()
	}
	return false
}

// ApiCallOptions describes how api call errors should be treated, i.e. which errors should be
// allowed (ignored) and which should be retried.
type ApiCallOptions struct {
	shouldAllowError func(error) bool
	shouldRetryError func(error) bool
}

// Allow creates an ApiCallOptions that allows (ignores) errors matching the given predicate.
func Allow(allowErrorPredicate func(error) bool) *ApiCallOptions {
	return &ApiCallOptions{shouldAllowError: allowErrorPredicate}
}

// Retry creates an ApiCallOptions that retries errors matching the given predicate.
func Retry(retryErrorPredicate func(error) bool) *ApiCallOptions {
	return &ApiCallOptions{shouldRetryError: retryErrorPredicate}
}

// RetryFunction opaques given function into retryable function.
func RetryFunction(f func() error, options ...*ApiCallOptions) wait.ConditionFunc {
	var shouldAllowErrorFuncs, shouldRetryErrorFuncs []func(error) bool
	for _, option := range options {
		if option.shouldAllowError != nil {
			shouldAllowErrorFuncs = append(shouldAllowErrorFuncs, option.shouldAllowError)
		}
		if option.shouldRetryError != nil {
			shouldRetryErrorFuncs = append(shouldRetryErrorFuncs, option.shouldRetryError)
		}
	}
	return func() (bool, error) {
		err := f()
		if err == nil {
			return true, nil
		}
		if IsRetryableAPIError(err) || IsRetryableNetError(err) {
			return false, nil
		}
		for _, shouldAllowError := range shouldAllowErrorFuncs {
			if shouldAllowError(err) {
				return true, nil
			}
		}
		for _, shouldRetryError := range shouldRetryErrorFuncs {
			if shouldRetryError(err) {
				return false, nil
			}
		}
		return false, err
	}
}

// ListPodsWithOptions lists the pods using the provided options.
func ListPodsWithOptions(c clientset.Interface, namespace string, listOpts metav1.ListOptions) ([]apiv1.Pod, error) {
	var pods []apiv1.Pod
	listFunc := func() error {
		podsList, err := c.CoreV1().Pods(namespace).List(listOpts)
		if err != nil {
			return err
		}
		pods = podsList.Items
		return nil
	}
	if err := RetryWithExponentialBackOff(RetryFunction(listFunc)); err != nil {
		return pods, err
	}
	return pods, nil
}

// ListNodes returns list of cluster nodes.
func ListNodes(c clientset.Interface) ([]apiv1.Node, error) {
	return ListNodesWithOptions(c, metav1.ListOptions{})
}

// ListNodesWithOptions lists the cluster nodes using the provided options.
func ListNodesWithOptions(c clientset.Interface, listOpts metav1.ListOptions) ([]apiv1.Node, error) {
	var nodes []apiv1.Node
	listFunc := func() error {
		nodesList, err := c.CoreV1().Nodes().List(listOpts)
		if err != nil {
			return err
		}
		nodes = nodesList.Items
		return nil
	}
	if err := RetryWithExponentialBackOff(RetryFunction(listFunc)); err != nil {
		return nodes, err
	}
	return nodes, nil
}

// CreateNamespace creates a single namespace with given name.
func CreateNamespace(lg *zap.Logger, c clientset.Interface, namespace string) error {
	createFunc := func() error {
		lg.Info("creating namespace", zap.String("namespace", namespace))
		_, err := c.CoreV1().Namespaces().Create(&apiv1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
		if err == nil {
			lg.Info("created namespace", zap.String("namespace", namespace))
		} else {
			lg.Warn("failed to create namespace", zap.String("namespace", namespace), zap.Error(err))
		}
		return err
	}
	return RetryWithExponentialBackOff(RetryFunction(createFunc, Allow(apierrs.IsAlreadyExists)))
}

// DeleteNamespace deletes namespace with given name.
func DeleteNamespace(lg *zap.Logger, c clientset.Interface, namespace string) error {
	foreground, zero := metav1.DeletePropagationForeground, int64(0)
	deleteFunc := func() error {
		lg.Info("deleting namespace", zap.String("namespace", namespace))
		err := c.CoreV1().Namespaces().Delete(
			namespace,
			&metav1.DeleteOptions{
				GracePeriodSeconds: &zero,
				PropagationPolicy:  &foreground,
			},
		)
		if err == nil {
			lg.Info("deleted namespace", zap.String("namespace", namespace))
		} else {
			lg.Warn("failed to delete namespace", zap.String("namespace", namespace), zap.Error(err))
		}
		return err
	}
	// requires "apierrs.IsNotFound"
	// ref. https://github.com/aws/aws-k8s-tester/issues/79
	return RetryWithExponentialBackOff(RetryFunction(deleteFunc, Allow(apierrs.IsNotFound)))
}

// DeleteNamespaceAndWait deletes namespace with given name and waits for its deletion.
// Default interval is 5-second and default timeout is 10-min.
func DeleteNamespaceAndWait(lg *zap.Logger, c clientset.Interface, namespace string, interval time.Duration, timeout time.Duration) error {
	if err := DeleteNamespace(lg, c, namespace); err != nil {
		return err
	}
	return waitForDeleteNamespace(lg, c, namespace, interval, timeout)
}

// WaitForDeleteNamespace waits untils namespace is terminated.
func WaitForDeleteNamespace(lg *zap.Logger, c clientset.Interface, namespace string) error {
	return waitForDeleteNamespace(lg, c, namespace, DefaultNamespaceDeletionInterval, DefaultNamespaceDeletionTimeout)
}

func waitForDeleteNamespace(lg *zap.Logger, c clientset.Interface, namespace string, interval time.Duration, timeout time.Duration) error {
	if interval == 0 {
		interval = DefaultNamespaceDeletionInterval
	}
	if timeout == 0 {
		timeout = DefaultNamespaceDeletionTimeout
	}
	retryWaitFunc := func() (done bool, err error) {
		lg.Info("waiting for namespace deletion", zap.String("namespace", namespace))
		_, err = c.CoreV1().Namespaces().Get(namespace, metav1.GetOptions{})
		if err != nil {
			if apierrs.IsNotFound(err) {
				lg.Info("namespace already deleted", zap.String("namespace", namespace))
				return true, nil
			}
			lg.Warn("failed to get namespace", zap.String("namespace", namespace), zap.Error(err))
			if strings.Contains(err.Error(), "i/o timeout") {
				return false, nil
			}
			if !IsRetryableAPIError(err) {
				return false, err
			}
		}
		lg.Info("namespace still exists", zap.String("namespace", namespace))
		return false, nil
	}
	return wait.PollImmediate(interval, timeout, retryWaitFunc)
}

// ListNamespaces returns list of existing namespace names.
func ListNamespaces(c clientset.Interface) ([]apiv1.Namespace, error) {
	var namespaces []apiv1.Namespace
	listFunc := func() error {
		namespacesList, err := c.CoreV1().Namespaces().List(metav1.ListOptions{})
		if err != nil {
			return err
		}
		namespaces = namespacesList.Items
		return nil
	}
	if err := RetryWithExponentialBackOff(RetryFunction(listFunc)); err != nil {
		return namespaces, err
	}
	return namespaces, nil
}

// ListEvents retrieves events for the object with the given name.
func ListEvents(c clientset.Interface, namespace string, name string, options ...*ApiCallOptions) (obj *apiv1.EventList, err error) {
	getFunc := func() error {
		obj, err = c.CoreV1().Events(namespace).List(metav1.ListOptions{
			FieldSelector: "involvedObject.name=" + name,
		})
		return err
	}
	if err := RetryWithExponentialBackOff(RetryFunction(getFunc, options...)); err != nil {
		return nil, err
	}
	return obj, nil
}

// CreateObject creates object based on given object description.
func CreateObject(dynamicClient dynamic.Interface, namespace string, name string, obj *unstructured.Unstructured, options ...*ApiCallOptions) error {
	gvk := obj.GroupVersionKind()
	gvr, _ := meta.UnsafeGuessKindToResource(gvk)
	obj.SetName(name)
	createFunc := func() error {
		_, err := dynamicClient.Resource(gvr).Namespace(namespace).Create(obj, metav1.CreateOptions{})
		return err
	}
	options = append(options, Allow(apierrs.IsAlreadyExists))
	return RetryWithExponentialBackOff(RetryFunction(createFunc, options...))
}
