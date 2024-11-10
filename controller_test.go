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
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/diff"
	kubeinformers "k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2/ktesting"
)

var (
	alwaysReady        = func() bool { return true }
	noResyncPeriodFunc = func() time.Duration { return 0 }
)

type fixture struct {
	t *testing.T

	config     *ControllerConfig
	kubeclient *k8sfake.Clientset
	// Objects to put in the store.
	namespaceLister []*corev1.Namespace
	// Actions expected to happen on the client.
	kubeactions []core.Action
	// Objects from here preloaded into NewSimpleFake.
	kubeobjects []runtime.Object
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{}
	f.t = t
	f.kubeobjects = []runtime.Object{}
	return f
}

func (f *fixture) newController(ctx context.Context) (*Controller, kubeinformers.SharedInformerFactory) {
	f.kubeclient = k8sfake.NewSimpleClientset(f.kubeobjects...)
	f.config = &ControllerConfig{
		ExcludedNamespaces: ExcludedNamespaces,
	}
	k8sI := kubeinformers.NewSharedInformerFactory(f.kubeclient, noResyncPeriodFunc())

	c := NewController(ctx, f.config, f.kubeclient, k8sI.Core().V1().Namespaces())

	c.namespacesSynced = alwaysReady
	c.recorder = &record.FakeRecorder{}

	for _, n := range f.namespaceLister {
		k8sI.Core().V1().Namespaces().Informer().GetIndexer().Add(n)
	}

	return c, k8sI
}

func (f *fixture) run(ctx context.Context, nsRef cache.ObjectName) {
	f.runController(ctx, nsRef, true, false)
}

func (f *fixture) runExpectError(ctx context.Context, nsRef cache.ObjectName) {
	f.runController(ctx, nsRef, true, true)
}

func (f *fixture) runController(ctx context.Context, nsRef cache.ObjectName, startInformers bool, expectError bool) {
	c, k8sI := f.newController(ctx)
	if startInformers {
		k8sI.Start(ctx.Done())
	}

	err := c.syncHandler(ctx, nsRef)
	if !expectError && err != nil {
		f.t.Errorf("error syncing namespace: %v", err)
	} else if expectError && err == nil {
		f.t.Error("expected error syncing namespace, got nil")
	}

	k8sActions := f.kubeclient.Actions()
	for i, action := range k8sActions {
		if len(f.kubeactions) < i+1 {
			f.t.Errorf("%d unexpected actions: %+v", len(k8sActions)-len(f.kubeactions), k8sActions[i:])
			break
		}

		expectedAction := f.kubeactions[i]
		checkAction(expectedAction, action, f.t)
	}

	if len(f.kubeactions) > len(k8sActions) {
		f.t.Errorf("%d additional expected actions:%+v", len(f.kubeactions)-len(k8sActions), f.kubeactions[len(k8sActions):])
	}
}

// checkAction verifies that expected and actual actions are equal and both have
// same attached resources
func checkAction(expected, actual core.Action, t *testing.T) {
	if !(expected.Matches(actual.GetVerb(), actual.GetResource().Resource) && actual.GetSubresource() == expected.GetSubresource()) {
		t.Errorf("Expected\n\t%#v\ngot\n\t%#v", expected, actual)
		return
	}

	if reflect.TypeOf(actual) != reflect.TypeOf(expected) {
		t.Errorf("Action has wrong type. Expected: %t. Got: %t", expected, actual)
		return
	}

	switch a := actual.(type) {
	case core.CreateActionImpl:
		e, _ := expected.(core.CreateActionImpl)
		expObject := e.GetObject()
		object := a.GetObject()

		if !reflect.DeepEqual(expObject, object) {
			t.Errorf("Action %s %s has wrong object\nDiff:\n %s",
				a.GetVerb(), a.GetResource().Resource, diff.ObjectGoPrintSideBySide(expObject, object))
		}
	case core.UpdateActionImpl:
		e, _ := expected.(core.UpdateActionImpl)
		expObject := e.GetObject()
		object := a.GetObject()

		if !reflect.DeepEqual(expObject, object) {
			t.Errorf("Action %s %s has wrong object\nDiff:\n %s",
				a.GetVerb(), a.GetResource().Resource, diff.ObjectGoPrintSideBySide(expObject, object))
		}
	case core.PatchActionImpl:
		e, _ := expected.(core.PatchActionImpl)
		expPatch := e.GetPatch()
		patch := a.GetPatch()

		if !reflect.DeepEqual(expPatch, patch) {
			t.Errorf("Action %s %s has wrong patch\nDiff:\n %s",
				a.GetVerb(), a.GetResource().Resource, diff.ObjectGoPrintSideBySide(expPatch, patch))
		}
	default:
		t.Errorf("Uncaptured Action %s %s, you should explicitly add a case to capture it",
			actual.GetVerb(), actual.GetResource().Resource)
	}
}

func (f *fixture) expectListNamespacesAction() {
	f.kubeactions = append(f.kubeactions, core.NewListAction(schema.GroupVersionResource{Resource: "namespaces"}, schema.GroupVersionKind{Kind: "Namespace"}, "", metav1.ListOptions{}))
}

func (f *fixture) expectNoActions() {
	f.kubeactions = []core.Action{}
}

func newNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
}

func getRef(ns *corev1.Namespace) cache.ObjectName {
	ref := cache.MetaObjectToName(ns)
	return ref
}

func TestNamespaceSync(t *testing.T) {
	f := newFixture(t)
	ns := newNamespace("test")
	_, ctx := ktesting.NewTestContext(t)

	f.namespaceLister = append(f.namespaceLister, newNamespace("test"))
	// The current implementation of the controller does not do any actions
	f.expectNoActions()

	f.run(ctx, getRef(ns))
}
