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

package flavorassigner

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	"sigs.k8s.io/kueue/pkg/cache"
	"sigs.k8s.io/kueue/pkg/workload"
)

type Assignment struct {
	PodSets     []PodSetAssignment
	TotalBorrow cache.FlavorResourceQuantities

	// usedResources is the accumulated usage of resources as pod sets get
	// flavors assigned.
	usage cache.FlavorResourceQuantities

	// representativeMode is the cached representative mode for this assignment.
	representativeMode *FlavorAssignmentMode
}

func (a *Assignment) Borrows() bool {
	return len(a.TotalBorrow) > 0
}

// RepresentativeMode calculates the representative mode for the assigment as
// the worst assignment mode among all the pod sets.
func (a *Assignment) RepresentativeMode() FlavorAssignmentMode {
	if len(a.PodSets) == 0 {
		// No assignments calculated.
		return NoFit
	}
	if a.representativeMode != nil {
		return *a.representativeMode
	}
	mode := Fit
	for _, ps := range a.PodSets {
		psMode := ps.RepresentativeMode()
		if psMode < mode {
			mode = psMode
		}
	}
	a.representativeMode = &mode
	return mode
}

func (a *Assignment) Message() string {
	var builder strings.Builder
	for _, ps := range a.PodSets {
		if ps.Status == nil {
			continue
		}
		if ps.Status.IsError() {
			return fmt.Sprintf("failed to assign flavors to pod set %s: %v", ps.Name, ps.Status.err)
		}
		if builder.Len() > 0 {
			builder.WriteString("; ")
		}
		builder.WriteString("couldn't assign flavors to pod set ")
		builder.WriteString(ps.Name)
		builder.WriteString(": ")
		builder.WriteString(ps.Status.Message())
	}
	return builder.String()
}

func (a *Assignment) ToAPI() []kueue.PodSetAssignment {
	psFlavors := make([]kueue.PodSetAssignment, len(a.PodSets))
	for i := range psFlavors {
		psFlavors[i] = a.PodSets[i].toAPI()
	}
	return psFlavors
}

type Status struct {
	reasons []string
	err     error
}

func (s *Status) IsError() bool {
	return s != nil && s.err != nil
}

func (s *Status) append(r ...string) *Status {
	s.reasons = append(s.reasons, r...)
	return s
}

func (s *Status) Message() string {
	if s == nil {
		return ""
	}
	if s.err != nil {
		return s.err.Error()
	}
	sort.Strings(s.reasons)
	return strings.Join(s.reasons, ", ")
}

func (s *Status) Equal(o *Status) bool {
	if s == nil || o == nil {
		return s == o
	}
	if s.err != nil {
		return errors.Is(s.err, o.err)
	}
	return cmp.Equal(s.reasons, o.reasons, cmpopts.SortSlices(func(a, b string) bool {
		return a < b
	}))
}

// PodSetAssignment holds the assigned flavors and status messages for each of
// the resources that the pod set requests. Each assigned flavor is accompanied
// with an AssignmentMode.
// Empty .Flavors can be interpreted as NoFit mode for all the resources.
// Empty .Status can be interpreted as Fit mode for all the resources.
// .Flavors and .Status can't be empty at the same time, once PodSetAssignment
// is fully calculated.
type PodSetAssignment struct {
	Name     string
	Flavors  ResourceAssignment
	Status   *Status
	Requests corev1.ResourceList
}

// RepresentativeMode calculates the representative mode for this assignment as
// the worst assignment mode among all assigned flavors.
func (psa *PodSetAssignment) RepresentativeMode() FlavorAssignmentMode {
	if psa.Status == nil {
		return Fit
	}
	if len(psa.Flavors) == 0 {
		return NoFit
	}
	mode := Fit
	for _, flvAssignment := range psa.Flavors {
		if flvAssignment.Mode < mode {
			mode = flvAssignment.Mode
		}
	}
	return mode
}

type ResourceAssignment map[corev1.ResourceName]*FlavorAssignment

