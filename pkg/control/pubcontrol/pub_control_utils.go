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

package pubcontrol

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	policyv1alpha1 "github.com/openkruise/kruise/apis/policy/v1alpha1"
	kubeClient "github.com/openkruise/kruise/pkg/client"
	"github.com/openkruise/kruise/pkg/util"
)

const (
	// MaxUnavailablePodSize is the max size of PUB.DisruptedPods + PUB.UnavailablePods.
	MaxUnavailablePodSize = 2000
)

var ConflictRetry = wait.Backoff{
	Steps:    4,
	Duration: 500 * time.Millisecond,
	Factor:   1.0,
	Jitter:   0.1,
}

const (
	// related-pub annotation in pod
	PodRelatedPubAnnotation = "kruise.io/related-pub"
)

// parameters:
// 1. allowed(bool) indicates whether to allow this update operation  表示是否允许 update operation。 如果为true,代表可以更新，如果是false 则拒绝更新
// 2. err(error)
func PodUnavailableBudgetValidatePod(client client.Client, control PubControl, pod *corev1.Pod, operation policyv1alpha1.PubOperation, dryRun bool) (allowed bool, reason string, err error) {
	klog.V(3).Infof("validating pod(%s/%s) operation(%s) for PodUnavailableBudget", pod.Namespace, pod.Name, operation)
	// pods that contain annotations[pod.kruise.io/pub-no-protect]="true" will be ignore
	// and will no longer check the pub quota
	// 1.检查 pod 的 Annotations，如果 pub.kruise.io/no-protect = true， 则直接返回 true （代表pod不需要保护，不需要检查pub）
	if pod.Annotations[policyv1alpha1.PodPubNoProtectionAnnotation] == "true" {
		klog.V(3).Infof("pod(%s/%s) contains annotations[%s]=true, then don't need check pub", pod.Namespace, pod.Name, policyv1alpha1.PodPubNoProtectionAnnotation)
		return true, "", nil
		// If the pod is not ready, it doesn't count towards healthy and we should not decrement
	} else if !control.IsPodReady(pod) { // 2.检查 oldPod 是否Ready, 如果没有Ready,代表不用check pub， 直接返回true
		klog.V(3).Infof("pod(%s/%s) is not ready, then don't need check pub", pod.Namespace, pod.Name)
		return true, "", nil
	}

	// pub for pod
	pub, err := control.GetPubForPod(pod)
	if err != nil { // 3.1 获取pod pub,如果失败，返回false
		return false, "", err
		// if there is no matching PodUnavailableBudget, just return true
	} else if pub == nil { //3.2 如果没有err,但pub为nil，没有匹配的pub，不用校验，返回 true
		return true, "", nil
		// if desired available == 0, then allow all request  3.3 判断 pub DesiredAvailable， 如果为0， 返回true
	} else if pub.Status.DesiredAvailable == 0 {
		return true, "", nil
	} else if !isNeedPubProtection(pub, operation) { // 3.4 判断pub的操作是否需要保护，如果不需要,返回 true；// 判断当前pub的操作是否需要 portect， Annotation 不存在则允许全部的操作，如果存在，看当前操作是否被允许
		klog.V(3).Infof("pod(%s/%s) operation(%s) is not in pub(%s) protection", pod.Namespace, pod.Name, pub.Name)
		return true, "", nil
		// pod is in pub.Status.DisruptedPods or pub.Status.UnavailablePods, then don't need check it
	} else if isPodRecordedInPub(pod.Name, pub) { // 3.5 判断pod是否已经被处理，如果 pod 在 pub.Status.DisruptedPods 或 pub.Status.UnavailablePods 中，则不需要检查
		klog.V(3).Infof("pod(%s/%s) already is recorded in pub(%s/%s)", pod.Namespace, pod.Name, pub.Namespace, pub.Name)
		return true, "", nil
	}
	// check and decrement pub quota
	var conflictTimes int
	var costOfGet, costOfUpdate time.Duration
	refresh := false
	var pubClone *policyv1alpha1.PodUnavailableBudget
	err = retry.RetryOnConflict(ConflictRetry, func() error {
		unlock := util.GlobalKeyedMutex.Lock(string(pub.UID))
		defer unlock()

		start := time.Now()
		if refresh { // 4.1 重新获取 pub obj
			pubClone, err = kubeClient.GetGenericClient().KruiseClient.PolicyV1alpha1().
				PodUnavailableBudgets(pub.Namespace).Get(context.TODO(), pub.Name, metav1.GetOptions{})
			if err != nil {
				klog.Errorf("Get PodUnavailableBudget(%s/%s) failed form etcd: %s", pub.Namespace, pub.Name, err.Error())
				return err
			}
		} else {
			// compare local cache and informer cache, then get the newer one  4.2 比较 local cache 和 informer cache，然后获取较新的缓存
			item, _, err := util.GlobalCache.Get(pub)
			if err != nil {
				klog.Errorf("Get cache failed for PodUnavailableBudget(%s/%s): %s", pub.Namespace, pub.Name, err.Error())
			}
			if localCached, ok := item.(*policyv1alpha1.PodUnavailableBudget); ok {
				pubClone = localCached.DeepCopy()
			} else {
				pubClone = pub.DeepCopy()
			}

			informerCached := &policyv1alpha1.PodUnavailableBudget{}
			if err := client.Get(context.TODO(), types.NamespacedName{Namespace: pub.Namespace,
				Name: pub.Name}, informerCached); err == nil {
				var localRV, informerRV int64
				_ = runtime.Convert_string_To_int64(&pubClone.ResourceVersion, &localRV, nil)
				_ = runtime.Convert_string_To_int64(&informerCached.ResourceVersion, &informerRV, nil)
				if informerRV > localRV {
					pubClone = informerCached
				}
			}
		}
		costOfGet += time.Since(start) // 4.3 计算 get costTime

		// Try to verify-and-decrement
		// If it was false already, or if it becomes false during the course of our retries,
		// 4.4 尝试  verify-and-decrement，
		err := checkAndDecrement(pod.Name, pubClone, operation)
		if err != nil {
			return err
		}

		// If this is a dry-run, we don't need to go any further than that.  4.5 如果这是试运行，我们不需要做更多的事情。
		if dryRun {
			klog.V(3).Infof("pod(%s) operation for pub(%s/%s) is a dry run", pod.Name, pubClone.Namespace, pubClone.Name)
			return nil
		}
		// pub ns/name status 字段打印出来
		klog.V(3).Infof("pub(%s/%s) update status(disruptedPods:%d, unavailablePods:%d, expectedCount:%d, desiredAvailable:%d, currentAvailable:%d, unavailableAllowed:%d)",
			pubClone.Namespace, pubClone.Name, len(pubClone.Status.DisruptedPods), len(pubClone.Status.UnavailablePods),
			pubClone.Status.TotalReplicas, pubClone.Status.DesiredAvailable, pubClone.Status.CurrentAvailable, pubClone.Status.UnavailableAllowed)
		start = time.Now()
		err = client.Status().Update(context.TODO(), pubClone) // 4.6 调用 client 更新 pubClone
		costOfUpdate += time.Since(start)                      // 4.7 计算update time
		if err == nil {
			if err = util.GlobalCache.Add(pubClone); err != nil {
				klog.Errorf("Add cache failed for PodUnavailableBudget(%s/%s): %s", pub.Namespace, pub.Name, err.Error())
			}
			return nil
		}
		// if conflict, then retry
		conflictTimes++
		refresh = true
		return err
	})
	klog.V(3).Infof("Webhook cost of pub(%s/%s): conflict times %v, cost of Get %v, cost of Update %v",
		pub.Namespace, pub.Name, conflictTimes, costOfGet, costOfUpdate)
	// 4.8 日志 webhook 每个阶段消耗的时间，如果 err!=nil ,则返回false
	if err != nil && err != wait.ErrWaitTimeout {
		klog.V(3).Infof("pod(%s/%s) operation(%s) for pub(%s/%s) failed: %s", pod.Namespace, pod.Name, operation, pub.Namespace, pub.Name, err.Error())
		return false, err.Error(), nil
	} else if err == wait.ErrWaitTimeout {
		err = errors.NewTimeoutError(fmt.Sprintf("couldn't update PodUnavailableBudget %s due to conflicts", pub.Name), 10)
		klog.Errorf("pod(%s/%s) operation(%s) failed: %s", pod.Namespace, pod.Name, operation, err.Error())
		return false, err.Error(), nil
	}

	klog.V(3).Infof("admit pod(%s/%s) operation(%s) for pub(%s/%s)", pod.Namespace, pod.Name, operation, pub.Namespace, pub.Name)
	return true, "", nil
}

