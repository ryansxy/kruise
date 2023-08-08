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

package podunavailablebudget

import (
	"context"
	"flag"
	"fmt"
	"time"

	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	kubecontroller "k8s.io/kubernetes/pkg/controller"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kruiseappsv1alpha1 "github.com/openkruise/kruise/apis/apps/v1alpha1"
	kruiseappsv1beta1 "github.com/openkruise/kruise/apis/apps/v1beta1"
	policyv1alpha1 "github.com/openkruise/kruise/apis/policy/v1alpha1"
	kubeClient "github.com/openkruise/kruise/pkg/client"
	"github.com/openkruise/kruise/pkg/control/pubcontrol"
	"github.com/openkruise/kruise/pkg/features"
	"github.com/openkruise/kruise/pkg/util"
	"github.com/openkruise/kruise/pkg/util/controllerfinder"
	utildiscovery "github.com/openkruise/kruise/pkg/util/discovery"
	utilfeature "github.com/openkruise/kruise/pkg/util/feature"
	"github.com/openkruise/kruise/pkg/util/ratelimiter"
)

func init() {
	flag.IntVar(&concurrentReconciles, "podunavailablebudget-workers", concurrentReconciles, "Max concurrent workers for PodUnavailableBudget controller.")
}

var (
	concurrentReconciles = 3
	controllerKind       = policyv1alpha1.SchemeGroupVersion.WithKind("PodUnavailableBudget")
)

const (
	DeletionTimeout       = 20 * time.Second
	UpdatedDelayCheckTime = 10 * time.Second
)

