package revision

import (
	"fmt"
	"time"

	"github.com/golang/glog"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	operatorv1 "github.com/openshift/api/operator/v1"

	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/common"
)

const operatorStatusRevisionControllerFailing = "RevisionControllerFailing"
const revisionControllerWorkQueueKey = "key"

// RevisionController is a controller that watches a set of configmaps and secrets and them against a revision snapshot
// of them. If the original resources changes, the revision counter is increased, stored in LatestAvailableRevision
// field of the operator config and new snapshots suffixed by the revision are created.
type RevisionController struct {
	targetNamespace string
	// configMaps is the list of configmaps that are directly copied.A different actor/controller modifies these.
	// the first element should be the configmap that contains the static pod manifest
	configMaps []string
	// secrets is a list of secrets that are directly copied for the current values.  A different actor/controller modifies these.
	secrets []string

	operatorConfigClient common.OperatorClient

	kubeClient kubernetes.Interface

	// queue only ever has one item, but it has nice error handling backoff/retry semantics
	queue workqueue.RateLimitingInterface

	eventRecorder events.Recorder
}

// NewRevisionController create a new revision controller.
func NewRevisionController(
	targetNamespace string,
	configMaps []string,
	secrets []string,
	kubeInformersForTargetNamespace informers.SharedInformerFactory,
	operatorConfigClient common.OperatorClient,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
) *RevisionController {
	c := &RevisionController{
		targetNamespace: targetNamespace,
		configMaps:      configMaps,
		secrets:         secrets,

		operatorConfigClient: operatorConfigClient,
		kubeClient:           kubeClient,
		eventRecorder:        eventRecorder,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "RevisionController"),
	}

	operatorConfigClient.Informer().AddEventHandler(c.eventHandler())
	kubeInformersForTargetNamespace.Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForTargetNamespace.Core().V1().Secrets().Informer().AddEventHandler(c.eventHandler())

	return c
}

// createRevisionIfNeeded takes care of creating content for the static pods to use.
// returns whether or not requeue and if an error happened when updating status.  Normally it updates status itself.
func (c RevisionController) createRevisionIfNeeded(operatorSpec *operatorv1.OperatorSpec, operatorStatusOriginal *operatorv1.StaticPodOperatorStatus, resourceVersion string) (bool, error) {
	operatorStatus := operatorStatusOriginal.DeepCopy()

	latestRevision := operatorStatus.LatestAvailableRevision
	isLatestRevisionCurrent, reason := c.isLatestRevisionCurrent(latestRevision)

	// check to make sure that the latestRevision has the exact content we expect.  No mutation here, so we start creating the next Revision only when it is required
	if isLatestRevisionCurrent {
		return false, nil
	}

	nextRevision := latestRevision + 1
	glog.Infof("new revision %d triggered by %q", nextRevision, reason)
	if err := c.createNewRevision(nextRevision); err != nil {
		cond := operatorv1.OperatorCondition{
			Type:    "RevisionControllerFailing",
			Status:  operatorv1.ConditionTrue,
			Reason:  "ContentCreationError",
			Message: err.Error(),
		}
		if _, _, updateError := common.UpdateStatus(c.operatorConfigClient, common.UpdateConditionFn(cond)); updateError != nil {
			c.eventRecorder.Warningf("RevisionCreateFailed", "Failed to create revision %d: %v", nextRevision, err.Error())
			return true, updateError
		}
		return true, nil
	}

	cond := operatorv1.OperatorCondition{
		Type:   "RevisionControllerFailing",
		Status: operatorv1.ConditionFalse,
	}
	if _, updated, updateError := common.UpdateStatus(c.operatorConfigClient, common.UpdateConditionFn(cond), func(operatorStatus *operatorv1.StaticPodOperatorStatus) error {
		operatorStatus.LatestAvailableRevision = nextRevision
		return nil
	}); updateError != nil {
		return true, updateError
	} else if updated {
		c.eventRecorder.Eventf("RevisionCreate", "Revision %d created because %s", operatorStatus.LatestAvailableRevision, reason)
	}

	return false, nil
}

func nameFor(name string, revision int32) string {
	return fmt.Sprintf("%s-%d", name, revision)
}