// 校验是否合法，并将 UnavailableAllowed --; pub 传入的是 指针
func checkAndDecrement(podName string, pub *policyv1alpha1.PodUnavailableBudget, operation policyv1alpha1.PubOperation) error {
	// 1.1 异常情况校验，检查 pub.status.UnavailableAllowed, 如果<=0,代表不可用的为负数，不能被允许，返回err
	if pub.Status.UnavailableAllowed <= 0 {
		return errors.NewForbidden(policyv1alpha1.Resource("podunavailablebudget"), pub.Name, fmt.Errorf("pub unavailable allowed is negative"))
	}
	// 1.2 异常情况校验，检查 pub.status.如果DisruptedPods和UnavailablePods 如果两者的pod的个数加起来 小于MaxUnavailablePodSize，返回err
	if len(pub.Status.DisruptedPods)+len(pub.Status.UnavailablePods) > MaxUnavailablePodSize {
		return errors.NewForbidden(policyv1alpha1.Resource("podunavailablebudget"), pub.Name, fmt.Errorf("DisruptedPods and UnavailablePods map too big - too many unavailable not confirmed by PUB controller"))
	}

	// 2. 将当前的 pub.Status.UnavailableAllowed--- , UnavailableAllowed 是 当前允许的不可用的pod数量
	pub.Status.UnavailableAllowed--

	if pub.Status.DisruptedPods == nil {
		pub.Status.DisruptedPods = make(map[string]metav1.Time)
	}
	if pub.Status.UnavailablePods == nil {
		pub.Status.UnavailablePods = make(map[string]metav1.Time)
	}

	// 3.根据 operation 判断符合更新pub.Status.UnavailablePods 和 DisruptedPods 的 map[podName]Time
	// 1)如果 operation = update, 则将pod更新到， Pod.Status.UnavailablePods[podName]=time.Now;
	// 2)如果是 operation 为create 或 delete,则将pod更新到， DisruptedPods[podName]=time.Now;
	if operation == policyv1alpha1.PubUpdateOperation {
		pub.Status.UnavailablePods[podName] = metav1.Time{Time: time.Now()}
		klog.V(3).Infof("pod(%s) is recorded in pub(%s/%s) UnavailablePods", podName, pub.Namespace, pub.Name)
	} else {
		pub.Status.DisruptedPods[podName] = metav1.Time{Time: time.Now()}
		klog.V(3).Infof("pod(%s) is recorded in pub(%s/%s) DisruptedPods", podName, pub.Namespace, pub.Name)
	}
	return nil
}

