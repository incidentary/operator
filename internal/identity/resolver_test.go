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
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/incidentary/operator/internal/identity"
)

const (
	kindDeployment  = "Deployment"
	kindStatefulSet = "StatefulSet"
	kindDaemonSet   = "DaemonSet"
	kindPod         = "Pod"
)

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
					Kind:       kindStatefulSet,
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
	if got.OwnerKind != kindStatefulSet {
		t.Fatalf("OwnerKind = %q, want %q", got.OwnerKind, kindStatefulSet)
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
					Kind:       kindDaemonSet,
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
	if got.OwnerKind != kindDaemonSet {
		t.Fatalf("OwnerKind = %q, want %q", got.OwnerKind, kindDaemonSet)
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
	if got.OwnerKind != kindPod {
		t.Fatalf("OwnerKind = %q, want %q", got.OwnerKind, kindPod)
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
	if got.OwnerKind != kindPod {
		t.Fatalf("OwnerKind = %q, want %q", got.OwnerKind, kindPod)
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

	got, err := r.ResolveRef(context.Background(), kindPod, "prod", "checkout-abcde-xyz")
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

// errorReader is a client.Reader that always returns the provided error.
// Used to exercise non-NotFound error paths in the resolver.
type errorReader struct{ err error }

func (e *errorReader) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return e.err
}
func (e *errorReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return e.err
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

// -----------------------------------------------------------------------------
// Resolve: direct workload types (StatefulSet, DaemonSet, unknown/default)
// -----------------------------------------------------------------------------

func TestResolve_StatefulSetDirect(t *testing.T) {
	t.Parallel()

	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kafka",
			Namespace: "data",
			Annotations: map[string]string{
				identity.ServiceIDAnnotation: "kafka-service",
			},
		},
	}
	r := identity.NewResolver(newFakeClient(t, ss))

	got, err := r.Resolve(context.Background(), ss)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ServiceID != "kafka-service" {
		t.Fatalf("ServiceID = %q, want %q", got.ServiceID, "kafka-service")
	}
	if got.OwnerKind != kindStatefulSet {
		t.Fatalf("OwnerKind = %q, want StatefulSet", got.OwnerKind)
	}
	if got.Source != identity.SourceAnnotation {
		t.Fatalf("Source = %q, want annotation", got.Source)
	}
}

func TestResolve_DaemonSetDirect(t *testing.T) {
	t.Parallel()

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node-exporter",
			Namespace: "monitoring",
		},
	}
	r := identity.NewResolver(newFakeClient(t, ds))

	got, err := r.Resolve(context.Background(), ds)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ServiceID != "node-exporter" {
		t.Fatalf("ServiceID = %q, want %q", got.ServiceID, "node-exporter")
	}
	if got.OwnerKind != kindDaemonSet {
		t.Fatalf("OwnerKind = %q, want DaemonSet", got.OwnerKind)
	}
	if got.Source != identity.SourceWorkloadName {
		t.Fatalf("Source = %q, want workload_name", got.Source)
	}
}

func TestResolve_DefaultTypeReturnsEmpty(t *testing.T) {
	t.Parallel()

	// A ConfigMap is a client.Object but not a recognized workload — it falls
	// through to the default case and returns an empty Result.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-config", Namespace: "prod"},
	}
	r := identity.NewResolver(newFakeClient(t, cm))

	got, err := r.Resolve(context.Background(), cm)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Empty() {
		t.Fatalf("expected empty Result for ConfigMap, got %+v", got)
	}
}

// -----------------------------------------------------------------------------
// Resolve: Pod with unknown controller kind (Job, CronJob, etc.)
// -----------------------------------------------------------------------------

