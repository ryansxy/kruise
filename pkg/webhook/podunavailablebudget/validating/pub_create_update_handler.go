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
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	policyv1alpha1 "github.com/openkruise/kruise/apis/policy/v1alpha1"
	"github.com/openkruise/kruise/pkg/features"
	"github.com/openkruise/kruise/pkg/util"
	utilfeature "github.com/openkruise/kruise/pkg/util/feature"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metavalidation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	appsvalidation "k8s.io/kubernetes/pkg/apis/apps/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/runtime/inject"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// PodUnavailableBudgetCreateUpdateHandler handles PodUnavailableBudget
type PodUnavailableBudgetCreateUpdateHandler struct {
	// To use the client, you need to do the following:
	// - uncomment it
	// - import sigs.k8s.io/controller-runtime/pkg/client
	// - uncomment the InjectClient method at the bottom of this file.
	Client client.Client

	// Decoder decodes objects
	Decoder *admission.Decoder
}

var _ admission.Handler = &PodUnavailableBudgetCreateUpdateHandler{}

/*
webhook的配置，可以看到 PodUnavailableBudgetCreateUpdateHandler 只会对Create和update pub 的事件响应；
- admissionReviewVersions:
  - v1
  - v1beta1
  clientConfig:
    service:
      name: webhook-service
      namespace: system
      path: /validate-policy-kruise-io-podunavailablebudget
  failurePolicy: Fail
  name: vpodunavailablebudget.kb.io
  rules:
  - apiGroups:
    - policy.kruise.io
    apiVersions:
    - v1alpha1
    operations:
    - CREATE
    - UPDATE
    resources:
    - podunavailablebudgets
  sideEffects: None

注:
1.如果 PodUnavailableBudgetDeleteGate 和 PodUnavailableBudgetUpdateGate 都没有开启，此 webhook 会返回错误，按照策略是Fail,则 create和 update pub 的请求会被拒绝
2.如果1步骤通过，会对本次操作的pub对象进行校验，合法则会更新，比如如果是create,只校验newObj,如果是Update,校验 newObj和oldObj
*/

// Handle handles admission requests.
func (h *PodUnavailableBudgetCreateUpdateHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	// 1.如果 pubDelete gate 没有开启 且 pubUpdate Gate (保护 pod in-place update) 没有开启；直接返回err
	if !utilfeature.DefaultFeatureGate.Enabled(features.PodUnavailableBudgetDeleteGate) && !utilfeature.DefaultFeatureGate.Enabled(features.PodUnavailableBudgetUpdateGate) {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("feature PodUnavailableBudget is invalid, please open via feature-gate(%s, %s)",
			features.PodUnavailableBudgetDeleteGate, features.PodUnavailableBudgetUpdateGate))
	}

	obj := &policyv1alpha1.PodUnavailableBudget{} // 2.decode req(新的obj)
	err := h.Decoder.Decode(req, obj)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	var old *policyv1alpha1.PodUnavailableBudget
	//when Operation is update, decode older object   3.如果Operation是update,则decode old obj
	if req.AdmissionRequest.Operation == admissionv1.Update {
		old = new(policyv1alpha1.PodUnavailableBudget)
		if err := h.Decoder.Decode(
			admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Object: req.AdmissionRequest.OldObject}},
			old); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
	}
	allErrs := h.validatingPodUnavailableBudgetFn(obj, old) //4.校验pub(newObj,oldObj),如果不合法返回错误
	if len(allErrs) != 0 {
		return admission.Errored(http.StatusBadRequest, allErrs.ToAggregate())
	}
	return admission.ValidationResponse(true, "")
}