func (psa *PodSetAssignment) toAPI() kueue.PodSetAssignment {
	flavors := make(map[corev1.ResourceName]kueue.ResourceFlavorReference, len(psa.Flavors))
	for res, flvAssignment := range psa.Flavors {
		flavors[res] = flvAssignment.Name
	}
	return kueue.PodSetAssignment{
		Name:          psa.Name,
		Flavors:       flavors,
		ResourceUsage: psa.Requests,
	}
}

// FlavorAssignmentMode describes whether the flavor can be assigned immediately
// or what needs to happen so it can be assigned.
type FlavorAssignmentMode int

// The flavor assigment modes below are ordered from lowest to highest
// preference.
const (
	// NoFit means that there is not enough quota to assign this flavor.
	NoFit FlavorAssignmentMode = iota
	// Preempt means that there is not enough unused min quota in the ClusterQueue
	// or cohort. Preempting other workloads in the CluserQueue or cohort, or
	// waiting for them to finish might make it possible to assign this flavor.
	Preempt
	// Fit means that there is enough unused quota in the cohort to assign this
	// flavor.
	Fit
)

func (m FlavorAssignmentMode) String() string {
	switch m {
	case NoFit:
		return "NoFit"
	case Preempt:
		return "Preempt"
	case Fit:
		return "Fit"
	}
	return "Unknown"
}

type FlavorAssignment struct {
	Name   kueue.ResourceFlavorReference
	Mode   FlavorAssignmentMode
	borrow int64
}

// AssignFlavors assigns flavors for each of the resources requested in each pod set.
// The result for each pod set is accompanied with reasons why the flavor can't
// be assigned immediately. Each assigned flavor is accompanied with a
// FlavorAssignmentMode.
func AssignFlavors(log logr.Logger, wl *workload.Info, resourceFlavors map[kueue.ResourceFlavorReference]*kueue.ResourceFlavor, cq *cache.ClusterQueue) Assignment {
	assignment := Assignment{
		TotalBorrow: make(cache.FlavorResourceQuantities),
		PodSets:     make([]PodSetAssignment, 0, len(wl.TotalRequests)),
		usage:       make(cache.FlavorResourceQuantities),
	}
	for i, podSet := range wl.TotalRequests {
		if _, found := cq.RGByResource[corev1.ResourcePods]; found {
			podSet.Requests[corev1.ResourcePods] = int64(wl.Obj.Spec.PodSets[i].Count)
		}

		psAssignment := PodSetAssignment{
			Name:     podSet.Name,
			Flavors:  make(ResourceAssignment, len(podSet.Requests)),
			Requests: podSet.Requests.ToResourceList(),
		}

		for resName := range podSet.Requests {
			if _, found := psAssignment.Flavors[resName]; found {
				// This resource got assigned the same flavor as its resource group.
				// No need to compute again.
				continue
			}
			rg, found := cq.RGByResource[resName]
			if !found {
				psAssignment.Flavors = nil
				psAssignment.Status = &Status{
					reasons: []string{fmt.Sprintf("resource %s unavailable in ClusterQueue", resName)},
				}
				break
			}
			flavors, status := assignment.findFlavorForResourceGroup(log, rg, podSet.Requests, resourceFlavors, cq, &wl.Obj.Spec.PodSets[i].Template.Spec)
			if status.IsError() || len(flavors) == 0 {
				psAssignment.Flavors = nil
				psAssignment.Status = status
				break
			}
			psAssignment.append(flavors, status)
		}

		assignment.append(podSet.Requests, &psAssignment)
		if psAssignment.Status.IsError() || (len(podSet.Requests) > 0 && len(psAssignment.Flavors) == 0) {
			// This assignment failed, no need to continue tracking.
			assignment.TotalBorrow = nil
			return assignment
		}
	}
	if len(assignment.TotalBorrow) == 0 {
		assignment.TotalBorrow = nil
	}
	return assignment
}

func (psa *PodSetAssignment) append(flavors ResourceAssignment, status *Status) {
	for resource, assignment := range flavors {
		psa.Flavors[resource] = assignment
	}
	if psa.Status == nil {
		psa.Status = status
	} else if status != nil {
		psa.Status.reasons = append(psa.Status.reasons, status.reasons...)
	}
}