func TestResolve_PodOwnedByJobFallsBackToPod(t *testing.T) {
	t.Parallel()

	// A Pod whose controller is a "Job" (not ReplicaSet/StatefulSet/DaemonSet)
	// falls to the default branch in resolveFromPod and uses the pod as identity.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "batch-worker-xyz",
			Namespace: "jobs",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "batch/v1",
					Kind:       "Job",
					Name:       "daily-report",
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
	if got.ServiceID != "batch-worker-xyz" {
		t.Fatalf("ServiceID = %q, want pod name %q", got.ServiceID, "batch-worker-xyz")
	}
	if got.OwnerKind != kindPod {
		t.Fatalf("OwnerKind = %q, want Pod (unknown controller fallback)", got.OwnerKind)
	}
}

// -----------------------------------------------------------------------------
// ResolveRef: additional kinds to improve coverage.
// -----------------------------------------------------------------------------

func TestResolveRef_EmptyNameReturnsEmpty(t *testing.T) {
	t.Parallel()

	r := identity.NewResolver(newFakeClient(t))
	got, err := r.ResolveRef(context.Background(), kindPod, "prod", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Empty() {
		t.Fatalf("expected empty Result for empty name, got %+v", got)
	}
}

func TestResolveRef_Deployment(t *testing.T) {
	t.Parallel()

	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-service",
			Namespace: "prod",
			Annotations: map[string]string{
				identity.ServiceIDAnnotation: "api",
			},
		},
	}
	r := identity.NewResolver(newFakeClient(t, d))

	got, err := r.ResolveRef(context.Background(), "Deployment", "prod", "api-service")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if got.ServiceID != "api" {
		t.Fatalf("ServiceID = %q, want %q", got.ServiceID, "api")
	}
	if got.OwnerKind != kindDeployment {
		t.Fatalf("OwnerKind = %q, want %q", got.OwnerKind, kindDeployment)
	}
}

func TestResolveRef_StatefulSet(t *testing.T) {
	t.Parallel()

	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "mysql", Namespace: "data"},
	}
	r := identity.NewResolver(newFakeClient(t, ss))

	got, err := r.ResolveRef(context.Background(), kindStatefulSet, "data", "mysql")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if got.ServiceID != "mysql" {
		t.Fatalf("ServiceID = %q, want mysql", got.ServiceID)
	}
	if got.OwnerKind != kindStatefulSet {
		t.Fatalf("OwnerKind = %q, want StatefulSet", got.OwnerKind)
	}
}

func TestResolveRef_DaemonSet(t *testing.T) {
	t.Parallel()

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "promtail", Namespace: "monitoring"},
	}
	r := identity.NewResolver(newFakeClient(t, ds))

	got, err := r.ResolveRef(context.Background(), kindDaemonSet, "monitoring", "promtail")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if got.ServiceID != "promtail" {
		t.Fatalf("ServiceID = %q, want promtail", got.ServiceID)
	}
	if got.OwnerKind != kindDaemonSet {
		t.Fatalf("OwnerKind = %q, want DaemonSet", got.OwnerKind)
	}
}

func TestResolveRef_ReplicaSetWalksToDeploy(t *testing.T) {
	t.Parallel()

	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: "prod"},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backend-abc12",
			Namespace: "prod",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Deployment", Name: "backend", Controller: ptrBool(true)},
			},
		},
	}
	r := identity.NewResolver(newFakeClient(t, d, rs))

	got, err := r.ResolveRef(context.Background(), "ReplicaSet", "prod", "backend-abc12")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if got.ServiceID != "backend" {
		t.Fatalf("ServiceID = %q, want backend", got.ServiceID)
	}
	if got.OwnerKind != kindDeployment {
		t.Fatalf("OwnerKind = %q, want Deployment", got.OwnerKind)
	}
}

func TestResolveRef_UnknownKindReturnsEmpty(t *testing.T) {
	t.Parallel()

	r := identity.NewResolver(newFakeClient(t))
	got, err := r.ResolveRef(context.Background(), "Frobnicator", "prod", "thing-1")
	if err != nil {
		t.Fatalf("unexpected error for unknown kind: %v", err)
	}
	if !got.Empty() {
		t.Fatalf("expected empty Result for unknown kind, got %+v", got)
	}
}