var ConflictRetry = wait.Backoff{
	Steps:    4,
	Duration: 500 * time.Millisecond,
	Factor:   1.0,
	Jitter:   0.1,
}

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new PodUnavailableBudget Controller and adds it to the Manager with default RBAC. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	if !utildiscovery.DiscoverGVK(controllerKind) { // 在 k8s 集群中，查看 k8s 的 DiscoveryClient 中是否有包含这个gvk，如果包含返回 true，添加该controller。 如果不包含，则返回false,返回
		return nil
	}
	// 判断 pubDeleteGate 和 DefaultFeatureGate 是否开启，如果都没有开启，则返回
	if !utilfeature.DefaultFeatureGate.Enabled(features.PodUnavailableBudgetDeleteGate) &&
		!utilfeature.DefaultFeatureGate.Enabled(features.PodUnavailableBudgetUpdateGate) {
		return nil
	}
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcilePodUnavailableBudget{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		recorder:         mgr.GetEventRecorderFor("podunavailablebudget-controller"),
		controllerFinder: controllerfinder.Finder,
		pubControl:       pubcontrol.NewPubControl(mgr.GetClient()),
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
// 创建 controller 后， 会 watch 如下 obj
// 1）watch pub 入队
// 2) watch pod 入队
// 3) watch deployment、kruise、CloneSet、StatefulSet， 如果是update事件，replicas发生了变更，如果是delete事件， 直接入queue (&SetEnqueueRequestForPUB{mgr},)
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// 1.Create a new controller
	c, err := controller.New("podunavailablebudget-controller", mgr, controller.Options{
		Reconciler: r, MaxConcurrentReconciles: concurrentReconciles, // 1.设置最大的task数
		RateLimiter: ratelimiter.DefaultControllerRateLimiter()}) // 2. 设置 RateLimiterQueue
	if err != nil {
		return err
	}

	// 2.Watch for changes to PodUnavailableBudget    pub 入队
	// 1）Watch 获取 Source 提供的事件，并使用 EventHandler 排队 reconcile.Requests 以响应事件。
	// 2）在将事件提供给 EventHandler 之前，可以为 Watch 提供一个或多个 Predicates 来过滤事件。 如果所有提供的 Predicates 评估为true，事件将传递给 EventHandler。

	// EnqueueRequestForObject enqueues a Request containing the Name and Namespace of the object that is the source of the Event.
	// (e.g. the created / deleted / updated objects Name and Namespace).  handler.EnqueueRequestForObject is used by almost all
	// Controllers that have associated Resources (e.g. CRDs) to reconcile the associated Resource.

	// EnqueueRequestForObject 将包含 作为事件源的对象的name和ns的request 排入队列。
	// 比如 ：handler.EnqueueRequestForObject 几乎所有具有关联资源（例如 CRD）的控制器都使用它 reconcile the associated Resource.
	err = c.Watch(&source.Kind{Type: &policyv1alpha1.PodUnavailableBudget{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// 3.Watch for changes to Pod
	if err = c.Watch(&source.Kind{Type: &corev1.Pod{}}, newEnqueueRequestForPod(mgr.GetClient())); err != nil {
		return err
	}

	// In workload scaling scenario, there is a risk of interception by the pub webhook against the scaled pod.
	// The solution for this scenario: the pub controller listens to workload replicas changes and adjusts UnavailableAllowed in time.
	// Example for:
	// 1. cloneSet.replicas = 100, pub.MaxUnavailable = 10%, then UnavailableAllowed=10.
	// 2. at this time the cloneSet.replicas is scaled down to 50, the pub controller listens to the replicas change, triggering reconcile will adjust UnavailableAllowed to 55.
	// 3. so pub webhook will not intercept the request to delete the pods
	// 在 workload scaling 场景中，pub webhook 存在被扩展的 pod 拦截的风险。
	// 这种场景的解决方案：pub controller监听workload replicas变化，及时调整Unavailable Allowed。比如
	// 1. cloneSet.replicas = 100, pub.MaxUnavailable = 10%, then UnavailableAllowed=10.
	// 2. 此时 cloneSet.replicas 缩小到 50，pub 控制器 监听副本变化，触发 reconcile 将调整 UnavailableAllowed 到 55。
	// 3.  所以 pub webhook 不会拦截删除 pod 的请求

	// 4.watch deployment    pub 入队
	if err = c.Watch(&source.Kind{Type: &apps.Deployment{}}, &SetEnqueueRequestForPUB{mgr}, predicate.Funcs{
		// 1. update 时, 判断 oldDeployment 和 newDeployment 的 replicas 是否发生变更，如果发生变更，代表需要 enqueue
		UpdateFunc: func(e event.UpdateEvent) bool {
			old := e.ObjectOld.(*apps.Deployment)
			new := e.ObjectNew.(*apps.Deployment)
			return *old.Spec.Replicas != *new.Spec.Replicas
		},
		// 2.delete 时， 直接返回true
		DeleteFunc: func(deleteEvent event.DeleteEvent) bool {
			return true
		},
	}); err != nil {
		return err
	}

	// 5, watch kruise AdvancedStatefulSet
	if err = c.Watch(&source.Kind{Type: &kruiseappsv1beta1.StatefulSet{}}, &SetEnqueueRequestForPUB{mgr}, predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			old := e.ObjectOld.(*kruiseappsv1beta1.StatefulSet)
			new := e.ObjectNew.(*kruiseappsv1beta1.StatefulSet)
			return *old.Spec.Replicas != *new.Spec.Replicas
		},
		DeleteFunc: func(deleteEvent event.DeleteEvent) bool {
			return true
		},
	}); err != nil {
		return err
	}

	// CloneSet
	if err = c.Watch(&source.Kind{Type: &kruiseappsv1alpha1.CloneSet{}}, &SetEnqueueRequestForPUB{mgr}, predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			old := e.ObjectOld.(*kruiseappsv1alpha1.CloneSet)
			new := e.ObjectNew.(*kruiseappsv1alpha1.CloneSet)
			return *old.Spec.Replicas != *new.Spec.Replicas
		},
		DeleteFunc: func(deleteEvent event.DeleteEvent) bool {
			return true
		},
	}); err != nil {
		return err
	}

	// StatefulSet
	if err = c.Watch(&source.Kind{Type: &apps.StatefulSet{}}, &SetEnqueueRequestForPUB{mgr}, predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			old := e.ObjectOld.(*apps.StatefulSet)
			new := e.ObjectNew.(*apps.StatefulSet)
			return *old.Spec.Replicas != *new.Spec.Replicas
		},
		DeleteFunc: func(deleteEvent event.DeleteEvent) bool {
			return true
		},
	}); err != nil {
		return err
	}

	klog.Infof("add podunavailablebudget reconcile.Reconciler success")
	return nil
}