func (a *Assignment) append(requests workload.Requests, psAssignment *PodSetAssignment) {
	a.PodSets = append(a.PodSets, *psAssignment)
	for resource, flvAssignment := range psAssignment.Flavors {
		if flvAssignment.borrow > 0 {
			if a.TotalBorrow[flvAssignment.Name] == nil {
				a.TotalBorrow[flvAssignment.Name] = make(map[corev1.ResourceName]int64)
			}
			// Don't accumulate borrowing. The returned `borrow` already considers
			// usage from previous pod sets.
			a.TotalBorrow[flvAssignment.Name][resource] = flvAssignment.borrow
		}
		if a.usage[flvAssignment.Name] == nil {
			a.usage[flvAssignment.Name] = make(map[corev1.ResourceName]int64)
		}
		a.usage[flvAssignment.Name][resource] += requests[resource]
	}
}

// findFlavorForResourceGroup finds the flavor which can satisfy the resource
// request, along with the information about resources that need to be borrowed.
// If the flavor cannot be immediately assigned, it returns a status with
// reasons or failure.
func (a *Assignment) findFlavorForResourceGroup(
	log logr.Logger,
	rg *cache.ResourceGroup,
	requests workload.Requests,
	resourceFlavors map[kueue.ResourceFlavorReference]*kueue.ResourceFlavor,
	cq *cache.ClusterQueue,
	spec *corev1.PodSpec) (ResourceAssignment, *Status) {
	status := &Status{}
	requests = filterRequestedResources(requests, rg.CoveredResources)

	var bestAssignment ResourceAssignment
	bestAssignmentMode := NoFit

	// We will only check against the flavors' labels for the resource.
	selector := flavorSelector(spec, rg.LabelKeys)
	for _, flvQuotas := range rg.Flavors {
		flavor, exist := resourceFlavors[flvQuotas.Name]
		if !exist {
			log.Error(nil, "Flavor not found", "Flavor", flvQuotas.Name)
			status.append(fmt.Sprintf("flavor %s not found", flvQuotas.Name))
			continue
		}
		taint, untolerated := corev1helpers.FindMatchingUntoleratedTaint(flavor.Spec.NodeTaints, spec.Tolerations, func(t *corev1.Taint) bool {
			return t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute
		})
		if untolerated {
			status.append(fmt.Sprintf("untolerated taint %s in flavor %s", taint, flvQuotas.Name))
			continue
		}
		if match, err := selector.Match(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: flavor.Spec.NodeLabels}}); !match || err != nil {
			if err != nil {
				status.err = err
				return nil, status
			}
			status.append(fmt.Sprintf("flavor %s doesn't match node affinity", flvQuotas.Name))
			continue
		}

		assignments := make(ResourceAssignment, len(requests))
		// Calculate representativeMode for this assignment as the worst mode among all requests.
		representativeMode := Fit
		for rName, val := range requests {
			resQuota := flvQuotas.Resources[rName]
			// Check considering the flavor usage by previous pod sets.
			mode, borrow, s := fitsResourceQuota(flvQuotas.Name, rName, val+a.usage[flvQuotas.Name][rName], cq, resQuota)
			if s != nil {
				status.reasons = append(status.reasons, s.reasons...)
			}
			if mode < representativeMode {
				representativeMode = mode
			}
			if representativeMode == NoFit {
				// The flavor doesn't fit, no need to check other resources.
				break
			}

			assignments[rName] = &FlavorAssignment{
				Name:   flvQuotas.Name,
				Mode:   mode,
				borrow: borrow,
			}
		}

		if representativeMode > bestAssignmentMode {
			bestAssignment = assignments
			bestAssignmentMode = representativeMode
			if bestAssignmentMode == Fit {
				// All the resources fit in the cohort, no need to check more flavors.
				return bestAssignment, nil
			}
		}
	}
	return bestAssignment, status
}