// -----------------------------------------------------------------------------
// Resolve: orphaned StatefulSet / DaemonSet pod paths.
// -----------------------------------------------------------------------------

func TestResolve_PodOrphanedStatefulSet(t *testing.T) {
	t.Parallel()

	// Pod references a StatefulSet that no longer exists — fallback to
	// StatefulSet name-based identity (with nil annotations).
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gone-ss-0",
			Namespace: "data",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: kindStatefulSet, Name: "gone-ss", Controller: ptrBool(true)},
			},
		},
	}
	r := identity.NewResolver(newFakeClient(t, pod))

	got, err := r.Resolve(context.Background(), pod)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Fallback uses owner.Name when the StatefulSet object can't be fetched.
	if got.ServiceID != "gone-ss" {
		t.Fatalf("ServiceID = %q, want %q", got.ServiceID, "gone-ss")
	}
	if got.OwnerKind != kindStatefulSet {
		t.Fatalf("OwnerKind = %q, want StatefulSet", got.OwnerKind)
	}
}

func TestResolve_PodOrphanedDaemonSet(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gone-ds-node1",
			Namespace: "kube-system",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: kindDaemonSet, Name: "gone-ds", Controller: ptrBool(true)},
			},
		},
	}
	r := identity.NewResolver(newFakeClient(t, pod))

	got, err := r.Resolve(context.Background(), pod)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ServiceID != "gone-ds" {
		t.Fatalf("ServiceID = %q, want %q", got.ServiceID, "gone-ds")
	}
	if got.OwnerKind != kindDaemonSet {
		t.Fatalf("OwnerKind = %q, want DaemonSet", got.OwnerKind)
	}
}

// -----------------------------------------------------------------------------
// controllerOwner: fallback when no owner has Controller=true.
// -----------------------------------------------------------------------------

func TestResolve_PodWithNonControllerOwnerRefUsesFirst(t *testing.T) {
	t.Parallel()

	// Owner reference with Controller=nil — the fallback path in controllerOwner
	// returns &refs[0], which has Kind "Job". resolveFromPod defaults to pod name.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "job-worker-abc",
			Namespace: "batch",
			OwnerReferences: []metav1.OwnerReference{
				{
					Kind: "Job",
					Name: "nightly-run",
					// Controller is nil — not set
				},
			},
		},
	}
	r := identity.NewResolver(newFakeClient(t, pod))

	got, err := r.Resolve(context.Background(), pod)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// controllerOwner returns &refs[0] (Kind="Job") → falls to default in
	// resolveFromPod → uses pod name.
	if got.ServiceID != "job-worker-abc" {
		t.Fatalf("ServiceID = %q, want %q", got.ServiceID, "job-worker-abc")
	}
}

// -----------------------------------------------------------------------------
// Non-NotFound error paths (via errorReader).
// -----------------------------------------------------------------------------

func TestResolveRef_DeploymentFetchError(t *testing.T) {
	t.Parallel()

	boom := errors.New("server unavailable")
	r := identity.NewResolver(&errorReader{err: boom})

	_, err := r.ResolveRef(context.Background(), "Deployment", "prod", "api")
	if err == nil {
		t.Fatal("expected error from ResolveRef on non-NotFound fetch failure")
	}
}

func TestResolveRef_StatefulSetFetchError(t *testing.T) {
	t.Parallel()

	r := identity.NewResolver(&errorReader{err: errors.New("etcd timeout")})
	_, err := r.ResolveRef(context.Background(), kindStatefulSet, "data", "pg")
	if err == nil {
		t.Fatal("expected error from ResolveRef for StatefulSet fetch failure")
	}
}