var _ reconcile.Reconciler = &ReconcilePodUnavailableBudget{}

// ReconcilePodUnavailableBudget reconciles a PodUnavailableBudget object
type ReconcilePodUnavailableBudget struct {
	client.Client
	Scheme           *runtime.Scheme
	recorder         record.EventRecorder
	controllerFinder *controllerfinder.ControllerFinder // 得到 controllerRef
	pubControl       pubcontrol.PubControl              //
}

// +kubebuilder:rbac:groups=policy.kruise.io,resources=podunavailablebudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy.kruise.io,resources=podunavailablebudgets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=*,resources=*/scale,verbs=get;list;watch

// pkg/controller/cloneset/cloneset_controller.go Watch for changes to CloneSet
func (r *ReconcilePodUnavailableBudget) Reconcile(_ context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Fetch the PodUnavailableBudget instance
	// 1.获取 pub，如果 err !=nil 且err是notFound，或 pub存在，但是pub.DeletionTimestamp !=nil,则清除 GlobalCache中的pub；删除成功，则返回error, 删除失败，重试
	pub := &policyv1alpha1.PodUnavailableBudget{}
	err := r.Get(context.TODO(), req.NamespacedName, pub)
	if (err != nil && errors.IsNotFound(err)) || (err == nil && !pub.DeletionTimestamp.IsZero()) {
		klog.V(3).Infof("pub(%s/%s) is Deletion in this time", req.Namespace, req.Name)
		if cacheErr := util.GlobalCache.Delete(&policyv1alpha1.PodUnavailableBudget{
			TypeMeta: metav1.TypeMeta{
				APIVersion: policyv1alpha1.GroupVersion.String(),
				Kind:       controllerKind.Kind,
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      req.Name,
				Namespace: req.Namespace,
			},
		}); cacheErr != nil {
			klog.Errorf("Delete cache failed for PodUnavailableBudget(%s/%s): %s", req.Namespace, req.Name, err.Error())
		}
		// Object not found, return.  Created objects are automatically garbage collected.
		// For additional cleanup logic use finalizers.
		return reconcile.Result{}, nil
	} else if err != nil {
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	klog.V(3).Infof("begin to process podUnavailableBudget(%s/%s)", pub.Namespace, pub.Name)
	recheckTime, err := r.syncPodUnavailableBudget(pub) //2.syncPub同时返回一个 recheckTime， 如果recheckTime不等于nil,等待 recheckTime 后再次入队
	if err != nil {
		return ctrl.Result{}, err
	}
	if recheckTime != nil {
		return ctrl.Result{RequeueAfter: time.Until(*recheckTime)}, nil
	}
	return ctrl.Result{}, nil
}

func (r *ReconcilePodUnavailableBudget) syncPodUnavailableBudget(pub *policyv1alpha1.PodUnavailableBudget) (*time.Time, error) {
	currentTime := time.Now()
	pods, expectedCount, err := r.pubControl.GetPodsForPub(pub) // 1. 从pub中获取对应的pods,和 expectedCount
	if err != nil {
		return nil, err
	}
	if len(pods) == 0 { // 2.如果pods 为 nil,打印event; 否则在workload 所有的 pod 上修改 related-pub annotation
		r.recorder.Eventf(pub, corev1.EventTypeNormal, "NoPods", "No matching pods found")
	} else {
		// patch related-pub annotation in all pods of workload
		if err = r.patchRelatedPubAnnotationInPod(pub, pods); err != nil {
			klog.Errorf("pub(%s/%s) patch pod annotation failed: %s", pub.Namespace, pub.Name, err.Error())
			return nil, err
		}
	}

	klog.V(3).Infof("pub(%s/%s) controller pods(%d) expectedCount(%d)", pub.Namespace, pub.Name, len(pods), expectedCount)
	desiredAvailable, err := r.getDesiredAvailableForPub(pub, expectedCount) // 3.获取pub DesiredAvailable pod 个数
	if err != nil {
		r.recorder.Eventf(pub, corev1.EventTypeWarning, "CalculateExpectedPodCountFailed", "Failed to calculate the number of expected pods: %v", err)
		return nil, err
	}

	// 4.
	// for debug
	var conflictTimes int
	var costOfGet, costOfUpdate time.Duration
	var pubClone *policyv1alpha1.PodUnavailableBudget
	refresh := false
	var recheckTime *time.Time
	err = retry.RetryOnConflict(ConflictRetry, func() error {
		unlock := util.GlobalKeyedMutex.Lock(string(pub.UID)) //4.1 lock
		defer unlock()

		start := time.Now()
		if refresh {
			// fetch pub from etcd  4.2 获取指定的pub，如果获取错误，直接返回err
			pubClone, err = kubeClient.GetGenericClient().KruiseClient.PolicyV1alpha1().
				PodUnavailableBudgets(pub.Namespace).Get(context.TODO(), pub.Name, metav1.GetOptions{})
			if err != nil {
				klog.Errorf("Get PodUnavailableBudget(%s/%s) failed from etcd: %s", pub.Namespace, pub.Name, err.Error())
				return err
			}
		} else {
			// compare local cache and informer cache, then get the newer one
			item, _, err := util.GlobalCache.Get(pub)
			if err != nil {
				klog.Errorf("Get PodUnavailableBudget(%s/%s) cache failed: %s", pub.Namespace, pub.Name, err.Error())
			}
			if localCached, ok := item.(*policyv1alpha1.PodUnavailableBudget); ok {
				pubClone = localCached.DeepCopy()
			} else {
				pubClone = pub.DeepCopy()
			}
			informerCached := &policyv1alpha1.PodUnavailableBudget{}
			if err := r.Get(context.TODO(), types.NamespacedName{Namespace: pub.Namespace,
				Name: pub.Name}, informerCached); err == nil {
				var localRV, informerRV int64
				_ = runtime.Convert_string_To_int64(&pubClone.ResourceVersion, &localRV, nil)
				_ = runtime.Convert_string_To_int64(&informerCached.ResourceVersion, &informerRV, nil)
				if informerRV > localRV {
					pubClone = informerCached
				}
			}
		}
		costOfGet += time.Since(start)

		// disruptedPods contains information about pods whose eviction or deletion was processed by the API handler but has not yet been observed by the PodUnavailableBudget.
		// unavailablePods contains information about pods whose specification changed(in-place update), in case of informer cache latency, after 5 seconds to remove it.

		// disruptedPods 包含有关其eviction or deletion 已由 API 处理程序处理但尚未被 PodUnavailableBudget 观察到的 pod 的信息。
		// unavailablePods 包含有关规格更改(in-place update)的 pod 的信息，以防 informer 缓存延迟，5 秒后将其删除。

		var disruptedPods, unavailablePods map[string]metav1.Time
		disruptedPods, unavailablePods, recheckTime = r.buildDisruptedAndUnavailablePods(pods, pubClone, currentTime) // 4.3 获取 disruptedPods 和 unavailablePods pods
		currentAvailable := countAvailablePods(pods, disruptedPods, unavailablePods, r.pubControl)

		start = time.Now()
		// 4.4 调用 update PubStatus
		updateErr := r.updatePubStatus(pubClone, currentAvailable, desiredAvailable, expectedCount, disruptedPods, unavailablePods)
		costOfUpdate += time.Since(start)
		if updateErr == nil {
			return nil
		}
		// update failed, and retry
		refresh = true
		conflictTimes++
		return updateErr
	})
	klog.V(3).Infof("Controller cost of pub(%s/%s): conflict times %v, cost of Get %v, cost of Update %v",
		pub.Namespace, pub.Name, conflictTimes, costOfGet, costOfUpdate)
	if err != nil {
		klog.Errorf("update pub(%s/%s) status failed: %s", pub.Namespace, pub.Name, err.Error())
	}
	return recheckTime, err
}

func (r *ReconcilePodUnavailableBudget) patchRelatedPubAnnotationInPod(pub *policyv1alpha1.PodUnavailableBudget, pods []*corev1.Pod) error {
	var updatedPods []*corev1.Pod
	for i := range pods {
		if pods[i].Annotations[pubcontrol.PodRelatedPubAnnotation] == "" {
			updatedPods = append(updatedPods, pods[i].DeepCopy())
		}
	}
	if len(updatedPods) == 0 {
		return nil
	}
	// update related-pub annotation in pods
	for _, pod := range updatedPods {
		body := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, pubcontrol.PodRelatedPubAnnotation, pub.Name)
		if err := r.Patch(context.TODO(), pod, client.RawPatch(types.StrategicMergePatchType, []byte(body))); err != nil {
			return err
		}
	}
	klog.V(3).Infof("patch pub(%s/%s) old pods(%d) related-pub annotation success", pub.Namespace, pub.Name, len(updatedPods))
	return nil
}