func flavorSelector(spec *corev1.PodSpec, allowedKeys sets.Set[string]) nodeaffinity.RequiredNodeAffinity {
	// This function generally replicates the implementation of kube-scheduler's NodeAffintiy
	// Filter plugin as of v1.24.
	var specCopy corev1.PodSpec

	// Remove affinity constraints with irrelevant keys.
	if len(spec.NodeSelector) != 0 {
		specCopy.NodeSelector = map[string]string{}
		for k, v := range spec.NodeSelector {
			if allowedKeys.Has(k) {
				specCopy.NodeSelector[k] = v
			}
		}
	}

	affinity := spec.Affinity
	if affinity != nil && affinity.NodeAffinity != nil && affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		var termsCopy []corev1.NodeSelectorTerm
		for _, t := range affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			var expCopy []corev1.NodeSelectorRequirement
			for _, e := range t.MatchExpressions {
				if allowedKeys.Has(e.Key) {
					expCopy = append(expCopy, e)
				}
			}
			// If a term becomes empty, it means node affinity matches any flavor since those terms are ORed,
			// and so matching gets reduced to spec.NodeSelector
			if len(expCopy) == 0 {
				termsCopy = nil
				break
			}
			termsCopy = append(termsCopy, corev1.NodeSelectorTerm{MatchExpressions: expCopy})
		}
		if len(termsCopy) != 0 {
			specCopy.Affinity = &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: termsCopy,
					},
				},
			}
		}
	}
	return nodeaffinity.GetRequiredNodeAffinity(&corev1.Pod{Spec: specCopy})
}

// fitsResourceQuota returns how this flavor could be assigned to the resource,
// according to the remaining quota in the ClusterQueue and cohort.
// If it fits, also returns any borrowing required.
// If the flavor doesn't satisfy limits immediately (when waiting or preemption
// could help), it returns a Status with reasons.
func fitsResourceQuota(fName kueue.ResourceFlavorReference, rName corev1.ResourceName, val int64, cq *cache.ClusterQueue, rQuota *cache.ResourceQuota) (FlavorAssignmentMode, int64, *Status) {
	var status Status
	used := cq.Usage[fName][rName]
	mode := NoFit
	if val <= rQuota.Nominal {
		// The request can be satisfied by the min quota, assuming quota is
		// reclaimed from the cohort or assuming all active workloads in the
		// ClusterQueue are preempted.
		mode = Preempt
	}
	if rQuota.BorrowingLimit != nil && used+val > rQuota.Nominal+*rQuota.BorrowingLimit {
		status.append(fmt.Sprintf("borrowing limit for %s in flavor %s exceeded", rName, fName))
		return mode, 0, &status
	}

	cohortUsed := used
	cohortAvailable := rQuota.Nominal
	if cq.Cohort != nil {
		cohortUsed = cq.Cohort.Usage[fName][rName]
		cohortAvailable = cq.Cohort.RequestableResources[fName][rName]
	}

	lack := cohortUsed + val - cohortAvailable
	if lack <= 0 {
		borrow := used + val - rQuota.Nominal
		if borrow < 0 {
			borrow = 0
		}
		return Fit, borrow, nil
	}

	lackQuantity := workload.ResourceQuantity(rName, lack)
	msg := fmt.Sprintf("insufficient unused quota in cohort for %s in flavor %s, %s more needed", rName, fName, &lackQuantity)
	if cq.Cohort == nil {
		if mode == NoFit {
			msg = fmt.Sprintf("insufficient quota for %s in flavor %s in ClusterQueue", rName, fName)
		} else {
			msg = fmt.Sprintf("insufficient unused quota for %s in flavor %s, %s more needed", rName, fName, &lackQuantity)
		}
	}
	status.append(msg)
	return mode, 0, &status
}

func filterRequestedResources(req workload.Requests, allowList sets.Set[corev1.ResourceName]) workload.Requests {
	filtered := make(workload.Requests)
	for n, v := range req {
		if allowList.Has(n) {
			filtered[n] = v
		}
	}
	return filtered
}