func (h *PodUnavailableBudgetCreateUpdateHandler) validatingPodUnavailableBudgetFn(obj, old *policyv1alpha1.PodUnavailableBudget) field.ErrorList {
	// validate pub.annotations
	allErrs := field.ErrorList{}
	// 1.查看pub的保护返回,可能会有 DELETE,EVICT,UPDATE 三种
	if operationsValue, ok := obj.Annotations[policyv1alpha1.PubProtectOperationAnnotation]; ok {
		operations := strings.Split(operationsValue, ",")
		for _, operation := range operations { //1.1 如果 operation 不等于  DELETE,EVICT,UPDATE 三种 ，代表非法
			if operation != string(policyv1alpha1.PubUpdateOperation) && operation != string(policyv1alpha1.PubDeleteOperation) &&
				operation != string(policyv1alpha1.PubEvictOperation) {
				allErrs = append(allErrs, field.InternalError(field.NewPath("metadata"), fmt.Errorf("annotation[%s] is invalid", policyv1alpha1.PubProtectOperationAnnotation)))
			}
		}
	}
	// 2.从obj的Annotations上 获取obj 保护的总副本数
	if replicasValue, ok := obj.Annotations[policyv1alpha1.PubProtectTotalReplicas]; ok {
		if _, err := strconv.ParseInt(replicasValue, 10, 32); err != nil {
			allErrs = append(allErrs, field.InternalError(field.NewPath("metadata"), fmt.Errorf("annotation[%s] is invalid", policyv1alpha1.PubProtectTotalReplicas)))
		}
	}

	//validate Pub.Spec 3.验证pub spec 是否合法
	allErrs = append(allErrs, validatePodUnavailableBudgetSpec(obj, field.NewPath("spec"))...)
	// when operation is update, validating whether old and new pub conflict 4.当操作是update, 验证新旧 pub 是否冲突,其实就是new和old的 Selector 或 TargetReference 是否发生变化
	if old != nil {
		allErrs = append(allErrs, validateUpdatePubConflict(obj, old, field.NewPath("spec"))...)
	}
	// validate whether pub is in conflict with others  5.验证pub是否和其他的冲突
	pubList := &policyv1alpha1.PodUnavailableBudgetList{}
	if err := h.Client.List(context.TODO(), pubList, &client.ListOptions{Namespace: obj.Namespace}); err != nil { // 查询该namespace下所有的 pub
		allErrs = append(allErrs, field.InternalError(field.NewPath(""), fmt.Errorf("query other podUnavailableBudget failed, err: %v", err)))
	} else {
		allErrs = append(allErrs, validatePubConflict(obj, pubList.Items, field.NewPath("spec"))...)
	}
	return allErrs
}

func validateUpdatePubConflict(obj, old *policyv1alpha1.PodUnavailableBudget, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	// selector and targetRef can't be changed
	if !reflect.DeepEqual(obj.Spec.Selector, old.Spec.Selector) || !reflect.DeepEqual(obj.Spec.TargetReference, old.Spec.TargetReference) {
		allErrs = append(allErrs, field.Required(fldPath.Child("selector, targetRef"), "selector and targetRef cannot be modified"))
	}
	return allErrs
}