func TestResolveRef_DaemonSetFetchError(t *testing.T) {
	t.Parallel()

	r := identity.NewResolver(&errorReader{err: errors.New("connection refused")})
	_, err := r.ResolveRef(context.Background(), kindDaemonSet, "kube-system", "ds")
	if err == nil {
		t.Fatal("expected error from ResolveRef for DaemonSet fetch failure")
	}
}

func TestResolveRef_ReplicaSetFetchError(t *testing.T) {
	t.Parallel()

	r := identity.NewResolver(&errorReader{err: errors.New("timeout")})
	_, err := r.ResolveRef(context.Background(), "ReplicaSet", "prod", "rs")
	if err == nil {
		t.Fatal("expected error from ResolveRef for ReplicaSet fetch failure")
	}
}

func TestResolveRef_PodFetchError(t *testing.T) {
	t.Parallel()

	r := identity.NewResolver(&errorReader{err: errors.New("throttled")})
	_, err := r.ResolveRef(context.Background(), kindPod, "prod", "my-pod")
	if err == nil {
		t.Fatal("expected error from ResolveRef for Pod fetch failure")
	}
}

func TestResolve_ReplicaSetDeploymentFetchError(t *testing.T) {
	t.Parallel()

	// Resolve with a *appsv1.ReplicaSet that has a Deployment owner. The error
	// reader makes the Deployment Get fail with a non-NotFound error, covering
	// the resolveFromReplicaSet error path.
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-rs",
			Namespace: "prod",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Deployment", Name: "api", Controller: ptrBool(true)},
			},
		},
	}
	r := identity.NewResolver(&errorReader{err: errors.New("timeout")})
	_, err := r.Resolve(context.Background(), rs)
	if err == nil {
		t.Fatal("expected error when Deployment fetch fails with non-NotFound error")
	}
}

func TestResolve_PodReplicaSetFetchError(t *testing.T) {
	t.Parallel()

	// Pod owned by a ReplicaSet; errorReader causes the RS Get to fail with a
	// non-NotFound error, covering the resolveFromPod RS error path.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-pod",
			Namespace: "prod",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "api-rs", Controller: ptrBool(true)},
			},
		},
	}
	r := identity.NewResolver(&errorReader{err: errors.New("timeout")})
	_, err := r.Resolve(context.Background(), pod)
	if err == nil {
		t.Fatal("expected error when ReplicaSet fetch fails with non-NotFound error")
	}
}

func TestResolve_PodStatefulSetFetchError(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db-0",
			Namespace: "data",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: kindStatefulSet, Name: "db", Controller: ptrBool(true)},
			},
		},
	}
	r := identity.NewResolver(&errorReader{err: errors.New("etcd unavailable")})
	_, err := r.Resolve(context.Background(), pod)
	if err == nil {
		t.Fatal("expected error when StatefulSet fetch fails with non-NotFound error")
	}
}

func TestResolve_PodDaemonSetFetchError(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ds-node1",
			Namespace: "kube-system",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: kindDaemonSet, Name: "fluentd", Controller: ptrBool(true)},
			},
		},
	}
	r := identity.NewResolver(&errorReader{err: errors.New("connection refused")})
	_, err := r.Resolve(context.Background(), pod)
	if err == nil {
		t.Fatal("expected error when DaemonSet fetch fails with non-NotFound error")
	}
}

func TestResolve_ReplicaSetNoOwner(t *testing.T) {
	t.Parallel()

	// A ReplicaSet with no owner references: controllerOwner returns nil so
	// resolveFromReplicaSet takes the orphaned-RS path (first return).
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "standalone-rs", Namespace: "prod"},
	}
	r := identity.NewResolver(newFakeClient(t, rs))
	got, err := r.Resolve(context.Background(), rs)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ServiceID != "standalone-rs" {
		t.Fatalf("ServiceID = %q, want %q", got.ServiceID, "standalone-rs")
	}
	if got.OwnerKind != "ReplicaSet" {
		t.Fatalf("OwnerKind = %q, want ReplicaSet", got.OwnerKind)
	}
}
