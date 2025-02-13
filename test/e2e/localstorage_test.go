package e2e

import (
	goctx "context"
	"fmt"
	"os"
	"regexp"
	"testing"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	localv1 "github.com/openshift/local-storage-operator/pkg/apis/local/v1"
	commontypes "github.com/openshift/local-storage-operator/pkg/common"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"github.com/operator-framework/operator-sdk/pkg/test/e2eutil"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	dynclient "sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	retryInterval        = time.Second * 5
	timeout              = time.Second * 120
	cleanupRetryInterval = time.Second * 1
	cleanupTimeout       = time.Second * 5

	awsEBSNitroRegex  = "^[cmr]5.*|t3|z1d"
	labelInstanceType = "beta.kubernetes.io/instance-type"
)

func addToScheme(s *runtime.Scheme) error {
	return localv1.AddToScheme(s)
}

func TestLocalStorageOperator(t *testing.T) {
	localVolumeList := &localv1.LocalVolumeList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "LocalVolume",
			APIVersion: localv1.SchemeGroupVersion.String(),
		},
	}
	err := framework.AddToFrameworkScheme(addToScheme, localVolumeList)
	if err != nil {
		t.Fatalf("error adding local volume list : %v", err)
	}

	ctx := framework.NewTestCtx(t)
	defer ctx.Cleanup()

	err = waitForOperatorToBeReady(t, ctx)
	if err != nil {
		t.Fatalf("error waiting for operator to be ready : %v", err)
	}

	f := framework.Global
	namespace, err := ctx.GetNamespace()
	if err != nil {
		t.Fatalf("error fetching namespace : %v", err)
	}

	selectedNode := selectNode(t, f.KubeClient)
	selectedDisk, _ := selectDisk(f.KubeClient, selectedNode)
	waitForPVCreation := true
	if selectedDisk == "" {
		waitForPVCreation = false
		selectedDisk = "/dev/foobar"
	}
	localVolume := &localv1.LocalVolume{
		TypeMeta: metav1.TypeMeta{
			Kind:       "LocalVolume",
			APIVersion: localv1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-local-disk",
			Namespace: namespace,
		},
		Spec: localv1.LocalVolumeSpec{
			NodeSelector: &v1.NodeSelector{
				NodeSelectorTerms: []v1.NodeSelectorTerm{
					{
						MatchFields: []v1.NodeSelectorRequirement{
							{Key: "metadata.name", Operator: v1.NodeSelectorOpIn, Values: []string{selectedNode.Name}},
						},
					},
				},
			},
			StorageClassDevices: []localv1.StorageClassDevice{
				{
					StorageClassName: "test-local-sc",
					DevicePaths:      []string{selectedDisk},
				},
			},
		},
	}

	err = f.Client.Create(goctx.TODO(), localVolume, &framework.CleanupOptions{TestContext: ctx, Timeout: cleanupTimeout, RetryInterval: cleanupRetryInterval})
	if err != nil {
		t.Fatalf("error creating localvolume cr : %v", err)
	}

	provisionerDSName := localVolume.Name + "-local-provisioner"
	diskMakerDSName := localVolume.Name + "-local-diskmaker"

	err = waitForDaemonSet(t, f.KubeClient, namespace, provisionerDSName, retryInterval, timeout)
	if err != nil {
		t.Fatalf("error waiting for provisioner daemonset : %v", err)
	}

	err = waitForDaemonSet(t, f.KubeClient, namespace, diskMakerDSName, retryInterval, timeout)
	if err != nil {
		t.Fatalf("error waiting for diskmaker daemonset : %v", err)
	}

	err = verifyLocalVolume(localVolume, f.Client)
	if err != nil {
		t.Fatalf("error verifying localvolume cr: %v", err)
	}

	err = checkLocalVolumeStatus(localVolume)
	if err != nil {
		t.Fatalf("error checking localvolume condition: %v", err)
	}

	if waitForPVCreation {
		err = waitForCreatedPV(f.KubeClient, localVolume)
		if err != nil {
			t.Fatalf("error waiting for creation of pv: %v", err)
		}
		err = deleteCreatedPV(f.KubeClient, localVolume)
		if err != nil {
			t.Errorf("error deleting created PV: %v", err)
		}
	}
}

func verifyLocalVolume(lv *localv1.LocalVolume, client framework.FrameworkClient) error {
	waitErr := wait.PollImmediate(retryInterval, timeout, func() (bool, error) {
		objectKey := dynclient.ObjectKey{
			Namespace: lv.Namespace,
			Name:      lv.Name,
		}
		err := client.Get(goctx.TODO(), objectKey, lv)
		if err != nil {
			return false, err
		}
		finaliers := lv.GetFinalizers()
		if len(finaliers) == 0 {
			return false, nil
		}
		return true, nil
	})
	return waitErr
}

func checkLocalVolumeStatus(lv *localv1.LocalVolume) error {
	localVolumeConditions := lv.Status.Conditions
	if len(localVolumeConditions) == 0 {
		return fmt.Errorf("expected local volume to have conditions")
	}

	c := localVolumeConditions[0]
	if c.Type != operatorv1.OperatorStatusTypeAvailable || c.Status != operatorv1.ConditionTrue {
		return fmt.Errorf("expected available operator condition got %v", localVolumeConditions)
	}

	if c.LastTransitionTime.IsZero() {
		return fmt.Errorf("expect last transition time to be set")
	}
	return nil
}

