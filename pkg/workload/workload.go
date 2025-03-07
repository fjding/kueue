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

package workload

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	"sigs.k8s.io/kueue/pkg/constants"
	"sigs.k8s.io/kueue/pkg/util/api"
	"sigs.k8s.io/kueue/pkg/util/limitrange"
)

var (
	admissionManagedConditions = []string{kueue.WorkloadAdmitted, kueue.WorkloadEvicted}
)

// Info holds a Workload object and some pre-processing.
type Info struct {
	Obj *kueue.Workload
	// list of total resources requested by the podsets.
	TotalRequests []PodSetResources
	// Populated from the queue during admission or from the admission field if
	// already admitted.
	ClusterQueue string
}

type PodSetResources struct {
	Name     string
	Requests Requests
	Flavors  map[corev1.ResourceName]kueue.ResourceFlavorReference
}

func NewInfo(w *kueue.Workload) *Info {
	info := &Info{
		Obj: w,
	}
	if w.Status.Admission != nil {
		info.ClusterQueue = string(w.Status.Admission.ClusterQueue)
		info.TotalRequests = totalRequestsFromAdmission(w)
	} else {
		info.TotalRequests = totalRequestsFromPodSets(w)
	}
	return info
}

func (i *Info) Update(wl *kueue.Workload) {
	i.Obj = wl
}

func Key(w *kueue.Workload) string {
	return fmt.Sprintf("%s/%s", w.Namespace, w.Name)
}

func QueueKey(w *kueue.Workload) string {
	return fmt.Sprintf("%s/%s", w.Namespace, w.Spec.QueueName)
}

func totalRequestsFromPodSets(wl *kueue.Workload) []PodSetResources {
	if len(wl.Spec.PodSets) == 0 {
		return nil
	}
	res := make([]PodSetResources, 0, len(wl.Spec.PodSets))

	for _, ps := range wl.Spec.PodSets {
		setRes := PodSetResources{
			Name: ps.Name,
		}
		setRes.Requests = newRequests(limitrange.TotalRequests(&ps.Template.Spec))
		setRes.Requests.scale(int64(ps.Count))
		res = append(res, setRes)
	}
	return res
}

func totalRequestsFromAdmission(wl *kueue.Workload) []PodSetResources {
	if wl.Status.Admission == nil {
		return nil
	}
	res := make([]PodSetResources, 0, len(wl.Spec.PodSets))
	for _, ps := range wl.Status.Admission.PodSetAssignments {
		setRes := PodSetResources{
			Name: ps.Name,
		}
		setRes.Flavors = ps.Flavors
		setRes.Requests = newRequests(ps.ResourceUsage)
		res = append(res, setRes)
	}
	return res
}

// The following resources calculations are inspired on
// https://github.com/kubernetes/kubernetes/blob/master/pkg/scheduler/framework/types.go

// Requests maps ResourceName to flavor to value; for CPU it is tracked in MilliCPU.
type Requests map[corev1.ResourceName]int64

func newRequests(rl corev1.ResourceList) Requests {
	r := Requests{}
	for name, quant := range rl {
		r[name] = ResourceValue(name, quant)
	}
	return r
}

func (r Requests) ToResourceList() corev1.ResourceList {
	ret := make(corev1.ResourceList, len(r))
	for k, v := range r {
		ret[k] = ResourceQuantity(k, v)
	}
	return ret
}

// ResourceValue returns the integer value for the resource name.
// It's milli-units for CPU and absolute units for everything else.
func ResourceValue(name corev1.ResourceName, q resource.Quantity) int64 {
	if name == corev1.ResourceCPU {
		return q.MilliValue()
	}
	return q.Value()
}

func ResourceQuantity(name corev1.ResourceName, v int64) resource.Quantity {
	switch name {
	case corev1.ResourceCPU:
		return *resource.NewMilliQuantity(v, resource.DecimalSI)
	case corev1.ResourceMemory, corev1.ResourceEphemeralStorage:
		return *resource.NewQuantity(v, resource.BinarySI)
	default:
		if strings.HasPrefix(string(name), corev1.ResourceHugePagesPrefix) {
			return *resource.NewQuantity(v, resource.BinarySI)
		}
		return *resource.NewQuantity(v, resource.DecimalSI)
	}
}

func (r Requests) scale(f int64) {
	for name := range r {
		r[name] *= f
	}
}