// pod 在pub.status UnavailablePods 和 DisruptedPods 中存在
func isPodRecordedInPub(podName string, pub *policyv1alpha1.PodUnavailableBudget) bool {
	if _, ok := pub.Status.UnavailablePods[podName]; ok {
		return true
	}
	if _, ok := pub.Status.DisruptedPods[podName]; ok {
		return true
	}
	return false
}

// check APIVersion, Kind, Name
func IsReferenceEqual(ref1, ref2 *policyv1alpha1.TargetReference) bool {
	gv1, err := schema.ParseGroupVersion(ref1.APIVersion)
	if err != nil {
		return false
	}
	gv2, err := schema.ParseGroupVersion(ref2.APIVersion)
	if err != nil {
		return false
	}
	return gv1.Group == gv2.Group && ref1.Kind == ref2.Kind && ref1.Name == ref2.Name
}

// 判断当前pub的操作是否需要 portect， Annotation 不存在则允许全部的操作，如果存在，看当前操作是否被允许
func isNeedPubProtection(pub *policyv1alpha1.PodUnavailableBudget, operation policyv1alpha1.PubOperation) bool {
	operationValue, ok := pub.Annotations[policyv1alpha1.PubProtectOperationAnnotation] //  1.如果 pub 的 kruise.io/pub-protect-operations Annotations 不存在 或 为""，返回true；  即是默认值
	if !ok || operationValue == "" {
		return true
	}
	operations := sets.NewString(strings.Split(operationValue, ",")...) // 2.分割，并判断其中的值 是否包含 当前的 operation
	return operations.Has(string(operation))
}
