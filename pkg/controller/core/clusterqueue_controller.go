/*
Copyright 2022 The Kubernetes Authors.

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

package core

import (
	"context"
	"fmt"
	"reflect"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	"sigs.k8s.io/kueue/pkg/cache"
	"sigs.k8s.io/kueue/pkg/constants"
	"sigs.k8s.io/kueue/pkg/queue"
)

const wlUpdateChBuffer = 10

// ClusterQueueReconciler reconciles a ClusterQueue object
type ClusterQueueReconciler struct {
	client     client.Client
	log        logr.Logger
	qManager   *queue.Manager
	cache      *cache.Cache
	wlUpdateCh chan event.GenericEvent
}

func NewClusterQueueReconciler(client client.Client, qMgr *queue.Manager, cache *cache.Cache) *ClusterQueueReconciler {
	return &ClusterQueueReconciler{
		client:     client,
		log:        ctrl.Log.WithName("cluster-queue-reconciler"),
		qManager:   qMgr,
		cache:      cache,
		wlUpdateCh: make(chan event.GenericEvent, wlUpdateChBuffer),
	}
}

//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;watch;update
//+kubebuilder:rbac:groups=kueue.x-k8s.io,resources=clusterqueues,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kueue.x-k8s.io,resources=clusterqueues/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kueue.x-k8s.io,resources=clusterqueues/finalizers,verbs=update

func (r *ClusterQueueReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cqObj kueue.ClusterQueue
	if err := r.client.Get(ctx, req.NamespacedName, &cqObj); err != nil {
		// we'll ignore not-found errors, since there is nothing to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log := ctrl.LoggerFrom(ctx).WithValues("clusterQueue", klog.KObj(&cqObj))
	ctx = ctrl.LoggerInto(ctx, log)
	log.V(2).Info("Reconciling ClusterQueue")

	status, err := r.Status(&cqObj)
	if err != nil {
		log.Error(err, "Failed getting status from cache")
		return ctrl.Result{}, err
	}

	if !equality.Semantic.DeepEqual(status, cqObj.Status) {
		cqObj.Status = status
		err := r.client.Status().Update(ctx, &cqObj)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	return ctrl.Result{}, nil
}

func (r *ClusterQueueReconciler) NotifyWorkloadUpdate(w *kueue.Workload) {
	r.wlUpdateCh <- event.GenericEvent{Object: w}
}

// Event handlers return true to signal the controller to reconcile the
// ClusterQueue associated with the event.

func (r *ClusterQueueReconciler) Create(e event.CreateEvent) bool {
	cq, match := e.Object.(*kueue.ClusterQueue)
	if !match {
		// No need to interact with the cache for other objects.
		return true
	}
	log := r.log.WithValues("clusterQueue", klog.KObj(cq))
	log.V(2).Info("ClusterQueue create event")
	ctx := ctrl.LoggerInto(context.Background(), log)

	if err := r.updateReferences(&kueue.ClusterQueue{}, cq, log); err != nil {
		log.Error(err, "Failed to update resource flavor reference")
		return false
	}

	if err := r.cache.AddClusterQueue(ctx, cq); err != nil {
		log.Error(err, "Failed to add clusterQueue to cache")
	}

	if err := r.qManager.AddClusterQueue(ctx, cq); err != nil {
		log.Error(err, "Failed to add clusterQueue to queue manager")
	}
	return true
}

func (r *ClusterQueueReconciler) Delete(e event.DeleteEvent) bool {
	cq, match := e.Object.(*kueue.ClusterQueue)
	if !match {
		// No need to interact with the cache for other objects.
		return true
	}

	log := r.log.WithValues("clusterQueue", klog.KObj(cq))
	log.V(2).Info("ClusterQueue delete event")
	newCq := cq.DeepCopy()
	newCq.Spec = kueue.ClusterQueueSpec{}
	if err := r.updateReferences(cq, newCq, log); err != nil {
		r.log.Error(err, "Fail to remove resource flavor reference")
		return false
	}
	r.cache.DeleteClusterQueue(cq)
	r.qManager.DeleteClusterQueue(cq)
	return true
}

func (r *ClusterQueueReconciler) Update(e event.UpdateEvent) bool {
	cq, match := e.ObjectNew.(*kueue.ClusterQueue)
	if !match {
		// No need to interact with the cache for other objects.
		return true
	}
	log := r.log.WithValues("clusterQueue", klog.KObj(cq))
	log.V(2).Info("ClusterQueue update event")

	// Only catch resource updates.
	oldCQ, match := e.ObjectOld.(*kueue.ClusterQueue)
	if match && !reflect.DeepEqual(oldCQ.Spec.Resources, cq.Spec.Resources) {
		if err := r.updateReferences(oldCQ, cq, log); err != nil {
			log.Error(err, "Fail to update resource flavor reference")
			return false
		}
	}

	if err := r.cache.UpdateClusterQueue(cq); err != nil {
		log.Error(err, "Failed to update clusterQueue in cache")
	}
	if err := r.qManager.UpdateClusterQueue(cq); err != nil {
		log.Error(err, "Failed to update clusterQueue in queue manager")
	}
	return true
}

func (r *ClusterQueueReconciler) updateReferences(oldCQ *kueue.ClusterQueue, cq *kueue.ClusterQueue, log logr.Logger) error {
	oldFlavors := make(map[string]string)
	for _, res := range oldCQ.Spec.Resources {
		for _, f := range res.Flavors {
			oldFlavors[string(f.Name)] = ""
		}
	}
	newFlavors := make(map[string]string)
	for _, res := range cq.Spec.Resources {
		for _, f := range res.Flavors {
			newFlavors[string(f.Name)] = ""
		}
	}

	needRemove := make(map[string]string)
	for k := range oldFlavors {
		if _, ok := newFlavors[k]; !ok {
			needRemove[k] = ""
		}
	}
	needAdd := make(map[string]string)
	for k := range newFlavors {
		if _, ok := oldFlavors[k]; !ok {
			needAdd[k] = ""
		}
	}

	if err := r.updateResourceFlavorReferences(cq, needRemove, true, log); err != nil {
		return err
	}
	if err := r.updateResourceFlavorReferences(cq, needAdd, false, log); err != nil {
		return err
	}

	return nil
}

func (r *ClusterQueueReconciler) updateResourceFlavorReferences(cq *kueue.ClusterQueue, objs map[string]string, deletion bool, log logr.Logger) error {
	var resourceFlavors kueue.ResourceFlavorList
	if err := r.client.List(context.TODO(), &resourceFlavors); err != nil {
		return err
	}

	rfCache := make(map[string]*kueue.ResourceFlavor)
	for i, rf := range resourceFlavors.Items {
		rfCache[rf.Name] = &resourceFlavors.Items[i]
	}

	for k := range objs {
		if rf, ok := rfCache[k]; ok {
			if deletion {
				delete(rf.ClusterQueues, kueue.ClusterQueueReference(cq.Name))
			} else {
				if rf.ClusterQueues == nil {
					rf.ClusterQueues = make(map[kueue.ClusterQueueReference]string, 0)
				}
				rf.ClusterQueues[kueue.ClusterQueueReference(cq.Name)] = ""
			}
			if err := r.client.Update(context.TODO(), rf); err != nil {
				log.Error(err, "Fail to update resource flavor reference")
			}
		} else {
			log.Error(fmt.Errorf("resource falvor %s does not exit", k), "Cannot find resource flavor")
		}
	}

	return nil
}

func (r *ClusterQueueReconciler) Generic(e event.GenericEvent) bool {
	r.log.V(3).Info("Got Workload event", "workload", klog.KObj(e.Object))
	return true
}

// cqWorkloadHandler signals the controller to reconcile the ClusterQueue
// associated to the workload in the event.
// Since the events come from a channel Source, only the Generic handler will
// receive events.
type cqWorkloadHandler struct {
	qManager *queue.Manager
}

func (h *cqWorkloadHandler) Create(event.CreateEvent, workqueue.RateLimitingInterface) {
}

func (h *cqWorkloadHandler) Update(event.UpdateEvent, workqueue.RateLimitingInterface) {
}

func (h *cqWorkloadHandler) Delete(event.DeleteEvent, workqueue.RateLimitingInterface) {
}

func (h *cqWorkloadHandler) Generic(e event.GenericEvent, q workqueue.RateLimitingInterface) {
	w := e.Object.(*kueue.Workload)
	req := h.requestForWorkloadClusterQueue(w)
	if req != nil {
		q.AddAfter(*req, constants.UpdatesBatchPeriod)
	}
}

func (h *cqWorkloadHandler) requestForWorkloadClusterQueue(w *kueue.Workload) *reconcile.Request {
	var name string
	if w.Spec.Admission != nil {
		name = string(w.Spec.Admission.ClusterQueue)
	} else {
		var ok bool
		name, ok = h.qManager.ClusterQueueForWorkload(w)
		if !ok {
			return nil
		}
	}
	return &reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name: name,
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterQueueReconciler) SetupWithManager(mgr ctrl.Manager) error {
	wHandler := cqWorkloadHandler{
		qManager: r.qManager,
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&kueue.ClusterQueue{}).
		Watches(&source.Channel{Source: r.wlUpdateCh}, &wHandler).
		WithEventFilter(r).
		Complete(r)
}

func (r *ClusterQueueReconciler) Status(cq *kueue.ClusterQueue) (kueue.ClusterQueueStatus, error) {
	usage, workloads, err := r.cache.Usage(cq)
	if err != nil {
		r.log.Error(err, "Failed getting usage from cache")
		// This is likely because the cluster queue was recently removed,
		// but we didn't process that event yet.
		return kueue.ClusterQueueStatus{}, err
	}

	return kueue.ClusterQueueStatus{
		UsedResources:     usage,
		AdmittedWorkloads: int32(workloads),
		PendingWorkloads:  r.qManager.Pending(cq),
	}, nil
}
