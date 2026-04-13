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

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	incidentaryv1alpha1 "github.com/incidentary/operator/api/v1alpha1"
	"github.com/incidentary/operator/internal/informers"
)

// Reconcile status values.
const (
	PhaseRunning  = "Running"
	PhaseDegraded = "Degraded"
)

// Ready condition reasons.
const (
	ConditionTypeReady = "Ready"

	ReasonReconciled     = "Reconciled"
	ReasonSecretNotFound = "SecretNotFound"
	ReasonInvalidAPIKey  = "InvalidAPIKey"
	ReasonReadError      = "ReadError"
)

// DefaultReconciliationIntervalSeconds is used when Spec.ReconciliationIntervalSeconds is zero.
const DefaultReconciliationIntervalSeconds = 300

// RequeueAfterError is how long to wait before retrying transient failures
// like "Secret not found" (may just not have been created yet).
const RequeueAfterError = 30 * time.Second

// DiscoveryObserver exposes the minimal surface the controller needs from
// the discovery loop: the current watched-workload count and the timestamp
// of the most recent successful topology report.
type DiscoveryObserver interface {
	WatchedWorkloads() int32
	LastReport() time.Time
}

// ReconcilerObserver exposes classification counts from the discovery
// reconciliation loop (matched, ghost, mismatched, new).
type ReconcilerObserver interface {
	Counts() (matched, ghost, mismatched, newCount int32)
}

// IncidentaryConfigReconciler reconciles a IncidentaryConfig object
type IncidentaryConfigReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Discovery  DiscoveryObserver  // optional; zero values reported when nil
	Classifier ReconcilerObserver // optional; zero values reported when nil
}

// --- CRD ---
// +kubebuilder:rbac:groups=incidentary.incidentary.com,resources=incidentaryconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=incidentary.incidentary.com,resources=incidentaryconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=incidentary.incidentary.com,resources=incidentaryconfigs/finalizers,verbs=update
//
// --- Secret (for API-key lookup) ---
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//
// --- Core watch set (read-only) ---
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
//
// --- Apps watch set (read-only) ---
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
//
// --- Autoscaling / Batch / Networking / Events (read-only) ---
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=get;list;watch
//
// --- Leader-election lease (required because Manager.LeaderElection is wired) ---
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;create;update

// Reconcile moves the current state of the cluster closer to the desired
// IncidentaryConfig: it reads the referenced API-key Secret, validates it,
// updates Status.Phase + Conditions, and requeues at the configured interval.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/reconcile
func (r *IncidentaryConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("incidentaryconfig", req.NamespacedName)

	// 1. Fetch the IncidentaryConfig. If it has been deleted, return without
	//    error (finalizers are not used in Phase 2).
	config := &incidentaryv1alpha1.IncidentaryConfig{}
	if err := r.Get(ctx, req.NamespacedName, config); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("IncidentaryConfig not found; likely deleted")
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to fetch IncidentaryConfig")
		return ctrl.Result{}, err
	}

	// 2. Read the referenced Secret.
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Name:      config.Spec.APIKeySecretRef.Name,
		Namespace: config.Namespace,
	}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("API-key Secret not found",
				"secret", secretKey,
				"key", config.Spec.APIKeySecretRef.Key,
			)
			return r.markDegraded(
				ctx,
				config,
				ReasonSecretNotFound,
				fmt.Sprintf("Secret %q not found in namespace %q", secretKey.Name, secretKey.Namespace),
				RequeueAfterError,
			)
		}
		log.Error(err, "failed to read API-key Secret",
			"secret", secretKey,
			"key", config.Spec.APIKeySecretRef.Key,
		)
		return r.markDegraded(
			ctx,
			config,
			ReasonReadError,
			fmt.Sprintf("error reading Secret %q; check operator logs for details", secretKey.Name),
			RequeueAfterError,
		)
	}

	// 3. Validate the API-key value is present and non-empty.
	rawValue, ok := secret.Data[config.Spec.APIKeySecretRef.Key]
	if !ok || len(rawValue) == 0 {
		log.Info("API-key Secret has empty or missing key",
			"secret", secretKey,
			"key", config.Spec.APIKeySecretRef.Key,
		)
		return r.markDegraded(
			ctx,
			config,
			ReasonInvalidAPIKey,
			fmt.Sprintf("Secret %q key %q is empty", secretKey.Name, config.Spec.APIKeySecretRef.Key),
			RequeueAfterError,
		)
	}

	// 4. Happy path: the operator has everything it needs to run. Mark Ready.
	now := metav1.Now()
	config.Status.Phase = PhaseRunning
	config.Status.LastReconciliation = &now
	if r.Discovery != nil {
		config.Status.WatchedWorkloads = r.Discovery.WatchedWorkloads()
	}
	if r.Classifier != nil {
		matched, _, mismatched, _ := r.Classifier.Counts()
		config.Status.MatchedServices = matched
		config.Status.UnmatchedWorkloads = mismatched
	}

	meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: config.Generation,
		LastTransitionTime: now,
		Reason:             ReasonReconciled,
		Message:            fmt.Sprintf("informers watching %d resource types", len(informers.WatchSet)),
	})

	if err := r.Status().Update(ctx, config); err != nil {
		log.Error(err, "failed to update IncidentaryConfig status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: reconciliationInterval(config)}, nil
}

// markDegraded sets Phase=Degraded and the Ready condition to False with the
// given reason/message, persists the status, and returns a requeue result.
// LastReconciliation is intentionally NOT updated on the degraded path — that
// field records the timestamp of the most recent successful reconciliation.
func (r *IncidentaryConfigReconciler) markDegraded(
	ctx context.Context,
	config *incidentaryv1alpha1.IncidentaryConfig,
	reason string,
	message string,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	now := metav1.Now()
	config.Status.Phase = PhaseDegraded

	meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: config.Generation,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	})

	if err := r.Status().Update(ctx, config); err != nil {
		log.Error(err, "failed to update IncidentaryConfig status (degraded)", "reason", reason)
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// reconciliationInterval returns the duration to wait before the next
// reconciliation. It falls back to DefaultReconciliationIntervalSeconds when
// the spec field is unset (zero).
func reconciliationInterval(config *incidentaryv1alpha1.IncidentaryConfig) time.Duration {
	seconds := config.Spec.ReconciliationIntervalSeconds
	if seconds <= 0 {
		seconds = DefaultReconciliationIntervalSeconds
	}
	return time.Duration(seconds) * time.Second
}

// SetupWithManager sets up the controller with the Manager.
func (r *IncidentaryConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&incidentaryv1alpha1.IncidentaryConfig{}).
		Named("incidentaryconfig").
		Complete(r)
}