func countAvailablePods(pods []*corev1.Pod, disruptedPods, unavailablePods map[string]metav1.Time, control pubcontrol.PubControl) (currentAvailable int32) {
	recordPods := sets.String{}
	for pName := range disruptedPods {
		recordPods.Insert(pName)
	}
	for pName := range unavailablePods {
		recordPods.Insert(pName)
	}

	for _, pod := range pods {
		if !kubecontroller.IsPodActive(pod) {
			continue
		}
		// ignore disrupted or unavailable pods, where the Pod is considered unavailable
		if recordPods.Has(pod.Name) {
			continue
		}
		// pod consistent and ready
		if control.IsPodStateConsistent(pod) && control.IsPodReady(pod) {
			currentAvailable++
		}
	}

	return
}

func (r *ReconcilePodUnavailableBudget) getDesiredAvailableForPub(pub *policyv1alpha1.PodUnavailableBudget, expectedCount int32) (desiredAvailable int32, err error) {
	if pub.Spec.MaxUnavailable != nil {
		var maxUnavailable int
		maxUnavailable, err = intstr.GetScaledValueFromIntOrPercent(pub.Spec.MaxUnavailable, int(expectedCount), true)
		if err != nil {
			return
		}

		desiredAvailable = expectedCount - int32(maxUnavailable)
		if desiredAvailable < 0 {
			desiredAvailable = 0
		}
	} else if pub.Spec.MinAvailable != nil {
		if pub.Spec.MinAvailable.Type == intstr.Int {
			desiredAvailable = pub.Spec.MinAvailable.IntVal
		} else if pub.Spec.MinAvailable.Type == intstr.String {
			var minAvailable int
			minAvailable, err = intstr.GetScaledValueFromIntOrPercent(pub.Spec.MinAvailable, int(expectedCount), true)
			if err != nil {
				return
			}
			desiredAvailable = int32(minAvailable)
		}
	}
	return
}