// isLatestRevisionCurrent returns whether the latest revision is up to date and an optional reason
func (c RevisionController) isLatestRevisionCurrent(revision int32) (bool, string) {
	for _, name := range c.configMaps {
		required, err := c.kubeClient.CoreV1().ConfigMaps(c.targetNamespace).Get(name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, err.Error()
		}
		existing, err := c.kubeClient.CoreV1().ConfigMaps(c.targetNamespace).Get(nameFor(name, revision), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, err.Error()
		}
		if !equality.Semantic.DeepEqual(existing.Data, required.Data) {
			return false, fmt.Sprintf("configmap/%s has changed", required.Name)
		}
	}
	for _, name := range c.secrets {
		required, err := c.kubeClient.CoreV1().Secrets(c.targetNamespace).Get(name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, err.Error()
		}
		existing, err := c.kubeClient.CoreV1().Secrets(c.targetNamespace).Get(nameFor(name, revision), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, err.Error()
		}
		if !equality.Semantic.DeepEqual(existing.Data, required.Data) {
			return false, fmt.Sprintf("secret/%s has changed", required.Name)
		}
	}

	return true, ""
}

func (c RevisionController) createNewRevision(revision int32) error {
	statusConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: c.targetNamespace,
			Name:      nameFor("revision-status", revision),
		},
		Data: map[string]string{
			"status":   "InProgress",
			"revision": fmt.Sprintf("%d", revision),
		},
	}
	statusConfigMap, _, err := resourceapply.ApplyConfigMap(c.kubeClient.CoreV1(), c.eventRecorder, statusConfigMap)
	if err != nil {
		return err
	}
	ownerRefs := []metav1.OwnerReference{{
		APIVersion: statusConfigMap.APIVersion,
		Kind:       statusConfigMap.Kind,
		Name:       statusConfigMap.Name,
		UID:        statusConfigMap.UID,
	}}

	for _, name := range c.configMaps {
		obj, _, err := resourceapply.SyncConfigMap(c.kubeClient.CoreV1(), c.eventRecorder, c.targetNamespace, name, c.targetNamespace, nameFor(name, revision), ownerRefs)
		if err != nil {
			return err
		}
		if obj == nil {
			return apierrors.NewNotFound(corev1.Resource("configmaps"), name)
		}
	}
	for _, name := range c.secrets {
		obj, _, err := resourceapply.SyncSecret(c.kubeClient.CoreV1(), c.eventRecorder, c.targetNamespace, name, c.targetNamespace, nameFor(name, revision), ownerRefs)
		if err != nil {
			return err
		}
		if obj == nil {
			return apierrors.NewNotFound(corev1.Resource("secrets"), name)
		}
	}

	return nil
}

func (c RevisionController) sync() error {
	operatorSpec, originalOperatorStatus, resourceVersion, err := c.operatorConfigClient.Get()
	if err != nil {
		return err
	}
	operatorStatus := originalOperatorStatus.DeepCopy()

	switch operatorSpec.ManagementState {
	case operatorv1.Unmanaged:
		return nil
	case operatorv1.Removed:
		// TODO probably just fail.  Static pod managers can't be removed.
		return nil
	}

	requeue, syncErr := c.createRevisionIfNeeded(operatorSpec, operatorStatus, resourceVersion)
	if requeue && syncErr == nil {
		return fmt.Errorf("synthetic requeue request (err: %v)", syncErr)
	}
	err = syncErr

	// update failing condition
	cond := operatorv1.OperatorCondition{
		Type:   operatorStatusRevisionControllerFailing,
		Status: operatorv1.ConditionFalse,
	}
	if err != nil {
		cond.Status = operatorv1.ConditionTrue
		cond.Reason = "Error"
		cond.Message = err.Error()
	}
	if _, _, updateError := common.UpdateStatus(c.operatorConfigClient, common.UpdateConditionFn(cond)); updateError != nil {
		if err == nil {
			return updateError
		}
	}

	return err
}

// Run starts the kube-apiserver and blocks until stopCh is closed.
func (c *RevisionController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	glog.Infof("Starting RevisionController")
	defer glog.Infof("Shutting down RevisionController")

	// doesn't matter what workers say, only start one.
	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
}

func (c *RevisionController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *RevisionController) processNextWorkItem() bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	err := c.sync()
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

// eventHandler queues the operator to check spec and status
func (c *RevisionController) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(revisionControllerWorkQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(revisionControllerWorkQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(revisionControllerWorkQueueKey) },
	}
}