// UpdateStatus updates the condition of a workload with ssa,
// filelManager being set to managerPrefix + "-" + conditionType
func UpdateStatus(ctx context.Context,
	c client.Client,
	wl *kueue.Workload,
	conditionType string,
	conditionStatus metav1.ConditionStatus,
	reason, message string,
	managerPrefix string) error {
	now := metav1.Now()
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             conditionStatus,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            api.TruncateConditionMessage(message),
	}

	newWl := BaseSSAWorkload(wl)
	newWl.Status.Conditions = []metav1.Condition{condition}
	return c.Status().Patch(ctx, newWl, client.Apply, client.FieldOwner(managerPrefix+"-"+condition.Type))
}

func UnsetAdmissionWithCondition(wl *kueue.Workload, reason, message string) {
	condition := metav1.Condition{
		Type:               kueue.WorkloadAdmitted,
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            api.TruncateConditionMessage(message),
	}
	apimeta.SetStatusCondition(&wl.Status.Conditions, condition)
	wl.Status.Admission = nil
}

// BaseSSAWorkload creates a new object based on the input workload that
// only contains the fields necessary to identify the original object.
// The object can be used in as a base for Server-Side-Apply.
func BaseSSAWorkload(w *kueue.Workload) *kueue.Workload {
	wlCopy := &kueue.Workload{
		ObjectMeta: metav1.ObjectMeta{
			UID:        w.UID,
			Name:       w.Name,
			Namespace:  w.Namespace,
			Generation: w.Generation, // Produce a conflict if there was a change in the spec.
		},
		TypeMeta: w.TypeMeta,
	}
	if wlCopy.APIVersion == "" {
		wlCopy.APIVersion = kueue.GroupVersion.String()
	}
	if wlCopy.Kind == "" {
		wlCopy.Kind = "Workload"
	}
	return wlCopy
}

// SetAdmission applies the provided admission to the workload.
// The WorkloadAdmitted and WorkloadEvicted are added or updated if necessary.
func SetAdmission(w *kueue.Workload, admission *kueue.Admission) {
	w.Status.Admission = admission
	admittedCond := metav1.Condition{
		Type:               kueue.WorkloadAdmitted,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "Admitted",
		Message:            fmt.Sprintf("Admitted by ClusterQueue %s", w.Status.Admission.ClusterQueue),
	}
	apimeta.SetStatusCondition(&w.Status.Conditions, admittedCond)

	//reset Evicted condition if present.
	if evictedCond := apimeta.FindStatusCondition(w.Status.Conditions, kueue.WorkloadEvicted); evictedCond != nil {
		evictedCond.Status = metav1.ConditionFalse
		evictedCond.LastTransitionTime = metav1.Now()
	}
}

func SetEvictedCondition(w *kueue.Workload, reason string, message string) {
	condition := metav1.Condition{
		Type:               kueue.WorkloadEvicted,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
	apimeta.SetStatusCondition(&w.Status.Conditions, condition)
}

// admissionPatch creates a new object based on the input workload that contains
// the admission and related conditions. The object can be used in Server-Side-Apply.
func admissionPatch(w *kueue.Workload) *kueue.Workload {
	wlCopy := BaseSSAWorkload(w)

	wlCopy.Status.Admission = w.Status.Admission.DeepCopy()
	for _, conditionName := range admissionManagedConditions {
		if existing := apimeta.FindStatusCondition(w.Status.Conditions, conditionName); existing != nil {
			wlCopy.Status.Conditions = append(wlCopy.Status.Conditions, *existing.DeepCopy())
		}
	}
	return wlCopy
}

// ApplyAdmissionStatus updated all the admission related status fields of a workload with SSA.
// if strict is true, resourceVersion will be part of the patch, make this call fail if Workload
// was changed.
func ApplyAdmissionStatus(ctx context.Context, c client.Client, w *kueue.Workload, strict bool) error {
	patch := admissionPatch(w)
	if strict {
		patch.ResourceVersion = w.ResourceVersion
	}
	return c.Status().Patch(ctx, patch, client.Apply, client.FieldOwner(constants.AdmissionName))
}

// GetQueueOrderTimestamp return the timestamp to be used by the scheduler. It could
// be the workload creation time or the last time a PodsReady timeout has occurred.
func GetQueueOrderTimestamp(w *kueue.Workload) *metav1.Time {
	if c := apimeta.FindStatusCondition(w.Status.Conditions, kueue.WorkloadEvicted); c != nil && c.Status == metav1.ConditionTrue && c.Reason == kueue.WorkloadEvictedByPodsReadyTimeout {
		return &c.LastTransitionTime
	}
	return &w.CreationTimestamp
}

// IsAdmitted checks if workload is admitted based on conditions
func IsAdmitted(w *kueue.Workload) bool {
	return apimeta.IsStatusConditionTrue(w.Status.Conditions, kueue.WorkloadAdmitted)
}