func (r *ReconcilePodUnavailableBudget) buildDisruptedAndUnavailablePods(pods []*corev1.Pod, pub *policyv1alpha1.PodUnavailableBudget, currentTime time.Time) (
	// disruptedPods, unavailablePods, recheckTime
	map[string]metav1.Time, map[string]metav1.Time, *time.Time) {

	disruptedPods := pub.Status.DisruptedPods
	unavailablePods := pub.Status.UnavailablePods

	resultDisruptedPods := make(map[string]metav1.Time)
	resultUnavailablePods := make(map[string]metav1.Time)
	var recheckTime *time.Time

	if disruptedPods == nil && unavailablePods == nil {
		return resultDisruptedPods, resultUnavailablePods, recheckTime
	}
	for _, pod := range pods {
		if !kubecontroller.IsPodActive(pod) {
			continue
		}

		// handle disruption pods which will be eviction or deletion
		disruptionTime, found := disruptedPods[pod.Name]
		if found {
			expectedDeletion := disruptionTime.Time.Add(DeletionTimeout)
			if expectedDeletion.Before(currentTime) {
				r.recorder.Eventf(pod, corev1.EventTypeWarning, "NotDeleted", "Pod was expected by PUB %s/%s to be deleted but it wasn't",
					pub.Namespace, pub.Name)
			} else {
				resultDisruptedPods[pod.Name] = disruptionTime
				if recheckTime == nil || expectedDeletion.Before(*recheckTime) {
					recheckTime = &expectedDeletion
				}
			}
		}

		// handle unavailable pods which have been in-updated specification
		unavailableTime, found := unavailablePods[pod.Name]
		if found {
			// in case of informer cache latency, after 10 seconds to remove it
			expectedUpdate := unavailableTime.Time.Add(UpdatedDelayCheckTime)
			if expectedUpdate.Before(currentTime) {
				continue
			} else {
				resultUnavailablePods[pod.Name] = unavailableTime
				if recheckTime == nil || expectedUpdate.Before(*recheckTime) {
					recheckTime = &expectedUpdate
				}
			}

		}
	}
	return resultDisruptedPods, resultUnavailablePods, recheckTime
}