func deleteCreatedPV(kubeClient kubernetes.Interface, lv *localv1.LocalVolume) error {
	err := kubeClient.Core().PersistentVolumes().DeleteCollection(nil, metav1.ListOptions{LabelSelector: commontypes.GetPVOwnerSelector(lv).String()})
	return err
}

func waitForCreatedPV(kubeClient kubernetes.Interface, lv *localv1.LocalVolume) error {
	waitErr := wait.PollImmediate(retryInterval, timeout, func() (bool, error) {
		pvs, err := kubeClient.Core().PersistentVolumes().List(metav1.ListOptions{LabelSelector: commontypes.GetPVOwnerSelector(lv).String()})
		if err != nil {
			if isRetryableAPIError(err) {
				return false, nil
			}
			return false, err
		}
		if len(pvs.Items) > 0 {
			return true, nil
		}
		return false, nil
	})
	return waitErr
}

func selectNode(t *testing.T, kubeClient kubernetes.Interface) v1.Node {
	nodes, err := kubeClient.Core().Nodes().List(metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/worker"})
	var dummyNode v1.Node
	if err != nil {
		t.Fatalf("error finding worker node with %v", err)
	}

	if len(nodes.Items) != 0 {
		return nodes.Items[0]
	}
	nodeList, err := waitListSchedulableNodes(kubeClient)
	if err != nil {
		t.Fatalf("error listing schedulable nodes : %v", err)
	}
	if len(nodeList.Items) != 0 {
		return nodeList.Items[0]
	}
	t.Fatalf("found no schedulable node")
	return dummyNode
}

func selectDisk(kubeClient kubernetes.Interface, node v1.Node) (string, error) {
	var nodeInstanceType string
	for k, v := range node.ObjectMeta.Labels {
		if k == labelInstanceType {
			nodeInstanceType = v
		}
	}
	if ok, _ := regexp.MatchString(awsEBSNitroRegex, nodeInstanceType); ok {
		return getNitroDisk(kubeClient, node)
	}

	localDisk := os.Getenv("TEST_LOCAL_DISK")
	if localDisk != "" {
		return localDisk, nil
	}
	return "", fmt.Errorf("can not find a suitable disk")
}

func getNitroDisk(kubeClient kubernetes.Interface, node v1.Node) (string, error) {
	return "", fmt.Errorf("unimplemented")
}

func isRetryableAPIError(err error) bool {
	// These errors may indicate a transient error that we can retry in tests.
	if apierrors.IsInternalError(err) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err) || utilnet.IsProbableEOF(err) || utilnet.IsConnectionReset(err) {
		return true
	}
	// If the error sends the Retry-After header, we respect it as an explicit confirmation we should retry.
	if _, shouldRetry := apierrors.SuggestsClientDelay(err); shouldRetry {
		return true
	}
	return false
}

// waitListSchedulableNodes is a wrapper around listing nodes supporting retries.
func waitListSchedulableNodes(c kubernetes.Interface) (*v1.NodeList, error) {
	var nodes *v1.NodeList
	var err error
	if wait.PollImmediate(retryInterval, timeout, func() (bool, error) {
		nodes, err = c.CoreV1().Nodes().List(metav1.ListOptions{FieldSelector: fields.Set{
			"spec.unschedulable": "false",
		}.AsSelector().String()})
		if err != nil {
			if isRetryableAPIError(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}) != nil {
		return nodes, err
	}
	return nodes, nil
}

func waitForOperatorToBeReady(t *testing.T, ctx *framework.TestCtx) error {
	t.Log("Initializing cluster resources...")
	err := ctx.InitializeClusterResources(&framework.CleanupOptions{TestContext: ctx, Timeout: cleanupTimeout, RetryInterval: cleanupRetryInterval})
	if err != nil {
		return err
	}
	t.Log("Initialized cluster resources")
	namespace, err := ctx.GetNamespace()
	if err != nil {
		return err
	}
	t.Logf("Found namespace: %v", namespace)

	// get global framework variables
	f := framework.Global
	// wait for local-storage-operator to be ready
	t.Log("Waiting for local-storage-operator to be ready...")
	err = e2eutil.WaitForDeployment(t, f.KubeClient, namespace, "local-storage-operator", 1, retryInterval, timeout)
	if err != nil {
		return err
	}
	return nil
}

func waitForDaemonSet(t *testing.T, kubeclient kubernetes.Interface, namespace, name string, retryInterval, timeout time.Duration) error {
	nodeCount := 1
	var err error
	err = wait.Poll(retryInterval, timeout, func() (done bool, err error) {
		daemonset, err := kubeclient.AppsV1().DaemonSets(namespace).Get(name, metav1.GetOptions{IncludeUninitialized: true})
		if err != nil {
			if apierrors.IsNotFound(err) {
				t.Logf("Waiting for availability of %s daemonset\n", name)
				return false, nil
			}
			return false, err
		}
		if int(daemonset.Status.NumberReady) == nodeCount {
			return true, nil
		}
		t.Logf("Waiting for full availability of %s daemonset (%d/%d)\n", name, int(daemonset.Status.NumberReady), nodeCount)
		return false, nil
	})
	if err != nil {
		return err
	}
	t.Logf("Daemonset available (%d/%d)\n", nodeCount, nodeCount)
	return nil
}
