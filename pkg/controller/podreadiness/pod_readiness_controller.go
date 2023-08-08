/*
Copyright 2021 The Kruise Authors.

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

package podreadiness

import (
	"context"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	appspub "github.com/openkruise/kruise/apis/apps/pub"
	utilclient "github.com/openkruise/kruise/pkg/util/client"
	utilpodreadiness "github.com/openkruise/kruise/pkg/util/podreadiness"
)

var (
	concurrentReconciles = 3
)

func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) *ReconcilePodReadiness {
	return &ReconcilePodReadiness{
		Client: utilclient.NewClientFromManager(mgr, "pod-readiness-controller"),
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r *ReconcilePodReadiness) error {
	// 1.Create a new controller
	// param：  1)该controller的name 2)mgr 3）以及 controller.Options 【比如worker数】

	c, err := controller.New("pod-readiness-controller", mgr, controller.Options{Reconciler: r, MaxConcurrentReconciles: concurrentReconciles})
	if err != nil {
		return err
	}

	// 2.controller care about resource:
	// watch的资源是pod, EnqueueRequestForObject= enqueue eventHandler ,过滤条件.
	// Watch 获取 Source 提供的事件，并使用 EventHandler 排队 reconcile.Requests 以响应事件。
	// 在将事件提供给 EventHandler 之前，可以为 Watch 提供一个或多个 Predicates 来过滤事件。如果所有提供的 Predicates 返回为true，事件将传递给 EventHandler。

	err = c.Watch(&source.Kind{Type: &v1.Pod{}}, &handler.EnqueueRequestForObject{}, predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			pod := e.Object.(*v1.Pod)
			return utilpodreadiness.ContainsReadinessGate(pod) && utilpodreadiness.GetReadinessCondition(pod) == nil
			// 2.1 查看 pod的 spec.ReadinessGates 中 ConditionType是 KruisePodReady 的 ReadinessGate 是否存在
			//     且获取 pod 的 pod.Status.Conditions 中 Type==KruisePodReady的Condition，判断其是否 为 nil
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			pod := e.ObjectNew.(*v1.Pod)
			return utilpodreadiness.ContainsReadinessGate(pod) && utilpodreadiness.GetReadinessCondition(pod) == nil
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false // 删除不关心
		},
		GenericFunc: func(e event.GenericEvent) bool { // generic 事件？？
			return false
		},
	})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcilePodReadiness{}

// ReconcilePodReadiness reconciles a Pod object
type ReconcilePodReadiness struct {
	client.Client
}

// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;update;patch
// 因为这个controller主要操作的是 pods以及pod的子资源status， 故 groups=core，resources=pods

func (r *ReconcilePodReadiness) Reconcile(_ context.Context, request reconcile.Request) (res reconcile.Result, err error) {
	start := time.Now() //1.Record the time spent on this request
	klog.V(3).Infof("Starting to process Pod %v", request.NamespacedName)
	defer func() {
		if err != nil {
			klog.Warningf("Failed to process Pod %v, elapsedTime %v, error: %v", request.NamespacedName, time.Since(start), err)
		} else {
			klog.Infof("Finish to process Pod %v, elapsedTime %v", request.NamespacedName, time.Since(start))
		}
	}()

	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error { // 2. 标准 的retry 机制，
		pod := &v1.Pod{}
		err = r.Get(context.TODO(), request.NamespacedName, pod) //2.1 调用 get 方法将数据指定的 pod 序列化到 pod对象中
		if err != nil {
			if errors.IsNotFound(err) {
				// Object not found, return.  Created objects are automatically garbage collected.
				// For additional cleanup logic use finalizers.
				return nil // 2.1.1 err=not found, 不处理
			}
			// Error reading the object - requeue the request. 2.1.2 出现err,会进行重试
			return err
		}
		if pod.DeletionTimestamp != nil { // 2.2 如果 DeletionTimestamp 不为nil， 代表pod要被删除，忽略不处理
			return nil
		}
		if !utilpodreadiness.ContainsReadinessGate(pod) { // 2.3  pod.Spec.ReadinessGates 中不包含 Type=KruisePodReady 的ReadinessGate , 忽略不处理
			return nil
		}
		if utilpodreadiness.GetReadinessCondition(pod) != nil { // 2.4  获取 pod.Status.Conditions 中 Type=KruisePodReady 的 condition ,如果不存在， 忽略不处理
			return nil
		}

		pod.Status.Conditions = append(pod.Status.Conditions, v1.PodCondition{ // 2.5 初始化 KruisePodReady condition 为true, 调用 status 更新pod的 status
			Type:               appspub.KruisePodReadyConditionType,
			Status:             v1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
		})
		return r.Status().Update(context.TODO(), pod)
	})
	return reconcile.Result{}, err
}
