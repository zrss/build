/*
Copyright 2018 The Knative Authors

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

package build

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/knative/build/pkg/builder"
	"github.com/knative/build/pkg/builder/nop"
	"go.uber.org/zap"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubeinformers "k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	v1alpha1 "github.com/knative/build/pkg/apis/build/v1alpha1"
	"github.com/knative/build/pkg/client/clientset/versioned/fake"
	informers "github.com/knative/build/pkg/client/informers/externalversions"
)

const (
	noErrorMessage = ""
)

const (
	noResyncPeriod time.Duration = 0
)

type fixture struct {
	t *testing.T

	client     *fake.Clientset
	kubeclient *k8sfake.Clientset
	// Objects to put in the store.
	buildLister []*v1alpha1.Build
	// Objects from here preloaded into NewSimpleFake.
	kubeobjects []runtime.Object
	objects     []runtime.Object
	eventCh     chan string
}

func newBuild(name string) *v1alpha1.Build {
	return &v1alpha1.Build{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: metav1.NamespaceDefault,
		},
		Spec: v1alpha1.BuildSpec{
			Timeout: "20m",
		},
	}
}

func (f *fixture) newController(b builder.Interface, eventCh chan string) (*Controller, informers.SharedInformerFactory, kubeinformers.SharedInformerFactory) {
	i := informers.NewSharedInformerFactory(f.client, noResyncPeriod)
	k8sI := kubeinformers.NewSharedInformerFactory(f.kubeclient, noResyncPeriod)
	logger := zap.NewExample().Sugar()
	c := NewController(b, f.kubeclient, f.client, k8sI, i, logger).(*Controller)

	c.buildsSynced = func() bool { return true }
	c.recorder = &record.FakeRecorder{
		Events: eventCh,
	}

	return c, i, k8sI
}

func (f *fixture) updateIndex(i informers.SharedInformerFactory, bl []*v1alpha1.Build) {
	for _, f := range bl {
		i.Build().V1alpha1().Builds().Informer().GetIndexer().Add(f)
	}
}

func getKey(build *v1alpha1.Build, t *testing.T) string {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(build)
	if err != nil {
		t.Errorf("Unexpected error getting key for build %v: %v", build.Name, err)
		return ""
	}
	return key
}

func TestBasicFlows(t *testing.T) {
	tests := []struct {
		bldr                 builder.Interface
		setup                func()
		expectedErrorMessage string
	}{{
		bldr:                 &nop.Builder{},
		expectedErrorMessage: noErrorMessage,
	}, {
		bldr:                 &nop.Builder{ErrorMessage: "boom"},
		expectedErrorMessage: "boom",
	}}

	for idx, test := range tests {
		build := newBuild("test")
		f := &fixture{
			t:           t,
			objects:     []runtime.Object{build},
			kubeobjects: nil,
			client:      fake.NewSimpleClientset(build),
			kubeclient:  k8sfake.NewSimpleClientset(),
		}

		stopCh := make(chan struct{})
		eventCh := make(chan string, 1024)
		defer close(stopCh)
		defer close(eventCh)

		c, i, k8sI := f.newController(test.bldr, eventCh)
		f.updateIndex(i, []*v1alpha1.Build{build})
		i.Start(stopCh)
		k8sI.Start(stopCh)

		// Run a single iteration of the syncHandler.
		if err := c.syncHandler(getKey(build, t)); err != nil {
			t.Errorf("error syncing build: %v", err)
		}

		buildClient := f.client.BuildV1alpha1().Builds(build.Namespace)
		first, err := buildClient.Get(build.Name, metav1.GetOptions{})
		if err != nil {
			t.Errorf("error fetching build: %v", err)
		}
		// Update status to current time
		first.Status.CreationTime = metav1.Now()

		if builder.IsDone(&first.Status) {
			t.Errorf("First IsDone(%d); wanted not done, got done.", idx)
		}
		if msg, failed := builder.ErrorMessage(&first.Status); failed {
			t.Errorf("First ErrorMessage(%d); wanted not failed, got %q.", idx, msg)
		}

		// We have to manually update the index, or the controller won't see the update.
		f.updateIndex(i, []*v1alpha1.Build{first})

		// Run a second iteration of the syncHandler.
		if err := c.syncHandler(getKey(build, t)); err != nil {
			t.Errorf("error syncing build: %v", err)
		}
		// A second reconciliation will trigger an asynchronous "Wait()", which
		// should immediately return and trigger an update.  Sleep to ensure that
		// is all done before further checks.
		time.Sleep(1 * time.Second)

		second, err := buildClient.Get(build.Name, metav1.GetOptions{})
		if err != nil {
			t.Errorf("error fetching build: %v", err)
		}

		if !builder.IsDone(&second.Status) {
			t.Errorf("Second IsDone(%d, %v); wanted done, got not done.", idx, second.Status)
		}
		if msg, _ := builder.ErrorMessage(&second.Status); test.expectedErrorMessage != msg {
			t.Errorf("Second ErrorMessage(%d); wanted %q, got %q.", idx, test.expectedErrorMessage, msg)
		}

		successEvent := "Normal Synced Build synced successfully"

		select {
		case statusEvent := <-eventCh:
			if statusEvent != successEvent {
				t.Errorf("Event; wanted %q, got %q", successEvent, statusEvent)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("No events published")
		}
	}
}

func TestErrFlows(t *testing.T) {
	bldr := &nop.Builder{Err: errors.New("not okay")}
	expectedErrEventMsg := "Warning BuildExecuteFailed Failed to execute Build"

	build := newBuild("test")
	f := &fixture{
		t:           t,
		objects:     []runtime.Object{build},
		kubeobjects: nil,
		client:      fake.NewSimpleClientset(build),
		kubeclient:  k8sfake.NewSimpleClientset(),
	}

	stopCh := make(chan struct{})
	eventCh := make(chan string, 1024)
	defer close(stopCh)
	defer close(eventCh)

	c, i, k8sI := f.newController(bldr, eventCh)
	f.updateIndex(i, []*v1alpha1.Build{build})
	i.Start(stopCh)
	k8sI.Start(stopCh)

	if err := c.syncHandler(getKey(build, t)); err == nil {
		t.Errorf("Expect error syncing build")
	}

	select {
	case statusEvent := <-eventCh:
		if !strings.Contains(statusEvent, expectedErrEventMsg) {
			t.Errorf("Event message; wanted %q, got %q", expectedErrEventMsg, statusEvent)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("No events published")
	}
}

func TestTimeoutFlows(t *testing.T) {
	bldr := &nop.Builder{}

	build := newBuild("test")
	build.Spec.Timeout = "1s"

	f := &fixture{
		t:           t,
		objects:     []runtime.Object{build},
		kubeobjects: nil,
		client:      fake.NewSimpleClientset(build),
		kubeclient:  k8sfake.NewSimpleClientset(),
	}

	stopCh := make(chan struct{})
	eventCh := make(chan string, 1024)
	defer close(stopCh)
	defer close(eventCh)

	c, i, k8sI := f.newController(bldr, eventCh)

	f.updateIndex(i, []*v1alpha1.Build{build})
	i.Start(stopCh)
	k8sI.Start(stopCh)

	if err := c.syncHandler(getKey(build, t)); err != nil {
		t.Errorf("Not Expect error when syncing build")
	}

	buildClient := f.client.BuildV1alpha1().Builds(build.Namespace)
	first, err := buildClient.Get(build.Name, metav1.GetOptions{})
	if err != nil {
		t.Errorf("error fetching build: %v", err)
	}

	// Update status to past time by substracting buffer time
	buffer, err := time.ParseDuration("10m")
	if err != nil {
		t.Errorf("Error parsing duration")
	}
	first.Status.CreationTime.Time = metav1.Now().Time.Add(-buffer)

	if builder.IsDone(&first.Status) {
		t.Error("First IsDone; wanted not done, got done.")
	}
	if msg, failed := builder.ErrorMessage(&first.Status); failed {
		t.Errorf("First ErrorMessage(%v); wanted not failed, got failed", msg)
	}

	// We have to manually update the index, or the controller won't see the update.
	f.updateIndex(i, []*v1alpha1.Build{first})

	// process the work item in queue
	c.processNextWorkItem()

	expectedTimeoutMsg := "Warning BuildTimeout Build \"test\" failed to finish within \"1s\""
	for i := 0; i < 2; i++ {
		select {
		case statusEvent := <-eventCh:
			// Check 2nd event for timeout error msg. First event will sync build successfully
			if !strings.Contains(statusEvent, expectedTimeoutMsg) && i != 0 {
				t.Errorf("Event message; wanted %q got %q", expectedTimeoutMsg, statusEvent)
			}
		case <-time.After(4 * time.Second):
			t.Fatalf("No events published")
		}
	}
}

func TestBuildTimeoutControllerFlow(t *testing.T)  {
	stopCh := make(chan struct{})
	eventCh := make(chan string, 16)
	defer close(stopCh)
	defer close(eventCh)

	buildClient := fake.NewSimpleClientset()
	kubeClient := k8sfake.NewSimpleClientset()

	buildInformerFactor := informers.NewSharedInformerFactory(buildClient, noResyncPeriod)
	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, noResyncPeriod)
	logger := zap.NewExample().Sugar()

	c := NewController(&nop.Builder{}, kubeClient, buildClient, kubeInformerFactory, buildInformerFactor, logger).(*Controller)
	c.recorder = &record.FakeRecorder{
		Events: eventCh,
	}

	buildInformerFactor.Start(stopCh)
	kubeInformerFactory.Start(stopCh)

	// run the build controller
	go func() {
		c.Run(2, stopCh)
	}()

	mockBuildName := "test"

	// create the build resource
	build := newBuild(mockBuildName)
	buildClient.BuildV1alpha1().Builds(metav1.NamespaceDefault).Create(build)

	// change me
	// currently, the following events are expected to occur
	//    create a build resource -> trigger an
	// 1. build add event
	//    exec syncHandler -> update status after the build execution, trigger an
	// 2. build update event
	//    exec syncHandler -> update status after the build timeout, trigger an
	// 3. build update event
	//    exec syncHandler -> build has done, the ending of process
	//
	// in summary, we should see the first MessageResourceSynced event, then BuildExecuteFailed event
	// and the final MessageResourceSynced event (totally 2 MessageResourceSynced event and 1 BuildExecuteFailed event)

	// It must a build timeout status as nop builder updates the build.Status.CreationTime to time.Time
	// and the syncHandler uses (time.Now() - build.Status.CreationTime) elapsed time to detect build timeout

	deadline := time.After(4 * time.Second)

	buildSyncedCnt := 0;
	buildTimeoutCnt := 0;

detect:
	for {
		select {
		case statusEvent := <-eventCh:
			// grep with the reason of event
			if strings.Contains(statusEvent, SuccessSynced) {
				buildSyncedCnt++
			}
			if strings.Contains(statusEvent, "BuildTimeout") {
				buildTimeoutCnt++
			}

			if buildSyncedCnt == 2 && buildTimeoutCnt == 1 {
				break detect
			}
		case <- deadline:
			t.Errorf("Timeout to wait BuildTimeout event")
			break detect
		}
	}
}

func TestTimeoutBuildReEnqueue(t *testing.T) {
	stopCh := make(chan struct{})
	eventCh := make(chan string, 16)
	defer close(stopCh)
	defer close(eventCh)

	build := newBuild("test")
	build.Spec.Timeout = "1s"

	f := &fixture{
		t:           t,
		client:      fake.NewSimpleClientset(build),
		kubeclient:  k8sfake.NewSimpleClientset(),
	}

	c, buildInformerFactor, _ := f.newController(&nop.Builder{}, eventCh)

	// let the Lister of Informer and Clientset can get the build resource
	f.updateIndex(buildInformerFactor, []*v1alpha1.Build{build})

	timeout, err := time.ParseDuration(build.Spec.Timeout)
	if err != nil {
		t.Errorf("Error parsing duration")
		return
	}

	drainWorkqueue := func (ctrlWorkqueue workqueue.RateLimitingInterface) bool {
		for ctrlWorkqueue.Len() > 0 {
			obj, shutdown := c.workqueue.Get()

			if shutdown {
				return false
			}

			ctrlWorkqueue.Forget(obj)
			ctrlWorkqueue.Done(obj)
		}

		return true
	}

	// updateBuild will add at least one build resource into workqueue

	// build add
	if err := c.syncHandler(getKey(build, t)); err != nil {
		t.Errorf("Not Expect error when syncing build")
		return
	}

	select {
	case <- time.After(5 * time.Second):
		if c.workqueue.Len() != 1 {
			t.Errorf("ReEnqueue a build resource failed; wanted %d got %d", 1, c.workqueue.Len())
			return
		}
	}

	drainWorkqueue(c.workqueue)

	// build update
	buildCur := newBuild("test")
	buildCur.Spec.Timeout = "2s"

	// 1. unchanging
	c.updateBuild(build, build)
	select {
	case <- time.After(timeout):
		if c.workqueue.Len() != 1 {
			t.Errorf("ReEnqueue a build resource failed; wanted %d got %d", 1, c.workqueue.Len())
			return
		}
	}

	drainWorkqueue(c.workqueue)

	// 2. changed but build was timeout
	c.updateBuild(build, buildCur)
	select {
	case <- time.After(2 * time.Second):
		if c.workqueue.Len() != 1 {
			t.Errorf("ReEnqueue a build resource failed; wanted %d got %d", 1, c.workqueue.Len())
			return
		}
	}

	drainWorkqueue(c.workqueue)

	// 3. changed and build has not timeout
	buildCur.Status.CreationTime = metav1.Now()

	c.updateBuild(build, buildCur)
	select {
	case <- time.After(8 * time.Second):
		if c.workqueue.Len() != 2 {
			t.Errorf("ReEnqueue a build resource failed; wanted %d got %d", 2, c.workqueue.Len())
			return
		}
	}
}
