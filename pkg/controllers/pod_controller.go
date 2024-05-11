/*
Copyright 2023.

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

package controllers

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	acceleratorutils "sigs.k8s.io/lws/pkg/utils/accelerators"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
	statefulsetutils "sigs.k8s.io/lws/pkg/utils/statefulset"
)

// PodReconciler reconciles a LeaderWorkerSet object
type PodReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func NewPodReconciler(client client.Client, schema *runtime.Scheme) *PodReconciler {
	return &PodReconciler{Client: client, Scheme: schema}
}

//+kubebuilder:rbac:groups=core,resources=pods,verbs=create;delete;get;list;patch;update;watch
//+kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;update;patch

func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Name: req.Name, Namespace: req.Namespace}, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log := ctrl.LoggerFrom(ctx).WithValues("pod", klog.KObj(&pod))
	ctx = ctrl.LoggerInto(ctx, log)

	// get the leaderWorkerSet name
	lwsName := pod.Labels[leaderworkerset.SetNameLabelKey]
	if lwsName == "" {
		return ctrl.Result{}, errors.New("leaderworkerset.sigs.k8s.io/name label is unexpected missing")
	}
	if _, exist := pod.Labels[leaderworkerset.WorkerIndexLabelKey]; !exist {
		return ctrl.Result{}, errors.New("leaderworkerset.sigs.k8s.io/worker-index label is unexpected missing")
	}
	// get the leaderWorkerSet object
	var leaderWorkerSet leaderworkerset.LeaderWorkerSet
	if err := r.Get(ctx, types.NamespacedName{Name: lwsName, Namespace: pod.Namespace}, &leaderWorkerSet); err != nil {
		// If lws not found, it's mostly because deleted, ignore the error as Pods will be GCed finally.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	leaderDeleted, err := r.handleRestartPolicy(ctx, pod, leaderWorkerSet)
	if err != nil {
		return ctrl.Result{}, err
	}
	if leaderDeleted {
		log.V(2).Info("restarting the group")
		return ctrl.Result{}, nil
	}

	// worker pods' reconciliation is only done to handle restart policy
	if !podutils.LeaderPod(pod) {
		return ctrl.Result{}, nil
	}

	// if it's not leader pod or leader pod is being deleted, we should not create the worker statefulset
	// this is critical to avoid race condition in all-or-nothing restart where the worker sts may be created
	// when the leader pod is being deleted
	if pod.DeletionTimestamp != nil {
		log.V(2).Info("skip creating the worker sts since the leader pod is being deleted")
		return ctrl.Result{}, nil
	}

	// logic for handling leader pod
	// workerStsReplicas := *leaderWorkerSet.Spec.LeaderWorkerTemplate.Size - 1
	if leaderWorkerSet.Spec.StartupPolicy == leaderworkerset.WaitForLeaderReady {
		var leaderSts appsv1.StatefulSet
		if err = r.Get(ctx, types.NamespacedName{Name: lwsName, Namespace: pod.Namespace}, &leaderSts); err != nil {
			return ctrl.Result{}, err
		}
		if leaderSts.Status.ReadyReplicas != *leaderSts.Spec.Replicas {
			// workerStsReplicas = 0
			// create the workers sts only after the leader is ready
			return ctrl.Result{}, nil
		}
	}

	statefulSet, err := constructWorkerStatefulSetApplyConfiguration(pod, leaderWorkerSet)
	if err != nil {
		return ctrl.Result{}, err
	}

	// if exclusive placement is enabled but leader pod is not scheduled, don't create the worker sts
	if topologyKey, found := leaderWorkerSet.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]; found {
		// check if the leader pod is scheduled.
		if pod.Spec.NodeName == "" {
			log.V(2).Info(fmt.Sprintf("Pod %q is not scheduled yet", pod.Name))
			return ctrl.Result{}, nil
		}
		if err := r.setNodeSelectorForWorkerPods(ctx, &pod, statefulSet, topologyKey); err != nil {
			log.Error(err, "setting node selector for worker pods")
			return ctrl.Result{}, err
		}
	}

	if err := setControllerReferenceWithStatefulSet(&pod, statefulSet, r.Scheme); err != nil {
		log.Error(err, "Setting controller reference.")
		return ctrl.Result{}, nil
	}

	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(statefulSet)
	if err != nil {
		return ctrl.Result{}, err
	}
	patch := &unstructured.Unstructured{
		Object: obj,
	}
	// Use server side apply and add fieldmanagaer to the lws owned fields
	// If there are conflicts in the fields owned by the lws controller, lws will obtain the ownership and force override
	// these fields to the ones desired by the lws controller. These fields are specified in the StatefulSetApplyConfiguration
	// TODO b/316776287 add E2E test for SSA
	err = r.Patch(ctx, patch, client.Apply, &client.PatchOptions{
		FieldManager: fieldManager,
		Force:        ptr.To[bool](true),
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	log.V(2).Info("Worker Reconcile completed.")
	return ctrl.Result{}, nil
}

func (r *PodReconciler) handleRestartPolicy(ctx context.Context, pod corev1.Pod, leaderWorkerSet leaderworkerset.LeaderWorkerSet) (bool, error) {
	if leaderWorkerSet.Spec.LeaderWorkerTemplate.RestartPolicy != leaderworkerset.RecreateGroupOnPodRestart {
		return false, nil
	}
	// the leader pod will be deleted if the worker pod is deleted or any containes were restarted
	if !podutils.ContainerRestarted(pod) && !podutils.PodDeleted(pod) {
		return false, nil
	}
	var leader corev1.Pod
	if !podutils.LeaderPod(pod) {
		leaderPodName, ordinal := statefulsetutils.GetParentNameAndOrdinal(pod.Name)
		if ordinal == -1 {
			return false, fmt.Errorf("parsing pod name for pod %s", pod.Name)
		}
		if err := r.Get(ctx, types.NamespacedName{Name: leaderPodName, Namespace: pod.Namespace}, &leader); err != nil {
			return false, err
		}
	} else {
		leader = pod
	}
	// if the leader pod is being deleted, we don't need to send deletion requests
	if leader.DeletionTimestamp != nil {
		return true, nil
	}
	deletionOpt := metav1.DeletePropagationForeground
	if err := r.Delete(ctx, &leader, &client.DeleteOptions{
		PropagationPolicy: &deletionOpt,
	}); err != nil {
		return false, err
	}
	return true, nil
}

func (r *PodReconciler) setNodeSelectorForWorkerPods(ctx context.Context, pod *corev1.Pod, sts *appsapplyv1.StatefulSetApplyConfiguration, topologyKey string) error {

	log := ctrl.LoggerFrom(ctx)
	topologyValue, err := r.topologyValueFromPod(ctx, pod, topologyKey)
	if err != nil {
		log.Error(err, "getting topology from leader pod")
		return err
	}

	// set node selector for worker pods, if worker pods already scheduled to different topology value
	// the following applying logic will automatically update it to match the leader pods, so we don't
	// need to verify if they have the same topology value
	sts.Spec.Template.Spec.WithNodeSelector(map[string]string{
		topologyKey: topologyValue,
	})
	return nil
}

func (r *PodReconciler) topologyValueFromPod(ctx context.Context, pod *corev1.Pod, topologyKey string) (string, error) {
	log := ctrl.LoggerFrom(ctx)

	nodeName := pod.Spec.NodeName
	ns := pod.Namespace

	// Get node the leader pod is running on.
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &node); err != nil {
		// We'll ignore not-found errors, since there is nothing we can do here.
		// A node may not exist temporarily due to a maintenance event or other scenarios.
		log.Error(err, fmt.Sprintf("getting node %s", nodeName))
		return "", client.IgnoreNotFound(err)
	}

	// Get topology (e.g. node pool name) from node labels.
	topology, exists := node.Labels[topologyKey]
	if !exists {
		return "", fmt.Errorf("node does not have topology label: %s", topology)
	}
	return topology, nil
}

// setControllerReferenceWithStatefulSet set controller reference for the StatefulSet
func setControllerReferenceWithStatefulSet(owner metav1.Object, sts *appsapplyv1.StatefulSetApplyConfiguration, scheme *runtime.Scheme) error {
	// Validate the owner.
	ro, ok := owner.(runtime.Object)
	if !ok {
		return fmt.Errorf("%T is not a runtime.Object, cannot call SetOwnerReference", owner)
	}
	gvk, err := apiutil.GVKForObject(ro, scheme)
	if err != nil {
		return err
	}
	sts.WithOwnerReferences(metaapplyv1.OwnerReference().
		WithAPIVersion(gvk.GroupVersion().String()).
		WithKind(gvk.Kind).
		WithName(owner.GetName()).
		WithUID(owner.GetUID()).
		WithBlockOwnerDeletion(true).
		WithController(true))
	return nil
}

// constructWorkerStatefulSetApplyConfiguration constructs the applied configuration for the leader StatefulSet
func constructWorkerStatefulSetApplyConfiguration(leaderPod corev1.Pod, lws leaderworkerset.LeaderWorkerSet) (*appsapplyv1.StatefulSetApplyConfiguration, error) {
	podTemplateSpec := *lws.Spec.LeaderWorkerTemplate.WorkerTemplate.DeepCopy()
	// construct pod template spec configuration
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&podTemplateSpec)
	if err != nil {
		return nil, err
	}
	var podTemplateApplyConfiguration coreapplyv1.PodTemplateSpecApplyConfiguration
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &podTemplateApplyConfiguration)
	if err != nil {
		return nil, err
	}
	selectorMap := map[string]string{
		leaderworkerset.GroupIndexLabelKey:      leaderPod.Labels[leaderworkerset.GroupIndexLabelKey],
		leaderworkerset.SetNameLabelKey:         lws.Name,
		leaderworkerset.GroupUniqueHashLabelKey: leaderPod.Labels[leaderworkerset.GroupUniqueHashLabelKey],
	}
	labelMap := map[string]string{
		leaderworkerset.GroupIndexLabelKey:      leaderPod.Labels[leaderworkerset.GroupIndexLabelKey],
		leaderworkerset.SetNameLabelKey:         lws.Name,
		leaderworkerset.GroupUniqueHashLabelKey: leaderPod.Labels[leaderworkerset.GroupUniqueHashLabelKey],
		leaderworkerset.TemplateRevisionHashKey: leaderPod.Labels[leaderworkerset.TemplateRevisionHashKey],
	}

	podTemplateApplyConfiguration.WithLabels(labelMap)
	podAnnotations := make(map[string]string)
	podAnnotations[leaderworkerset.SizeAnnotationKey] = strconv.Itoa(int(*lws.Spec.LeaderWorkerTemplate.Size))
	podAnnotations[leaderworkerset.LeaderPodNameAnnotationKey] = leaderPod.Name
	if lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] != "" {
		podAnnotations[leaderworkerset.ExclusiveKeyAnnotationKey] = lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]
	}
	if lws.Spec.LeaderWorkerTemplate.SubGroupPolicy != nil {
		podAnnotations[leaderworkerset.SubGroupSizeAnnotationKey] = strconv.Itoa(int(*lws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize))
		if lws.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey] != "" {
			podAnnotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey] = lws.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey]
		}
	}
	acceleratorutils.AddTPUAnnotations(leaderPod, podAnnotations)
	podTemplateApplyConfiguration.WithAnnotations(podAnnotations)
	// construct statefulset apply configuration
	statefulSetConfig := appsapplyv1.StatefulSet(leaderPod.Name, leaderPod.Namespace).
		WithSpec(appsapplyv1.StatefulSetSpec().
			WithServiceName(lws.Name).
			WithReplicas(*lws.Spec.LeaderWorkerTemplate.Size - 1).
			WithPodManagementPolicy(appsv1.ParallelPodManagement).
			WithTemplate(&podTemplateApplyConfiguration).
			WithOrdinals(appsapplyv1.StatefulSetOrdinals().WithStart(1)).
			WithSelector(metaapplyv1.LabelSelector().
				WithMatchLabels(selectorMap))).
		WithLabels(labelMap)
	return statefulSetConfig, nil
}

func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(object client.Object) bool {
			if pod, ok := object.(*corev1.Pod); ok {
				_, exist := pod.Labels[leaderworkerset.SetNameLabelKey]
				return exist
			}
			if statefulSet, ok := object.(*appsv1.StatefulSet); ok {
				_, exist := statefulSet.Labels[leaderworkerset.SetNameLabelKey]
				return exist
			}
			return false
		})).Owns(&appsv1.StatefulSet{}).Complete(r)
}
