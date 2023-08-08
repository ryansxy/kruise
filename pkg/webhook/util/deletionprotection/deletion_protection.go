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

package deletionprotection

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	v1 "k8s.io/api/core/v1"
	kubecontroller "k8s.io/kubernetes/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/openkruise/kruise/apis/policy/v1alpha1"
	"github.com/openkruise/kruise/pkg/features"
	utilfeature "github.com/openkruise/kruise/pkg/util/feature"
)

// builtin_handlers、cloneset、StatefulSet、UnitedDeployment
func ValidateWorkloadDeletion(obj metav1.Object, replicas *int32) error {
	// 1.如果 ResourcesDeletionProtection gate 没有开启，或 workload.DeletionTimestamp!=nil，则返回nil
	if !utilfeature.DefaultFeatureGate.Enabled(features.ResourcesDeletionProtection) || obj == nil || obj.GetDeletionTimestamp() != nil {
		return nil
	}
	// 2.得到 对象 policy.kruise.io/delete-protection 的 label
	//   如果value=always,表示该对象将始终被禁止删除，除非标签被删除。
	//   如果value=Cascading,表示如果该对象拥有活动资源，则将禁止删除该对象。
	//  注：因为传的replicas是spec.replicas，所以如果value=Cascading，我们需要将deployment 缩容为0，才能删除，反之如果value=Always,直接将label移除即可
	switch val := obj.GetLabels()[policyv1alpha1.DeletionProtectionKey]; val {
	case policyv1alpha1.DeletionProtectionTypeAlways:
		return fmt.Errorf("forbidden by ResourcesProtectionDeletion for %s=%s", policyv1alpha1.DeletionProtectionKey, val)
	case policyv1alpha1.DeletionProtectionTypeCascading:
		if replicas != nil && *replicas > 0 {
			return fmt.Errorf("forbidden by ResourcesProtectionDeletion for %s=%s and replicas %d>0", policyv1alpha1.DeletionProtectionKey, val, *replicas)
		}
	default:
	}
	return nil
}

func ValidateNamespaceDeletion(c client.Client, namespace *v1.Namespace) error {
	// 1.如果 ResourcesDeletionProtection gate 没有开启，或 namespace.DeletionTimestamp!=nil，则返回nil
	if !utilfeature.DefaultFeatureGate.Enabled(features.ResourcesDeletionProtection) || namespace.DeletionTimestamp != nil {
		return nil
	}
	// 2.如果namespace 有 DeletionProtectionKey label
	switch val := namespace.Labels[policyv1alpha1.DeletionProtectionKey]; val {
	// 2.1 如果 label = always, 返回 err,除非 label 删除
	case policyv1alpha1.DeletionProtectionTypeAlways:
		return fmt.Errorf("forbidden by ResourcesProtectionDeletion for %s=%s", policyv1alpha1.DeletionProtectionKey, val)
	case policyv1alpha1.DeletionProtectionTypeCascading:
		// 2.2 如果label=Cascading
		// 获取这个ns下所有的Pod,如果有pod的状态是active,则拒绝删除。
		// 如果pod 不是 succeeded、failed 或 pod.DeletionTimestamp!=nil,代表 pod 是 active 的
		pods := v1.PodList{}
		if err := c.List(context.TODO(), &pods, client.InNamespace(namespace.Name)); err != nil {
			return fmt.Errorf("forbidden by ResourcesProtectionDeletion for list pods error: %v", err)
		}
		var activeCount int
		for i := range pods.Items {
			pod := &pods.Items[i]
			if kubecontroller.IsPodActive(pod) {
				activeCount++
			}
		}
		if activeCount > 0 {
			return fmt.Errorf("forbidden by ResourcesProtectionDeletion for %s=%s and active pods %d>0", policyv1alpha1.DeletionProtectionKey, val, activeCount)
		}
	default:
	}
	return nil
}

// CustomResourceDefinition 删除时，需要判断其下面的cr资源是不是活跃的
// 如果 cr 资源的 DeletionTimestamp 不为 nil，则代表是active的
func ValidateCRDDeletion(c client.Client, obj metav1.Object, gvk schema.GroupVersionKind) error {
	// 1.如果 ResourcesDeletionProtection gate 没有开启，或 crd.DeletionTimestamp!=nil，则返回nil
	if !utilfeature.DefaultFeatureGate.Enabled(features.ResourcesDeletionProtection) || obj.GetDeletionTimestamp() != nil {
		return nil
	}
	switch val := obj.GetLabels()[policyv1alpha1.DeletionProtectionKey]; val {
	case policyv1alpha1.DeletionProtectionTypeAlways:
		return fmt.Errorf("forbidden by ResourcesProtectionDeletion for %s=%s", policyv1alpha1.DeletionProtectionKey, val)
	case policyv1alpha1.DeletionProtectionTypeCascading:
		objList := &unstructured.UnstructuredList{}
		objList.SetAPIVersion(gvk.GroupVersion().String())
		objList.SetKind(gvk.Kind)
		if err := c.List(context.TODO(), objList, client.InNamespace(v1.NamespaceAll)); err != nil {
			return fmt.Errorf("failed to list CRs of %v: %v", gvk, err)
		}

		var activeCount int
		for i := range objList.Items {
			if objList.Items[i].GetDeletionTimestamp() == nil {
				activeCount++
			}
		}
		if activeCount > 0 {
			return fmt.Errorf("forbidden by ResourcesProtectionDeletion for %s=%s and active CRs %d>0", policyv1alpha1.DeletionProtectionKey, val, activeCount)
		}
	default:
	}
	return nil
}