func validatePodUnavailableBudgetSpec(obj *policyv1alpha1.PodUnavailableBudget, fldPath *field.Path) field.ErrorList {
	spec := &obj.Spec
	allErrs := field.ErrorList{}

	// 1.如果Selector和TargetReference都为空，代表错误
	if spec.Selector == nil && spec.TargetReference == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("selector, targetRef"), "no selector or targetRef defined in PodUnavailableBudget"))
	} else if spec.Selector != nil && spec.TargetReference != nil { // 2.如果Selector和TargetReference都不为空，互斥，代表错误
		allErrs = append(allErrs, field.Required(fldPath.Child("selector, targetRef"), "selector and targetRef are mutually exclusive"))
	} else if spec.TargetReference != nil { // 3.如果TargetReference不为空，查看是否满足要求，不满足，代表错误
		if spec.TargetReference.APIVersion == "" || spec.TargetReference.Name == "" || spec.TargetReference.Kind == "" {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("TargetReference"), spec.TargetReference, "empty TargetReference is not valid for PodUnavailableBudget."))
		}
		_, err := schema.ParseGroupVersion(spec.TargetReference.APIVersion)
		if err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("TargetReference"), spec.TargetReference, err.Error()))
		}
	} else { // 4.如果 Selector 不为空，查看是否满足要求，不满足，代表错误
		allErrs = append(allErrs, metavalidation.ValidateLabelSelector(spec.Selector, fldPath.Child("selector"))...)
		if len(spec.Selector.MatchLabels)+len(spec.Selector.MatchExpressions) == 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("selector"), spec.Selector, "empty selector is not valid for PodUnavailableBudget."))
		}
		_, err := metav1.LabelSelectorAsSelector(spec.Selector)
		if err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("selector"), spec.Selector, ""))
		}
	}

	// 5.如果 MaxUnavailable  和 MinAvailable 都为nil,代表错误
	if spec.MaxUnavailable == nil && spec.MinAvailable == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("maxUnavailable, minAvailable"), "no maxUnavailable or minAvailable defined in PodUnavailableBudget"))
	} else if spec.MaxUnavailable != nil && spec.MinAvailable != nil { // 6.如果 MaxUnavailable  和 MinAvailable 都不为nil,互斥，代表错误
		allErrs = append(allErrs, field.Required(fldPath.Child("maxUnavailable, minAvailable"), "maxUnavailable and minAvailable are mutually exclusive"))
	} else if spec.MaxUnavailable != nil { // 7.ValidatePositiveIntOrPercent会判断给的值是否是int或string,如果不是，则代表错误
		allErrs = append(allErrs, appsvalidation.ValidatePositiveIntOrPercent(*spec.MaxUnavailable, fldPath.Child("maxUnavailable"))...)
		allErrs = append(allErrs, appsvalidation.IsNotMoreThan100Percent(*spec.MaxUnavailable, fldPath.Child("maxUnavailable"))...)
	} else {
		allErrs = append(allErrs, appsvalidation.ValidatePositiveIntOrPercent(*spec.MinAvailable, fldPath.Child("minAvailable"))...)
		allErrs = append(allErrs, appsvalidation.IsNotMoreThan100Percent(*spec.MinAvailable, fldPath.Child("minAvailable"))...)
	}
	return allErrs
}

// 验证当前的pub和这个namespace 的其他 pub有没有冲突
func validatePubConflict(pub *policyv1alpha1.PodUnavailableBudget, others []policyv1alpha1.PodUnavailableBudget, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	for _, other := range others {
		if pub.Name == other.Name { // 1，如果是自己，就直接忽略
			continue
		}
		// pod cannot be controlled by multiple pubs  2.pod 不能被多个 pub 控制
		if pub.Spec.TargetReference != nil && other.Spec.TargetReference != nil {
			curRef := pub.Spec.TargetReference
			otherRef := other.Spec.TargetReference
			// The previous has been verified, there is no possibility of error here
			curGv, _ := schema.ParseGroupVersion(curRef.APIVersion)
			otherGv, _ := schema.ParseGroupVersion(otherRef.APIVersion)
			if curGv.Group == otherGv.Group && curRef.Kind == otherRef.Kind && curRef.Name == otherRef.Name {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("targetReference"), pub.Spec.TargetReference, fmt.Sprintf(
					"pub.spec.targetReference is in conflict with other PodUnavailableBudget %s", other.Name)))
				return allErrs
			}
		} else if pub.Spec.Selector != nil && other.Spec.Selector != nil {
			if util.IsSelectorLooseOverlap(pub.Spec.Selector, other.Spec.Selector) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("selector"), pub.Spec.Selector, fmt.Sprintf(
					"pub.spec.selector is in conflict with other PodUnavailableBudget %s", other.Name)))
				return allErrs
			}
		}
	}
	return allErrs
}

var _ inject.Client = &PodUnavailableBudgetCreateUpdateHandler{}

// InjectClient injects the client into the PodUnavailableBudgetCreateUpdateHandler
func (h *PodUnavailableBudgetCreateUpdateHandler) InjectClient(c client.Client) error {
	h.Client = c
	return nil
}

var _ admission.DecoderInjector = &PodUnavailableBudgetCreateUpdateHandler{}

// InjectDecoder injects the decoder into the PodUnavailableBudgetCreateUpdateHandler
func (h *PodUnavailableBudgetCreateUpdateHandler) InjectDecoder(d *admission.Decoder) error {
	h.Decoder = d
	return nil
}
