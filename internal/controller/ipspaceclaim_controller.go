// Package controller implements the reconciliation loop for IPSpaceClaim
// objects, translating Kubernetes-declared intent into Infoblox Universal
// DDI address block allocations, and detecting drift when the Infoblox-side
// state diverges from what the cluster believes it owns.
package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ipamv1alpha1 "github.com/udaykishore-resu/infoblox-ipam-operator/api/v1alpha1"
	"github.com/udaykishore-resu/infoblox-ipam-operator/internal/infoblox"
)

const (
	finalizerName     = "infoblox.udaykishore.dev/release-allocation"
	driftRecheckEvery = 5 * time.Minute
	conditionReady    = "Ready"
	conditionDrift    = "DriftDetected"
)

// IPSpaceClaimReconciler reconciles an IPSpaceClaim object against Infoblox
// Universal DDI. It is deliberately written as a level-triggered, idempotent
// loop: re-running Reconcile with no spec change and no drift is a no-op.
type IPSpaceClaimReconciler struct {
	client.Client
	InfobloxClient *infoblox.Client
}

// Reconcile implements the controller-runtime reconcile.Reconciler interface.
func (r *IPSpaceClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var claim ipamv1alpha1.IPSpaceClaim
	if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get IPSpaceClaim: %w", err)
	}

	// Deletion path: release the Infoblox-side allocation (unless the user
	// asked us to retain it), then drop our finalizer so K8s can finish
	// deleting the object.
	if !claim.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &claim)
	}

	// Ensure our finalizer is present before we ever allocate anything —
	// otherwise a claim could be deleted from etcd while still holding a
	// live Infoblox allocation, leaking address space silently.
	if !containsString(claim.Finalizers, finalizerName) {
		claim.Finalizers = append(claim.Finalizers, finalizerName)
		if err := r.Update(ctx, &claim); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Not yet bound: allocate.
	if claim.Status.InfobloxRef == "" {
		return r.reconcileAllocate(ctx, &claim)
	}

	// Already bound: only re-check Infoblox for drift periodically, not on
	// every reconcile, to keep API call volume reasonable at fleet scale.
	if claim.Status.LastDriftCheckTime == nil ||
		time.Since(claim.Status.LastDriftCheckTime.Time) > driftRecheckEvery {
		return r.reconcileDriftCheck(ctx, &claim)
	}

	logger.V(1).Info("claim is bound and within drift-check window, nothing to do",
		"claim", claim.Name, "cidr", claim.Status.AllocatedCIDR)
	return ctrl.Result{RequeueAfter: driftRecheckEvery}, nil
}

func (r *IPSpaceClaimReconciler) reconcileAllocate(ctx context.Context, claim *ipamv1alpha1.IPSpaceClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	tags := map[string]string{
		"k8s-namespace": claim.Namespace,
		"k8s-name":      claim.Name,
		"managed-by":    "infoblox-ipam-operator",
	}
	for k, v := range claim.Spec.Tags {
		tags[k] = v
	}
	comment := fmt.Sprintf("managed by infoblox-ipam-operator: %s/%s", claim.Namespace, claim.Name)

	var block *infoblox.AddressBlock
	var err error

	switch {
	case claim.Spec.FixedCIDR != "":
		block, err = r.InfobloxClient.AllocateFixed(ctx, claim.Spec.IPSpaceName, claim.Spec.FixedCIDR, tags, comment)
	case claim.Spec.CIDRSize != nil:
		block, err = r.InfobloxClient.AllocateNextAvailable(ctx, claim.Spec.IPSpaceName, *claim.Spec.CIDRSize, tags, comment)
	default:
		err = fmt.Errorf("spec must set either cidrSize or fixedCIDR")
	}

	if err != nil {
		logger.Error(err, "allocation failed", "claim", claim.Name)
		claim.Status.Phase = ipamv1alpha1.PhaseFailed
		setCondition(claim, conditionReady, metav1.ConditionFalse, "AllocationFailed", err.Error())
		_ = r.Status().Update(ctx, claim)
		// Backoff instead of hot-looping against a misconfigured space or
		// an Infoblox outage.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	claim.Status.Phase = ipamv1alpha1.PhaseBound
	claim.Status.AllocatedCIDR = fmt.Sprintf("%s/%d", block.Address, block.CIDR)
	claim.Status.InfobloxRef = block.ID
	claim.Status.LastReconciledGeneration = claim.Generation
	now := metav1.Now()
	claim.Status.LastDriftCheckTime = &now
	setCondition(claim, conditionReady, metav1.ConditionTrue, "Allocated", "address block allocated from Infoblox")

	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status after allocation: %w", err)
	}

	logger.Info("allocated address block", "claim", claim.Name, "cidr", claim.Status.AllocatedCIDR, "ref", block.ID)
	return ctrl.Result{RequeueAfter: driftRecheckEvery}, nil
}

