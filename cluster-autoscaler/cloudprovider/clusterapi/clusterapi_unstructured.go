/*
Copyright 2020 The Kubernetes Authors.

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

package clusterapi

import (
	"context"
	"fmt"
	"path"
	"time"

	"github.com/pkg/errors"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type unstructuredScalableResource struct {
	controller   *machineController
	unstructured *unstructured.Unstructured
	maxSize      int
	minSize      int
}

func (r unstructuredScalableResource) ID() string {
	return path.Join(r.Kind(), r.Namespace(), r.Name())
}

func (r unstructuredScalableResource) MaxSize() int {
	return r.maxSize
}

func (r unstructuredScalableResource) MinSize() int {
	return r.minSize
}

func (r unstructuredScalableResource) Kind() string {
	return r.unstructured.GetKind()
}

func (r unstructuredScalableResource) GroupVersionResource() (schema.GroupVersionResource, error) {
	switch r.Kind() {
	case machineDeploymentKind:
		return r.controller.machineDeploymentResource, nil
	case machineSetKind:
		return r.controller.machineSetResource, nil
	default:
		return schema.GroupVersionResource{}, fmt.Errorf("unknown scalable resource kind %s", r.Kind())
	}
}

func (r unstructuredScalableResource) Name() string {
	return r.unstructured.GetName()
}

func (r unstructuredScalableResource) Namespace() string {
	return r.unstructured.GetNamespace()
}

func (r unstructuredScalableResource) ProviderIDs() ([]string, error) {
	providerIds, err := r.controller.scalableResourceProviderIDs(r.unstructured)
	if err != nil {
		return nil, err
	}

	return providerIds, nil
}

func (r unstructuredScalableResource) Replicas() (int, error) {
	gvr, err := r.GroupVersionResource()
	if err != nil {
		return 0, err
	}

	s, err := r.controller.managementScaleClient.Scales(r.Namespace()).Get(context.TODO(), gvr.GroupResource(), r.Name(), metav1.GetOptions{})
	if err != nil {
		return 0, err
	}

	if s == nil {
		return 0, fmt.Errorf("failed to fetch resource scale: unknown %s %s/%s", r.Kind(), r.Namespace(), r.Name())
	}
	return int(s.Spec.Replicas), nil
}

func (r unstructuredScalableResource) SetSize(nreplicas int) error {
	switch {
	case nreplicas > r.maxSize:
		return fmt.Errorf("size increase too large - desired:%d max:%d", nreplicas, r.maxSize)
	case nreplicas < r.minSize:
		return fmt.Errorf("size decrease too large - desired:%d min:%d", nreplicas, r.minSize)
	}

	gvr, err := r.GroupVersionResource()
	if err != nil {
		return err
	}

	s, err := r.controller.managementScaleClient.Scales(r.Namespace()).Get(context.TODO(), gvr.GroupResource(), r.Name(), metav1.GetOptions{})
	if err != nil {
		return err
	}

	if s == nil {
		return fmt.Errorf("unknown %s %s/%s", r.Kind(), r.Namespace(), r.Name())
	}

	s.Spec.Replicas = int32(nreplicas)
	_, updateErr := r.controller.managementScaleClient.Scales(r.Namespace()).Update(context.TODO(), gvr.GroupResource(), s, metav1.UpdateOptions{})

	if updateErr == nil {
		updateErr = unstructured.SetNestedField(r.unstructured.UnstructuredContent(), int64(nreplicas), "spec", "replicas")
	}

	return updateErr
}

func (r unstructuredScalableResource) UnmarkMachineForDeletion(machine *unstructured.Unstructured) error {
	u, err := r.controller.managementClient.Resource(r.controller.machineResource).Namespace(machine.GetNamespace()).Get(context.TODO(), machine.GetName(), metav1.GetOptions{})
	if err != nil {
		return err
	}

	annotations := u.GetAnnotations()
	delete(annotations, machineDeleteAnnotationKey)
	delete(annotations, capiMachineDeleteAnnotationKey)
	u.SetAnnotations(annotations)
	_, updateErr := r.controller.managementClient.Resource(r.controller.machineResource).Namespace(u.GetNamespace()).Update(context.TODO(), u, metav1.UpdateOptions{})

	return updateErr
}

func (r unstructuredScalableResource) MarkMachineForDeletion(machine *unstructured.Unstructured) error {
	u, err := r.controller.managementClient.Resource(r.controller.machineResource).Namespace(machine.GetNamespace()).Get(context.TODO(), machine.GetName(), metav1.GetOptions{})
	if err != nil {
		return err
	}

	u = u.DeepCopy()

	annotations := u.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	annotations[machineDeleteAnnotationKey] = time.Now().String()
	annotations[capiMachineDeleteAnnotationKey] = time.Now().String()
	u.SetAnnotations(annotations)

	_, updateErr := r.controller.managementClient.Resource(r.controller.machineResource).Namespace(u.GetNamespace()).Update(context.TODO(), u, metav1.UpdateOptions{})

	return updateErr
}

func (r unstructuredScalableResource) Labels() map[string]string {
	labels, found, err := unstructured.NestedStringMap(r.unstructured.Object, "spec", "template", "spec", "metadata", "labels")
	if !found || err != nil {
		return nil
	}
	return labels
}

func (r unstructuredScalableResource) Taints() []apiv1.Taint {
	taints, found, err := unstructured.NestedSlice(r.unstructured.Object, "spec", "template", "spec", "taints")
	if !found || err != nil {
		return nil
	}
	ret := make([]apiv1.Taint, len(taints))
	for i, t := range taints {
		if v, ok := t.(apiv1.Taint); ok {
			ret[i] = v
		} else {
			// if we cannot convert the interface to a Taint, return early with zero value
			return nil
		}
	}
	return ret
}

func (r unstructuredScalableResource) CanScaleFromZero() bool {
	return scaleFromZeroEnabled(r.unstructured.GetAnnotations())
}

func (r unstructuredScalableResource) InstanceCPUCapacity() (resource.Quantity, error) {
	return parseCPUCapacity(r.unstructured.GetAnnotations())
}

func (r unstructuredScalableResource) InstanceMemoryCapacity() (resource.Quantity, error) {
	return parseMemoryCapacity(r.unstructured.GetAnnotations())
}

func (r unstructuredScalableResource) InstanceGPUCapacity() (resource.Quantity, error) {
	return parseGPUCapacity(r.unstructured.GetAnnotations())
}

func (r unstructuredScalableResource) InstanceMaxPodsCapacity() (resource.Quantity, error) {
	return parseMaxPodsCapacity(r.unstructured.GetAnnotations())
}

func newUnstructuredScalableResource(controller *machineController, u *unstructured.Unstructured) (*unstructuredScalableResource, error) {
	minSize, maxSize, err := parseScalingBounds(u.GetAnnotations())
	if err != nil {
		return nil, errors.Wrap(err, "error validating min/max annotations")
	}

	return &unstructuredScalableResource{
		controller:   controller,
		unstructured: u,
		maxSize:      maxSize,
		minSize:      minSize,
	}, nil
}