func (r *ReconcilePodUnavailableBudget) updatePubStatus(pub *policyv1alpha1.PodUnavailableBudget, currentAvailable, desiredAvailable, expectedCount int32,
	disruptedPods, unavailablePods map[string]metav1.Time) error {

	unavailableAllowed := currentAvailable - desiredAvailable
	if unavailableAllowed <= 0 {
		unavailableAllowed = 0
	}

	if pub.Status.CurrentAvailable == currentAvailable &&
		pub.Status.DesiredAvailable == desiredAvailable &&
		pub.Status.TotalReplicas == expectedCount &&
		pub.Status.UnavailableAllowed == unavailableAllowed &&
		pub.Status.ObservedGeneration == pub.Generation &&
		apiequality.Semantic.DeepEqual(pub.Status.DisruptedPods, disruptedPods) &&
		apiequality.Semantic.DeepEqual(pub.Status.UnavailablePods, unavailablePods) {
		return nil
	}

	pub.Status = policyv1alpha1.PodUnavailableBudgetStatus{
		CurrentAvailable:   currentAvailable,
		DesiredAvailable:   desiredAvailable,
		TotalReplicas:      expectedCount,
		UnavailableAllowed: unavailableAllowed,
		DisruptedPods:      disruptedPods,
		UnavailablePods:    unavailablePods,
		ObservedGeneration: pub.Generation,
	}
	err := r.Client.Status().Update(context.TODO(), pub)
	if err != nil {
		return err
	}
	if err = util.GlobalCache.Add(pub); err != nil {
		klog.Errorf("Add cache failed for PodUnavailableBudget(%s/%s): %s", pub.Namespace, pub.Name, err.Error())
	}
	klog.V(3).Infof("pub(%s/%s) update status(disruptedPods:%d, unavailablePods:%d, expectedCount:%d, desiredAvailable:%d, currentAvailable:%d, unavailableAllowed:%d)",
		pub.Namespace, pub.Name, len(disruptedPods), len(unavailablePods), expectedCount, desiredAvailable, currentAvailable, unavailableAllowed)
	return nil
}