func (r *IPSpaceClaimReconciler) reconcileDriftCheck(ctx context.Context, claim *ipamv1alpha1.IPSpaceClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	current, err := r.InfobloxClient.Get(ctx, claim.Status.InfobloxRef)
	now := metav1.Now()

	if err != nil {
		if apiErr, ok := err.(*infoblox.APIError); ok && apiErr.StatusCode == 404 {
			// Someone deleted the allocation directly in the Infoblox portal.
			logger.Info("infoblox-side allocation missing, marking claim as drifted", "claim", claim.Name)
			claim.Status.Phase = ipamv1alpha1.PhaseDrifted
			setCondition(claim, conditionDrift, metav1.ConditionTrue, "AllocationMissing",
				"allocation no longer exists in Infoblox; manual intervention required")
			claim.Status.LastDriftCheckTime = &now
			_ = r.Status().Update(ctx, claim)
			return ctrl.Result{RequeueAfter: driftRecheckEvery}, nil
		}
		logger.Error(err, "drift check failed, will retry", "claim", claim.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	wantCIDR := fmt.Sprintf("%s/%d", current.Address, current.CIDR)
	if wantCIDR != claim.Status.AllocatedCIDR {
		logger.Info("drift detected: Infoblox CIDR differs from status", "claim", claim.Name,
			"want", claim.Status.AllocatedCIDR, "got", wantCIDR)
		claim.Status.Phase = ipamv1alpha1.PhaseDrifted
		setCondition(claim, conditionDrift, metav1.ConditionTrue, "CIDRMismatch",
			fmt.Sprintf("infoblox reports %s, status has %s", wantCIDR, claim.Status.AllocatedCIDR))
	} else {
		claim.Status.Phase = ipamv1alpha1.PhaseBound
		setCondition(claim, conditionDrift, metav1.ConditionFalse, "InSync", "matches Infoblox state")
	}

	claim.Status.LastDriftCheckTime = &now
	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status after drift check: %w", err)
	}
	return ctrl.Result{RequeueAfter: driftRecheckEvery}, nil
}

func (r *IPSpaceClaimReconciler) reconcileDelete(ctx context.Context, claim *ipamv1alpha1.IPSpaceClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if containsString(claim.Finalizers, finalizerName) {
		if claim.Spec.ReclaimPolicy != "Retain" && claim.Status.InfobloxRef != "" {
			if err := r.InfobloxClient.Release(ctx, claim.Status.InfobloxRef); err != nil {
				logger.Error(err, "failed to release infoblox allocation, will retry", "claim", claim.Name)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			logger.Info("released infoblox allocation", "claim", claim.Name, "ref", claim.Status.InfobloxRef)
		} else if claim.Spec.ReclaimPolicy == "Retain" {
			logger.Info("reclaimPolicy=Retain, leaving infoblox allocation intact", "claim", claim.Name, "ref", claim.Status.InfobloxRef)
		}

		claim.Finalizers = removeString(claim.Finalizers, finalizerName)
		if err := r.Update(ctx, claim); err != nil {
			return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
		}
	}
	return ctrl.Result{}, nil
}

func setCondition(claim *ipamv1alpha1.IPSpaceClaim, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, c := range claim.Status.Conditions {
		if c.Type == condType {
			claim.Status.Conditions[i] = metav1.Condition{
				Type: condType, Status: status, Reason: reason, Message: message,
				LastTransitionTime: now, ObservedGeneration: claim.Generation,
			}
			return
		}
	}
	claim.Status.Conditions = append(claim.Status.Conditions, metav1.Condition{
		Type: condType, Status: status, Reason: reason, Message: message,
		LastTransitionTime: now, ObservedGeneration: claim.Generation,
	})
}

func containsString(s []string, target string) bool {
	for _, v := range s {
		if v == target {
			return true
		}
	}
	return false
}

func removeString(s []string, target string) []string {
	out := make([]string, 0, len(s))
	for _, v := range s {
		if v != target {
			out = append(out, v)
		}
	}
	return out
}

// SetupWithManager wires the reconciler into the controller-runtime manager.
func (r *IPSpaceClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ipamv1alpha1.IPSpaceClaim{}).
		Complete(r)
}
