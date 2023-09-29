package framework

import (
	"context"
	"fmt"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"testing"
	"time"
)

// Framework is a helper struct for testing
// the cosignwebhook in a k8s cluster
type Framework struct {
	k8s *kubernetes.Clientset
}

func New() (*Framework, error) {
	k8s, err := createClientSet()
	if err != nil {
		return nil, err
	}

	return &Framework{
		k8s: k8s,
	}, nil
}

func createClientSet() (k8sClient *kubernetes.Clientset, err error) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.Getenv("HOME") + "/.kube/config"
	}

	// create restconfig from kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}

	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return cs, nil
}

// Cleanup removes all resources created by the framework
// and cleans up the testing directory
func (f *Framework) Cleanup(t testing.TB, err error) {
	cleanupKeys(t)
	f.cleanupDeployments(t)
	f.cleanupSecrets(t)
	if err != nil {
		t.Fatalf("test failed: %v", err)
	}
}

// cleanupDeployments removes all deployments from the testing namespace
// if they exist
func (f *Framework) cleanupDeployments(t testing.TB) {

	t.Logf("cleaning up deployments")
	deployments, err := f.k8s.AppsV1().Deployments("test-cases").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		f.Cleanup(t, err)
	}
	for _, d := range deployments.Items {
		err = f.k8s.AppsV1().Deployments("test-cases").Delete(context.Background(), d.Name, metav1.DeleteOptions{})
		if err != nil {
			f.Cleanup(t, err)
		}
	}

	timeout := time.After(30 * time.Second)
	for {
		select {
		case <-timeout:
			f.Cleanup(t, fmt.Errorf("timeout reached while waiting for deployments to be deleted"))
		default:
			pods, err := f.k8s.CoreV1().Pods("test-cases").List(context.Background(), metav1.ListOptions{})
			if err != nil {
				f.Cleanup(t, err)
			}

			if len(pods.Items) == 0 {
				t.Logf("All pods are deleted")
				return
			}
			time.Sleep(5 * time.Second)
		}
	}
}

// cleanupSecrets removes all secrets from the testing namespace
func (f *Framework) cleanupSecrets(t testing.TB) {

	t.Logf("cleaning up secrets")
	secrets, err := f.k8s.CoreV1().Secrets("test-cases").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		f.Cleanup(t, err)
	}
	if len(secrets.Items) == 0 {
		return
	}
	for _, s := range secrets.Items {
		err = f.k8s.CoreV1().Secrets("test-cases").Delete(context.Background(), s.Name, metav1.DeleteOptions{})
		if err != nil {
			f.Cleanup(t, err)
		}
	}
}

// CreateDeployment creates a deployment in the testing namespace
func (f *Framework) CreateDeployment(t testing.TB, d appsv1.Deployment) {
	_, err := f.k8s.AppsV1().Deployments("test-cases").Create(context.Background(), &d, metav1.CreateOptions{})
	if err != nil {
		f.Cleanup(t, err)
	}
}

// WaitForDeployment waits until the deployment is ready
func (f *Framework) WaitForDeployment(t *testing.T, d appsv1.Deployment) {

	t.Logf("waiting for deployment %s to be ready", d.Name)
	// wait until the deployment is ready
	w, err := f.k8s.AppsV1().Deployments(d.Namespace).Watch(context.Background(), metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", d.Name),
	})

	if err != nil {
		f.Cleanup(t, err)
	}

	timeout := time.After(30 * time.Second)
	for event := range w.ResultChan() {
		select {
		case <-timeout:
			f.Cleanup(t, fmt.Errorf("timeout reached while waiting for deployment to be ready"))
		default:
			deployment, ok := event.Object.(*appsv1.Deployment)
			if !ok {
				time.Sleep(5 * time.Second)
				continue
			}

			if deployment.Status.ReadyReplicas == 1 {
				t.Logf("deployment %s is ready", d.Name)
				return
			}
			time.Sleep(5 * time.Second)
		}
	}

	f.Cleanup(t, fmt.Errorf("failed to wait for deployment to be ready"))
}

// CreateSecret creates a secret in the testing namespace
func (f *Framework) CreateSecret(t *testing.T, secret corev1.Secret) {
	t.Logf("creating secret %s", secret.Name)
	s, err := f.k8s.CoreV1().Secrets("test-cases").Create(context.Background(), &secret, metav1.CreateOptions{})
	if err != nil {
		f.Cleanup(t, err)
	}
	t.Logf("created secret %s", s.Name)
}

// AssertDeploymentFailed asserts that the deployment cannot start
func (f *Framework) AssertDeploymentFailed(t *testing.T, d appsv1.Deployment) {

	t.Logf("waiting for deployment %s to fail", d.Name)

	// watch for replicasets of the deployment
	rsName, err := f.waitForReplicaSetCreation(t, d)
	if err != nil {
		f.Cleanup(t, err)
	}

	// get warning events of deployment's namespace and check if the deployment failed
	w, err := f.k8s.CoreV1().Events("test-cases").Watch(context.Background(), metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s", rsName),
	})
	if err != nil {
		f.Cleanup(t, err)
	}

	timeout := time.After(30 * time.Second)
	for event := range w.ResultChan() {
		select {
		case <-timeout:
			f.Cleanup(t, fmt.Errorf("timeout reached while waiting for deployment to fail"))
		default:
			e, ok := event.Object.(*corev1.Event)
			if !ok {
				time.Sleep(5 * time.Second)
				continue
			}
			if e.Reason == "FailedCreate" {
				t.Logf("deployment %s failed: %s", d.Name, e.Message)
				return
			}
			time.Sleep(5 * time.Second)
		}
	}
}

func (f *Framework) waitForReplicaSetCreation(t *testing.T, d appsv1.Deployment) (string, error) {
	rs, err := f.k8s.AppsV1().ReplicaSets("test-cases").Watch(context.Background(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", d.Name),
	})
	if err != nil {
		f.Cleanup(t, err)
	}

	timeout := time.After(30 * time.Second)
	for event := range rs.ResultChan() {
		select {
		case <-timeout:
			return "", fmt.Errorf("timeout reached while waiting for replicaset to be created")
		default:
			rs, ok := event.Object.(*appsv1.ReplicaSet)
			if ok {
				t.Logf("replicaset %s created", rs.Name)
				return rs.Name, nil
			}
			time.Sleep(5 * time.Second)
		}
	}
	return "", fmt.Errorf("failed to wait for replicaset creation")
}
