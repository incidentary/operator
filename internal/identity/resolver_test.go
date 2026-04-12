/*
Copyright 2026.

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

package identity_test

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/incidentary/operator/internal/identity"
)

const kindDeployment = "Deployment"

// newScheme returns a runtime.Scheme registered with corev1 and appsv1.
func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1.AddToScheme: %v", err)
	}
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatalf("appsv1.AddToScheme: %v", err)
	}
	return s
}

// newFakeClient constructs a fake client populated with initObjs.
func newFakeClient(t *testing.T, initObjs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(initObjs...).
		Build()
}

func ptrBool(b bool) *bool { return &b }

func TestResolve_DeploymentAnnotation(t *testing.T) {
	t.Parallel()

	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "payments",
			Namespace: "prod",
			Annotations: map[string]string{
				identity.ServiceIDAnnotation: "payment-service",
			},
		},
	}
	r := identity.NewResolver(newFakeClient(t, d))

	got, err := r.Resolve(context.Background(), d)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ServiceID != "payment-service" {
		t.Fatalf("ServiceID = %q, want %q", got.ServiceID, "payment-service")
	}
	if got.Source != identity.SourceAnnotation {
		t.Fatalf("Source = %q, want %q", got.Source, identity.SourceAnnotation)
	}
	if got.OwnerKind != kindDeployment {
		t.Fatalf("OwnerKind = %q, want %q", got.OwnerKind, kindDeployment)
	}
	if got.OwnerName != "payments" {
		t.Fatalf("OwnerName = %q, want %q", got.OwnerName, "payments")
	}
	if got.Namespace != "prod" {
		t.Fatalf("Namespace = %q, want %q", got.Namespace, "prod")
	}
}

func TestResolve_DeploymentWorkloadName(t *testing.T) {
	t.Parallel()

	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "orders",
			Namespace: "prod",
		},
	}
	r := identity.NewResolver(newFakeClient(t, d))

	got, err := r.Resolve(context.Background(), d)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ServiceID != "orders" {
		t.Fatalf("ServiceID = %q, want %q", got.ServiceID, "orders")
	}
	if got.Source != identity.SourceWorkloadName {
		t.Fatalf("Source = %q, want %q", got.Source, identity.SourceWorkloadName)
	}
}

func TestResolve_PodWalksToDeployment(t *testing.T) {
	t.Parallel()

	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "checkout",
			Namespace: "prod",
			Annotations: map[string]string{
				identity.ServiceIDAnnotation: "checkout-svc",
			},
		},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "checkout-7d9f6b",
			Namespace: "prod",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "checkout",
					Controller: ptrBool(true),
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "checkout-7d9f6b-xkp2m",
			Namespace: "prod",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "ReplicaSet",
					Name:       "checkout-7d9f6b",
					Controller: ptrBool(true),
				},
			},
		},
	}
	r := identity.NewResolver(newFakeClient(t, d, rs, pod))

	got, err := r.Resolve(context.Background(), pod)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ServiceID != "checkout-svc" {
		t.Fatalf("ServiceID = %q, want %q", got.ServiceID, "checkout-svc")
	}
	if got.Source != identity.SourceAnnotation {
		t.Fatalf("Source = %q, want %q", got.Source, identity.SourceAnnotation)
	}
	if got.OwnerKind != kindDeployment {
		t.Fatalf("OwnerKind = %q, want %q", got.OwnerKind, kindDeployment)
	}
	if got.OwnerName != "checkout" {
		t.Fatalf("OwnerName = %q, want %q", got.OwnerName, "checkout")
	}
}

func TestResolve_PodOwnedByStatefulSet(t *testing.T) {
	t.Parallel()

	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "postgres",
			Namespace: "data",
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "postgres-0",
			Namespace: "data",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "StatefulSet",
					Name:       "postgres",
					Controller: ptrBool(true),
				},
			},
		},
	}
	r := identity.NewResolver(newFakeClient(t, ss, pod))

	got, err := r.Resolve(context.Background(), pod)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ServiceID != "postgres" {
		t.Fatalf("ServiceID = %q, want %q", got.ServiceID, "postgres")
	}
	if got.OwnerKind != "StatefulSet" {
		t.Fatalf("OwnerKind = %q, want %q", got.OwnerKind, "StatefulSet")
	}
	if got.Source != identity.SourceWorkloadName {
		t.Fatalf("Source = %q, want %q", got.Source, identity.SourceWorkloadName)
	}
}

func TestResolve_PodOwnedByDaemonSet(t *testing.T) {
	t.Parallel()

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fluent-bit",
			Namespace: "logging",
			Annotations: map[string]string{
				identity.ServiceIDAnnotation: "log-collector",
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fluent-bit-abcde",
			Namespace: "logging",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "DaemonSet",
					Name:       "fluent-bit",
					Controller: ptrBool(true),
				},
			},
		},
	}
	r := identity.NewResolver(newFakeClient(t, ds, pod))

	got, err := r.Resolve(context.Background(), pod)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ServiceID != "log-collector" {
		t.Fatalf("ServiceID = %q, want %q", got.ServiceID, "log-collector")
	}
	if got.OwnerKind != "DaemonSet" {
		t.Fatalf("OwnerKind = %q, want %q", got.OwnerKind, "DaemonSet")
	}
	if got.Source != identity.SourceAnnotation {
		t.Fatalf("Source = %q, want %q", got.Source, identity.SourceAnnotation)
	}
}

func TestResolve_NakedPod(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "debug-pod",
			Namespace: "default",
		},
	}
	r := identity.NewResolver(newFakeClient(t, pod))

	got, err := r.Resolve(context.Background(), pod)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ServiceID != "debug-pod" {
		t.Fatalf("ServiceID = %q, want %q", got.ServiceID, "debug-pod")
	}
	if got.OwnerKind != "Pod" {
		t.Fatalf("OwnerKind = %q, want %q", got.OwnerKind, "Pod")
	}
	if got.Source != identity.SourceWorkloadName {
		t.Fatalf("Source = %q, want %q", got.Source, identity.SourceWorkloadName)
	}
}

func TestResolve_OrphanedReplicaSet(t *testing.T) {
	t.Parallel()

	// A ReplicaSet whose owning Deployment was deleted — the RS still exists
	// and its owner reference dangles.
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "legacy-rs",
			Namespace: "prod",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "legacy",
					Controller: ptrBool(true),
				},
			},
			Annotations: map[string]string{
				identity.ServiceIDAnnotation: "legacy-service",
			},
		},
	}
	// The Deployment is deliberately absent from the fake client.
	r := identity.NewResolver(newFakeClient(t, rs))

	got, err := r.Resolve(context.Background(), rs)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ServiceID != "legacy-service" {
		t.Fatalf("ServiceID = %q, want %q", got.ServiceID, "legacy-service")
	}
	if got.OwnerKind != "ReplicaSet" {
		t.Fatalf("OwnerKind = %q, want %q", got.OwnerKind, "ReplicaSet")
	}
}

func TestResolve_PodOrphanedReplicaSet(t *testing.T) {
	t.Parallel()

	// The Pod references a ReplicaSet that no longer exists.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "orphan-xyz",
			Namespace: "prod",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "ReplicaSet",
					Name:       "missing-rs",
					Controller: ptrBool(true),
				},
			},
		},
	}
	r := identity.NewResolver(newFakeClient(t, pod))

	got, err := r.Resolve(context.Background(), pod)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ServiceID != "orphan-xyz" {
		t.Fatalf("ServiceID = %q, want pod fallback %q", got.ServiceID, "orphan-xyz")
	}
	if got.OwnerKind != "Pod" {
		t.Fatalf("OwnerKind = %q, want %q", got.OwnerKind, "Pod")
	}
}

func TestResolveRef_Pod(t *testing.T) {
	t.Parallel()

	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "checkout",
			Namespace: "prod",
		},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "checkout-abcde",
			Namespace: "prod",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Deployment", Name: "checkout", Controller: ptrBool(true)},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "checkout-abcde-xyz",
			Namespace: "prod",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "checkout-abcde", Controller: ptrBool(true)},
			},
		},
	}
	r := identity.NewResolver(newFakeClient(t, d, rs, pod))

	got, err := r.ResolveRef(context.Background(), "Pod", "prod", "checkout-abcde-xyz")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if got.ServiceID != "checkout" {
		t.Fatalf("ServiceID = %q, want %q", got.ServiceID, "checkout")
	}
	if got.OwnerKind != kindDeployment {
		t.Fatalf("OwnerKind = %q, want %q", got.OwnerKind, kindDeployment)
	}
}

func TestResolveRef_MissingObject(t *testing.T) {
	t.Parallel()

	r := identity.NewResolver(newFakeClient(t))

	got, err := r.ResolveRef(context.Background(), "Deployment", "prod", "ghost")
	if err != nil {
		t.Fatalf("ResolveRef returned error on NotFound: %v", err)
	}
	if !got.Empty() {
		t.Fatalf("ResolveRef should return empty Result for missing object, got %+v", got)
	}
}

func TestResolveRef_Node(t *testing.T) {
	t.Parallel()

	r := identity.NewResolver(newFakeClient(t))

	got, err := r.ResolveRef(context.Background(), "Node", "", "worker-1")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if got.ServiceID != "worker-1" {
		t.Fatalf("ServiceID = %q, want %q", got.ServiceID, "worker-1")
	}
	if got.OwnerKind != "Node" {
		t.Fatalf("OwnerKind = %q, want %q", got.OwnerKind, "Node")
	}
}

func TestResolve_NilObject(t *testing.T) {
	t.Parallel()

	r := identity.NewResolver(newFakeClient(t))

	got, err := r.Resolve(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resolve(nil): %v", err)
	}
	if !got.Empty() {
		t.Fatalf("Resolve(nil) should return empty Result, got %+v", got)
	}
}

func TestNewResolver_PanicsOnNil(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewResolver(nil) did not panic")
		}
	}()
	_ = identity.NewResolver(nil)
}
