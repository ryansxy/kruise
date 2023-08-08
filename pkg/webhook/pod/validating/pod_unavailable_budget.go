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

package validating

import (
	"context"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/util/dryrun"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/apis/policy"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	policyv1alpha1 "github.com/openkruise/kruise/apis/policy/v1alpha1"
	"github.com/openkruise/kruise/pkg/control/pubcontrol"
)

// +kubebuilder:rbac:groups=policy.kruise.io,resources=podunavailablebudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy.kruise.io,resources=podunavailablebudgets/status,verbs=get;update;patch

// parameters:
// 1. allowed(bool) whether to allow this request   bool值代表 是否允许这个 request的操作
// 2. reason(string)
// 3. err(error)  为 nil 代表允许这个request 的操作
func (p *PodCreateHandler) podUnavailableBudgetValidatingPod(ctx context.Context, req admission.Request) (bool, string, error) {
	var checkPod *corev1.Pod
	var dryRun bool
	var operation policyv1alpha1.PubOperation
	switch req.AdmissionRequest.Operation {
	// filter out invalid Update operation, we only validate update Pod.MetaData, Pod.Spec  一、过滤掉无效的更新操作，我们只验证 Pod.MetaData，Pod.Spec 的更新
	case admissionv1.Update:
		newPod := &corev1.Pod{}
		//decode new pod
		err := p.Decoder.Decode(req, newPod)
		if err != nil {
			return false, "", err
		}
		if newPod.Annotations[pubcontrol.PodRelatedPubAnnotation] == "" { // 1.update, 判断 newPod 有没有 kruise.io/related-pub 的  Annotation，没有则返回true （代表新的不需要检验pub）
			return true, "", nil
		}
		oldPod := &corev1.Pod{}
		if err = p.Decoder.Decode(
			admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Object: req.AdmissionRequest.OldObject}},
			oldPod); err != nil {
			return false, "", err
		}
		// the change will not cause pod unavailability, then pass     2.解析出 oldPod，判断 本次pod的更改有没有带来影响， 没有则返回true
		if !p.pubControl.IsPodUnavailableChanged(oldPod, newPod) {
			klog.V(6).Infof("validate pod(%s/%s) changed can not cause unavailability, then don't need check pub", newPod.Namespace, newPod.Name)
			return true, "", nil
		}
		checkPod = oldPod                  // 3.如果对 pod 的更改会造成影响, 用 oldPod 作为 checkPod， 并将Request.options赋值给 UpdateOptions， 进而得到 options.dryRun，并和 operation 一起作为入参，进行 PodUnavailableBudgetValidatePod校验
		options := &metav1.UpdateOptions{} // 为什么用 oldPod， 因为oldPod才是原来的定义
		err = p.Decoder.DecodeRaw(req.Options, options)
		if err != nil {
			return false, "", err
		}
		// if dry run
		dryRun = dryrun.IsDryRun(options.DryRun)
		operation = policyv1alpha1.PubUpdateOperation

	// filter out invalid Delete operation, only validate delete pods resources  // 二、过滤掉无效的删除操作，只验证删除pod资源
	case admissionv1.Delete:
		if req.AdmissionRequest.SubResource != "" { // 1.判断 request.AdmissionRequest.SubResource，如果不为空，则返回true
			klog.V(6).Infof("pod(%s/%s) AdmissionRequest operation(DELETE) subResource(%s), then admit", req.Namespace, req.Name, req.SubResource)
			return true, "", nil
		}
		checkPod = &corev1.Pod{} //2.将oldPod 赋值给 checkPod
		if err := p.Decoder.DecodeRaw(req.OldObject, checkPod); err != nil {
			return false, "", err
		}
		deletion := &metav1.DeleteOptions{} // 3.根据 req.Options 初始化 deleteOptions, 进而得到options.dryRun，并和 operation 一起作为入参，进行PodUnavailableBudgetValidatePod校验
		err := p.Decoder.DecodeRaw(req.Options, deletion)
		if err != nil {
			return false, "", err
		}
		// if dry run
		dryRun = dryrun.IsDryRun(deletion.DryRun)
		operation = policyv1alpha1.PubDeleteOperation

	// filter out invalid Create operation, only validate create pod eviction subresource
	// 三、过滤掉无效的Create操作，只验证 create pod eviction subresource
	case admissionv1.Create:
		// ignore create operation other than subresource eviction  1.判断 req.AdmissionRequest.SubResource，如果不是 eviction ，代表和驱逐无关，直接返回true
		if req.AdmissionRequest.SubResource != "eviction" {
			klog.V(6).Infof("pod(%s/%s) AdmissionRequest operation(CREATE) Resource(%s) subResource(%s), then admit", req.Namespace, req.Name, req.Resource, req.SubResource)
			return true, "", nil
		}
		eviction := &policy.Eviction{} // 2.如果 是 eviction， 则初始化 policy.Eviction
		//decode eviction
		err := p.Decoder.Decode(req, eviction)
		if err != nil {
			return false, "", err
		}
		// if dry run  3. 判断是不是 dryRun，初始化 dryRun
		if eviction.DeleteOptions != nil {
			dryRun = dryrun.IsDryRun(eviction.DeleteOptions.DryRun)
		}
		checkPod = &corev1.Pod{}
		key := types.NamespacedName{
			Namespace: req.AdmissionRequest.Namespace,
			Name:      req.AdmissionRequest.Name,
		}
		if err = p.Client.Get(ctx, key, checkPod); err != nil { // 4.根据key获取对应的Pod，如果获取失败，直接返回false
			return false, "", err
		}
		operation = policyv1alpha1.PubEvictOperation
	}

	if checkPod.Annotations[pubcontrol.PodRelatedPubAnnotation] == "" { // 四.如果得到的 checkPod 的 kruise.io/related-pub annotation 为空，直接返回true，代表pod无法校验pub
		return true, "", nil
	}
	return pubcontrol.PodUnavailableBudgetValidatePod(p.Client, p.pubControl, checkPod, operation, dryRun) // 五、进行校验
}
