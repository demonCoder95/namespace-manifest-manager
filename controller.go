/*
Copyright 2017 The Kubernetes Authors.

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

package main

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/time/rate"

	corev1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const controllerAgentName = "namespace-manager-controller"

const (
	// SuccessSynced is used as part of the Event 'reason' when a Namespace is synced
	SuccessSynced = "Synced"

	// MessageResourceSynced is the message used for an Event fired when a Namespace
	// is synced successfully
	MessageNamespaceSynced = "Namespace synced successfully"
)

// ControllerConfig is the configuration for the controller
type ControllerConfig struct {
	ExcludedNamespaces []string
}

// Controller is the controller implementation for the namespace-manifest-manager
type Controller struct {
	// Configuration for the controller
	config *ControllerConfig
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface

	namespacesLister corelisters.NamespaceLister
	namespacesSynced cache.InformerSynced

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.TypedRateLimitingInterface[cache.ObjectName]
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder
}

// NewController returns a new namespace-manifest-manager controller
func NewController(
	ctx context.Context,
	config *ControllerConfig,
	kubeclientset kubernetes.Interface,
	namespaceInformer coreinformers.NamespaceInformer) *Controller {
	logger := klog.FromContext(ctx)

	// Create event broadcaster
	logger.V(4).Info("Creating event broadcaster")

	eventBroadcaster := record.NewBroadcaster(record.WithContext(ctx))
	eventBroadcaster.StartStructuredLogging(0)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})
	ratelimiter := workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[cache.ObjectName](5*time.Millisecond, 1000*time.Second),
		&workqueue.TypedBucketRateLimiter[cache.ObjectName]{Limiter: rate.NewLimiter(rate.Limit(50), 300)},
	)

	controller := &Controller{
		kubeclientset:    kubeclientset,
		namespacesLister: namespaceInformer.Lister(),
		namespacesSynced: namespaceInformer.Informer().HasSynced,
		workqueue:        workqueue.NewTypedRateLimitingQueue(ratelimiter),
		recorder:         recorder,
	}

	logger.Info("Setting up event handlers")
	// Set up an event handler for when Namespace resources change.
	namespaceInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		// Add a filter to the informer so we can ignore the excluded namespaces
		FilterFunc: func(obj interface{}) bool {
			namespace := obj.(*corev1.Namespace)
			for _, excludedNamespace := range config.ExcludedNamespaces {
				if namespace.Name == excludedNamespace {
					logger.Info("Ignoring excluded namespace", "namespace", namespace.Name)
					return false
				}
			}
			return true
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: controller.enqueueNamespace,
		},
	})

	return controller
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()
	logger := klog.FromContext(ctx)
	logger.Info("Starting namespace-manifest-manager controller")

	// Wait for the caches to be synced before starting workers
	logger.Info("Waiting for informer caches to sync")

	if ok := cache.WaitForCacheSync(ctx.Done(), c.namespacesSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	logger.Info("Starting workers", "count", workers)
	// Launch two workers to process Namespace resources
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	logger.Info("Started workers")
	<-ctx.Done()
	logger.Info("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	objRef, shutdown := c.workqueue.Get()
	logger := klog.FromContext(ctx)

	if shutdown {
		return false
	}

	// We call Done at the end of this func so the workqueue knows we have
	// finished processing this item. We also must remember to call Forget
	// if we do not want this work item being re-queued. For example, we do
	// not call Forget if a transient error occurs, instead the item is
	// put back on the workqueue and attempted again after a back-off
	// period.
	defer c.workqueue.Done(objRef)

	// Run the syncHandler, passing it the structured reference to the object to be synced.
	err := c.syncHandler(ctx, objRef)
	if err == nil {
		// If no error occurs then we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(objRef)
		logger.Info("Successfully synced", "namespace", objRef)
		return true
	}
	// there was a failure so be sure to report it.  This method allows for
	// pluggable error handling which can be used for things like
	// cluster-monitoring.
	utilruntime.HandleErrorWithContext(ctx, err, "Error syncing; requeuing for later retry", "objectReference", objRef)
	// since we failed, we should requeue the item to work on later.  This
	// method will add a backoff to avoid hotlooping on particular items
	// (they're probably still not going to work right away) and overall
	// controller protection (everything I've done is broken, this controller
	// needs to calm down or it can starve other useful work) cases.
	c.workqueue.AddRateLimited(objRef)
	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two.
func (c *Controller) syncHandler(ctx context.Context, objectRef cache.ObjectName) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "namespace", objectRef)
	logger.Info("Syncing resources", "namespace", objectRef.Name)

	c.syncResources(ctx, objectRef.Name)

	logger.Info("Successfully synced resources")
	// Retrieve the Namespace resource so that we can record the Event
	namespace, err := c.namespacesLister.Get(objectRef.Name)
	if err != nil {
		utilruntime.HandleErrorWithContext(ctx, err, "failed to get Namespace", "namespace", objectRef.Name)
	}
	c.recorder.Event(namespace, corev1.EventTypeNormal, SuccessSynced, MessageNamespaceSynced)
	return nil
}

// syncResources syncs the resources in the given namespace
func (c *Controller) syncResources(ctx context.Context, namespace string) {
	// TODO: implement this function
	logger := klog.FromContext(ctx)
	logMessage := fmt.Sprintf("Syncing resources in namespace %s", namespace)
	logger.Info(logMessage)
}

// enqueueNamespace takes a Namespace resource and converts it into an ObjectRef
// which is then put onto the work queue. This method should *not* be
// passed resources of any type other than Namespace.
func (c *Controller) enqueueNamespace(obj interface{}) {
	if objectRef, err := cache.ObjectToName(obj); err != nil {
		utilruntime.HandleError(err)
		return
	} else {
		c.workqueue.AddRateLimited(objectRef)
	}
}
