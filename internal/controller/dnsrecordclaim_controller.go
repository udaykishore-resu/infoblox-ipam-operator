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

// DNSRecordClaimReconciler reconciles a DNSRecordClaim object against
// Infoblox Universal DDI. Structurally identical to IPSpaceClaimReconciler
// — same finalizer-guarded delete, same periodic drift check — because both
// CRDs are thin fronts onto the same DDI v1 API and the same lifecycle
// concerns apply: a DNS record can be edited or deleted out-of-band in the
// Infoblox portal exactly as an address block can, and leaking a forgotten
// record is a real (if smaller) blast-radius problem the same way a leaked
// IP allocation is.
type DNSRecordClaimReconciler struct {
	client.Client
	InfobloxClient *infoblox.Client
}

func (r *DNSRecordClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var claim ipamv1alpha1.DNSRecordClaim
	if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get DNSRecordClaim: %w", err)
	}

	if !claim.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &claim)
	}

	if !containsString(claim.Finalizers, finalizerName) {
		claim.Finalizers = append(claim.Finalizers, finalizerName)
		if err := r.Update(ctx, &claim); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if claim.Status.InfobloxRef == "" {
		return r.reconcileCreate(ctx, &claim)
	}

	if claim.Status.LastDriftCheckTime == nil ||
		time.Since(claim.Status.LastDriftCheckTime.Time) > driftRecheckEvery {
		return r.reconcileDriftCheck(ctx, &claim)
	}

	logger.V(1).Info("dns claim is bound and within drift-check window, nothing to do",
		"claim", claim.Name, "fqdn", claim.Status.FQDN)
	return ctrl.Result{RequeueAfter: driftRecheckEvery}, nil
}

func (r *DNSRecordClaimReconciler) reconcileCreate(ctx context.Context, claim *ipamv1alpha1.DNSRecordClaim) (ctrl.Result, error) {
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

	rec, err := r.InfobloxClient.CreateDNSRecord(ctx, claim.Spec.Zone, claim.Spec.Name,
		claim.Spec.RecordType, claim.Spec.Value, claim.Spec.TTL, tags, comment)
	if err != nil {
		logger.Error(err, "dns record creation failed", "claim", claim.Name)
		claim.Status.Phase = ipamv1alpha1.DNSPhaseFailed
		setDNSCondition(claim, conditionReady, metav1.ConditionFalse, "CreateFailed", err.Error())
		_ = r.Status().Update(ctx, claim)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	claim.Status.Phase = ipamv1alpha1.DNSPhaseBound
	claim.Status.FQDN = fqdn(claim.Spec.Name, claim.Spec.Zone)
	claim.Status.InfobloxRef = rec.ID
	claim.Status.LastReconciledGeneration = claim.Generation
	now := metav1.Now()
	claim.Status.LastDriftCheckTime = &now
	setDNSCondition(claim, conditionReady, metav1.ConditionTrue, "Created", "DNS record created in Infoblox")

	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status after dns record creation: %w", err)
	}

	logger.Info("created dns record", "claim", claim.Name, "fqdn", claim.Status.FQDN, "ref", rec.ID)
	return ctrl.Result{RequeueAfter: driftRecheckEvery}, nil
}

func (r *DNSRecordClaimReconciler) reconcileDriftCheck(ctx context.Context, claim *ipamv1alpha1.DNSRecordClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	current, err := r.InfobloxClient.GetDNSRecord(ctx, claim.Status.InfobloxRef)
	now := metav1.Now()

	if err != nil {
		if apiErr, ok := err.(*infoblox.APIError); ok && apiErr.StatusCode == 404 {
			logger.Info("infoblox-side dns record missing, marking claim as drifted", "claim", claim.Name)
			claim.Status.Phase = ipamv1alpha1.DNSPhaseDrifted
			setDNSCondition(claim, conditionDrift, metav1.ConditionTrue, "RecordMissing",
				"dns record no longer exists in Infoblox; manual intervention required")
			claim.Status.LastDriftCheckTime = &now
			_ = r.Status().Update(ctx, claim)
			return ctrl.Result{RequeueAfter: driftRecheckEvery}, nil
		}
		logger.Error(err, "dns drift check failed, will retry", "claim", claim.Name)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if current.Value != claim.Spec.Value || current.TTL != claim.Spec.TTL {
		logger.Info("drift detected: infoblox dns record differs from spec", "claim", claim.Name,
			"want", claim.Spec.Value, "got", current.Value)
		claim.Status.Phase = ipamv1alpha1.DNSPhaseDrifted
		setDNSCondition(claim, conditionDrift, metav1.ConditionTrue, "ValueMismatch",
			fmt.Sprintf("infoblox reports value=%s ttl=%d, spec wants value=%s ttl=%d",
				current.Value, current.TTL, claim.Spec.Value, claim.Spec.TTL))
	} else {
		claim.Status.Phase = ipamv1alpha1.DNSPhaseBound
		setDNSCondition(claim, conditionDrift, metav1.ConditionFalse, "InSync", "matches Infoblox state")
	}

	claim.Status.LastDriftCheckTime = &now
	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status after dns drift check: %w", err)
	}
	return ctrl.Result{RequeueAfter: driftRecheckEvery}, nil
}

func (r *DNSRecordClaimReconciler) reconcileDelete(ctx context.Context, claim *ipamv1alpha1.DNSRecordClaim) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if containsString(claim.Finalizers, finalizerName) {
		if claim.Spec.ReclaimPolicy != "Retain" && claim.Status.InfobloxRef != "" {
			if err := r.InfobloxClient.DeleteDNSRecord(ctx, claim.Status.InfobloxRef); err != nil {
				logger.Error(err, "failed to delete infoblox dns record, will retry", "claim", claim.Name)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			logger.Info("deleted infoblox dns record", "claim", claim.Name, "ref", claim.Status.InfobloxRef)
		} else if claim.Spec.ReclaimPolicy == "Retain" {
			logger.Info("reclaimPolicy=Retain, leaving infoblox dns record intact", "claim", claim.Name, "ref", claim.Status.InfobloxRef)
		}

		claim.Finalizers = removeString(claim.Finalizers, finalizerName)
		if err := r.Update(ctx, claim); err != nil {
			return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
		}
	}
	return ctrl.Result{}, nil
}

func fqdn(name, zone string) string {
	if name == "@" || name == "" {
		return zone
	}
	return name + "." + zone
}

// setDNSCondition mirrors setCondition from ipspaceclaim_controller.go —
// duplicated rather than made generic because the two CRD status types
// don't share a common interface here (Go's lack of a struct-field-set
// abstraction makes a generic version more indirection than the ~15 lines
// it would save). If a third CRD needs this pattern, that's the point to
// factor out a shared interface.
func setDNSCondition(claim *ipamv1alpha1.DNSRecordClaim, condType string, status metav1.ConditionStatus, reason, message string) {
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

// SetupWithManager wires the reconciler into the controller-runtime manager.
func (r *DNSRecordClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ipamv1alpha1.DNSRecordClaim{}).
		Complete(r)
}
