package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const (
	AnnotationEnabled          = "og.3istor.com/enabled"
	AnnotationSleepAt          = "og.3istor.com/sleep-at"
	AnnotationWakeAt           = "og.3istor.com/wake-at"
	AnnotationOriginalReplicas = "og.3istor.com/original-replicas"
	AnnotationOverride         = "og.3istor.com/override"
)

type Client struct {
	ClientSet *kubernetes.Clientset
}

// InitClient connects OG to your K3s cluster (local or InCluster)
func InitClient() (*Client, error) {
	var config *rest.Config
	var err error

	// Attempt in-cluster config first (when running inside a Pod)
	config, err = rest.InClusterConfig()
	if err != nil {
		// Fallback to local kubeconfig for out-of-cluster development
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = filepath.Join(homedir.HomeDir(), ".kube", "config")
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("unable to load Kubernetes config: %v", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return &Client{ClientSet: clientset}, nil
}

// ScaleApp performs scaling operations and updates annotations
func (c *Client) ScaleApp(ctx context.Context, namespace, deploymentName string, action string) error {
	deploymentsClient := c.ClientSet.AppsV1().Deployments(namespace)

	// 1. Retrieve the current deployment
	deploy, err := deploymentsClient.Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	currentReplicas := *deploy.Spec.Replicas
	annotations := deploy.Annotations
	if annotations == nil {
		annotations = make(map[string]string)
	}

	var targetReplicas int32

	if action == "sleep" {
		if currentReplicas == 0 {
			return nil // Already sleeping
		}
		// Save current replica count before scaling down
		annotations[AnnotationOriginalReplicas] = fmt.Sprintf("%d", currentReplicas)
		annotations[AnnotationOverride] = "sleep"
		targetReplicas = 0

	} else if action == "wake" {
		if currentReplicas > 0 {
			return nil // Already awake
		}
		// Restore original replica count, fallback to 1
		targetReplicas = 1
		if val, exists := annotations[AnnotationOriginalReplicas]; exists {
			fmt.Sscanf(val, "%d", &targetReplicas)
			if targetReplicas == 0 {
				targetReplicas = 1 // Safety fallback
			}
		}
		annotations[AnnotationOverride] = "wake"
	}

	// 2. Prepare the JSON merge patch
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": annotations,
		},
		"spec": map[string]interface{}{
			"replicas": targetReplicas,
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	// 3. Apply the patch atomically
	_, err = deploymentsClient.Patch(ctx, deploymentName, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to patch deployment %s/%s: %v", namespace, deploymentName, err)
	}

	fmt.Printf("[K8S] Scaled %s/%s to %d replicas (Action: %s)\n", namespace, deploymentName, targetReplicas, action)
	return nil
}