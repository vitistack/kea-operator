/*
Copyright 2025.

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

package v1alpha1

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/vitistack/common/pkg/loggers/vlog"
	viticommonconditions "github.com/vitistack/common/pkg/operator/conditions"
	viticommonfinalizers "github.com/vitistack/common/pkg/operator/finalizers"
	reconcileutil "github.com/vitistack/common/pkg/operator/reconcileutil"
	vitistackcrdsv1alpha1 "github.com/vitistack/crds/pkg/v1alpha1"
	keaservice "github.com/vitistack/kea-operator/internal/services/kea"
	"github.com/vitistack/kea-operator/internal/util/unstructuredconv"
	"github.com/vitistack/kea-operator/pkg/interfaces/keainterface"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// NetworkConfigurationReconciler reconciles vitistack.io/v1alpha1 NetworkConfiguration
// resources. It fetches objects as Unstructured for loose coupling, converts to
// typed CRs for strict, type-safe access, and ensures DHCP reservations in Kea
// based on existing leases and a NetworkNamespace IPv4 prefix policy.
type NetworkConfigurationReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	KeaClient keainterface.KeaClient
	Kea       *keaservice.Service
}

const (
	finalizerName              = "networkconfiguration.finalizers.vitistack.io"
	conditionTypeReady         = "Ready"
	conditionReasonReconciling = "Reconciling"
	conditionReasonConfigured  = "Configured"
	conditionReasonError       = "Error"
)

// +kubebuilder:rbac:groups=vitistack.io,resources=networkconfigurations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vitistack.io,resources=networkconfigurations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vitistack.io,resources=networkconfigurations/finalizers,verbs=update
// +kubebuilder:rbac:groups=vitistack.io,resources=networknamespaces,verbs=get;list;watch

// Reconcile fetches the NetworkConfiguration as Unstructured, converts it to the
// typed CR, reads MAC addresses from spec.networkInterfaces[].macAddress, looks
// up the NetworkNamespace IPv4 prefix, resolves the Kea subnet-id, and for each
// MAC requires an existing Kea lease and creates an explicit reservation for that
// IP within the subnet. Status conditions are patched back onto the Unstructured
// object to avoid tight type coupling of status.
func (r *NetworkConfigurationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1) Fetch the NetworkConfiguration
	ncU := &unstructured.Unstructured{}
	ncU.SetGroupVersionKind(schema.GroupVersionKind{Group: "vitistack.io", Version: "v1alpha1", Kind: "NetworkConfiguration"})
	if err := r.Get(ctx, req.NamespacedName, ncU); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Convert to strictly typed NetworkConfiguration for spec access
	nc, err := unstructuredconv.ToNetworkConfiguration(ncU)
	if err != nil {
		log.Error(err, "failed to convert NetworkConfiguration to typed object")
		// Nothing actionable without a valid spec; requeue softly
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Ensure finalizer on non-deleted objects
	if ncU.GetDeletionTimestamp().IsZero() {
		if !viticommonfinalizers.Has(ncU, finalizerName) {
			if err := viticommonfinalizers.Ensure(ctx, r.Client, ncU, finalizerName); err != nil {
				return reconcileutil.Requeue(err)
			}
			return ctrl.Result{}, nil
		}
	} else {
		// Handle deletion: best-effort cleanup then remove finalizer
		if err := r.cleanupReservations(ctx, ncU); err != nil {
			log.Error(err, "cleanup during deletion failed")
		}
		if err := viticommonfinalizers.Remove(ctx, r.Client, ncU, finalizerName); err != nil {
			return reconcileutil.Requeue(err)
		}
		return ctrl.Result{}, nil
	}

	// Mark Reconciling only once per generation (if we haven't yet observed this generation at all).
	if ready := getReadyCondition(ncU); ready == nil || ready.ObservedGeneration != ncU.GetGeneration() {
		_ = r.setCondition(ctx, ncU, viticommonconditions.New(
			conditionTypeReady, metav1.ConditionFalse, conditionReasonReconciling, "reconciling", ncU.GetGeneration(),
		))
	}

	// 2) Fetch the NetworkNamespace in the same namespace to get ipv4_prefix
	ipv4Prefix, err := r.getIPv4PrefixFromNetworkNamespace(ctx, req.Namespace)
	if err != nil {
		log.Error(err, "failed to get NetworkNamespace ipv4_prefix", "namespace", req.Namespace)
		// Requeue to retry later
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// 3) Collect MAC addresses from the NetworkConfiguration resource itself
	macs := extractMACsFromTypedNetworkConfiguration(nc)
	if len(macs) == 0 {
		log.Info("no MAC addresses found on NetworkConfiguration; skipping reservation", "name", nc.GetName(), "namespace", nc.GetNamespace())
		// No error; just exit without requeue
		return ctrl.Result{}, nil
	}

	// 4) Find subnet-id for this prefix in Kea
	subnetID, err := r.Kea.GetSubnetID(ctx, ipv4Prefix)
	if err != nil {
		log.Error(err, "failed to resolve Kea subnet id", "ipv4Prefix", ipv4Prefix)
		txt := strings.ToLower(err.Error())
		_ = r.setCondition(ctx, ncU, viticommonconditions.New(
			conditionTypeReady, metav1.ConditionFalse, conditionReasonError, fmt.Sprintf("resolve subnet: %v", err), ncU.GetGeneration(),
		))
		// Do not hot-loop if command unsupported; just return without requeue (will reconcile on next event or resync)
		if strings.Contains(txt, "unsupported kea command") || strings.Contains(txt, "not supported") {
			return ctrl.Result{}, nil
		}
		// Otherwise requeue (transient error)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// 5) For each MAC, require an existing lease in Kea; use its IP to create a reservation.
	var errs []string
	var ipnet *net.IPNet
	if _, n, e := net.ParseCIDR(strings.TrimSpace(ipv4Prefix)); e == nil {
		ipnet = n
	}
	for _, mac := range macs {
		ip, leaseSubnetID, lerr := r.Kea.GetLeaseIPv4ForMAC(ctx, mac)
		if lerr != nil || ip == "" {
			if lerr != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", mac, lerr))
			} else {
				errs = append(errs, fmt.Sprintf("%s: no lease found", mac))
			}
			continue
		}
		// Optional safety: ensure lease IP is within the namespace prefix
		if ipnet != nil {
			if p := net.ParseIP(ip); p == nil || p.To4() == nil || !ipnet.Contains(p) {
				errs = append(errs, fmt.Sprintf("%s: lease IP %s not within %s", mac, ip, ipv4Prefix))
				continue
			}
		}
		// Prefer Kea's reported subnet-id if present, else use resolved subnetID
		sid := subnetID
		if leaseSubnetID > 0 {
			sid = leaseSubnetID
		}
		if err := r.Kea.EnsureReservationForMACIP(ctx, mac, sid, ip); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", mac, err))
			continue
		}
	}
	if len(errs) > 0 {
		_ = r.setCondition(ctx, ncU, viticommonconditions.New(
			conditionTypeReady, metav1.ConditionFalse, conditionReasonError, fmt.Sprintf("lease/reservation errors: %s", strings.Join(errs, "; ")), ncU.GetGeneration(),
		))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Success: set Ready True
	_ = r.setCondition(ctx, ncU, viticommonconditions.New(
		conditionTypeReady, metav1.ConditionTrue, conditionReasonConfigured, "configured", ncU.GetGeneration(),
	))
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// NewNetworkConfigurationReconciler constructs a new reconciler, wiring the
// controller-runtime client/scheme and a Kea service wrapper around the given client.
func NewNetworkConfigurationReconciler(mgr ctrl.Manager, keaClient keainterface.KeaClient) *NetworkConfigurationReconciler {
	return &NetworkConfigurationReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		KeaClient: keaClient,
		Kea:       keaservice.New(keaClient),
	}
}

// SetupWithManager registers the controller with the manager, watching
// NetworkConfiguration resources as Unstructured instances.
func (r *NetworkConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Watch NetworkConfiguration as unstructured to avoid scheme coupling
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "vitistack.io", Version: "v1alpha1", Kind: "NetworkConfiguration"})
	return ctrl.NewControllerManagedBy(mgr).
		For(u).
		Named("networkconfiguration").
		Complete(r)
}

// getIPv4PrefixFromNetworkNamespace returns the typed NetworkNamespace.Status.IPv4Prefix
// for the provided Kubernetes namespace. It lists NetworkNamespace objects as
// Unstructured and converts the first item to the typed CR to access the field.
func (r *NetworkConfigurationReconciler) getIPv4PrefixFromNetworkNamespace(ctx context.Context, namespace string) (string, error) {
	// Use unstructured to avoid tight coupling if the type isn't available
	nnList := &unstructured.UnstructuredList{}
	// Set the list GVK for vitistack.io/v1alpha1 NetworkNamespace
	nnList.SetAPIVersion("vitistack.io/v1alpha1")
	nnList.SetKind("NetworkNamespace")
	if err := r.List(ctx, nnList, client.InNamespace(namespace)); err != nil {
		return "", err
	}
	if len(nnList.Items) == 0 {
		return "", fmt.Errorf("no NetworkNamespace found in namespace %s", namespace)
	}
	// Assume single NetworkNamespace per Kubernetes namespace
	nnU := nnList.Items[0]
	// Convert to typed for strict access
	nn, err := unstructuredconv.ToNetworkNamespace(&nnU)
	if err != nil {
		return "", fmt.Errorf("failed to convert NetworkNamespace: %w", err)
	}
	if nn.Status.IPv4Prefix != "" {
		return nn.Status.IPv4Prefix, nil
	}
	return "", fmt.Errorf("NetworkNamespace missing status.IPv4Prefix in namespace %s", namespace)
}

// extractMACsFromTypedNetworkConfiguration reads MAC addresses strictly from
// spec.networkInterfaces[].macAddress on the typed NetworkConfiguration. It
// normalizes to lowercase, trims whitespace, replaces '-' with ':', validates
// using net.ParseMAC, and returns a de-duplicated list.
func extractMACsFromTypedNetworkConfiguration(networkconf *vitistackcrdsv1alpha1.NetworkConfiguration) []string {
	if len(networkconf.Spec.NetworkInterfaces) == 0 {
		vlog.Debug("no network interfaces found")
		return nil
	}

	// Normalize, validate, and deduplicate
	uniq := make(map[string]struct{})
	for _, ni := range networkconf.Spec.NetworkInterfaces {
		if ni.MacAddress == "" {
			continue
		}
		s := strings.ToLower(strings.TrimSpace(ni.MacAddress))
		if s == "" {
			continue
		}
		// Accept addresses using '-' by normalizing to ':'
		s = strings.ReplaceAll(s, "-", ":")
		if _, err := net.ParseMAC(s); err != nil {
			continue
		}
		uniq[s] = struct{}{}
	}
	if len(uniq) == 0 {
		return nil
	}
	out := make([]string, 0, len(uniq))
	for m := range uniq {
		out = append(out, m)
	}
	return out
}

// cleanupReservations best-effort removal of reservations on delete. It converts
// the Unstructured NetworkConfiguration to typed form to extract MACs, resolves
// the subnet-id for the namespace prefix, and issues reservation deletions in Kea.
func (r *NetworkConfigurationReconciler) cleanupReservations(ctx context.Context, ncU *unstructured.Unstructured) error {
	ipv4Prefix, err := r.getIPv4PrefixFromNetworkNamespace(ctx, ncU.GetNamespace())
	if err != nil {
		return err
	}
	subnetID, err := r.Kea.GetSubnetID(ctx, ipv4Prefix)
	if err != nil {
		return err
	}
	// Convert to typed NC to extract MACs strictly
	networkconf, convErr := unstructuredconv.ToNetworkConfiguration(ncU)
	if convErr != nil {
		vlog.Debug("failed to convert to typed NetworkConfiguration during cleanup", "error", convErr)
		return nil
	}
	macs := extractMACsFromTypedNetworkConfiguration(networkconf)
	for _, mac := range macs {
		_ = r.Kea.DeleteReservationForMAC(ctx, mac, subnetID)
	}
	return nil
}

// setCondition patches the status.conditions on the provided Unstructured object
// using the common conditions helper, and avoids no-op patches when the condition
// did not meaningfully change.
func (r *NetworkConfigurationReconciler) setCondition(ctx context.Context, nc *unstructured.Unstructured, cond metav1.Condition) error {
	base := nc.DeepCopy()

	// Read existing status.conditions into typed []metav1.Condition
	var existing []metav1.Condition
	if slice, found, _ := unstructured.NestedSlice(nc.Object, "status", "conditions"); found {
		for _, it := range slice {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			var c metav1.Condition
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &c); err == nil {
				existing = append(existing, c)
			}
		}
	}

	// Capture pre-update snapshot to detect changes for the specific condition
	var prev *metav1.Condition
	for i := range existing {
		if existing[i].Type == cond.Type {
			tmp := existing[i]
			prev = &tmp
			break
		}
	}

	// Update or add the condition using common helper
	viticommonconditions.SetOrUpdateCondition(&existing, &cond)

	// Locate updated condition to compare meaningful fields; if unchanged skip patch
	var cur *metav1.Condition
	for i := range existing {
		if existing[i].Type == cond.Type {
			cur = &existing[i]
			break
		}
	}
	if prev != nil && cur != nil {
		// If status, reason, message and observedGeneration are identical, skip patch
		if prev.Status == cur.Status && prev.Reason == cur.Reason && prev.Message == cur.Message && prev.ObservedGeneration == cur.ObservedGeneration {
			return nil
		}
	}

	// Convert back to unstructured form
	newSlice := make([]any, 0, len(existing))
	for _, c := range existing {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&c)
		if err != nil {
			continue
		}
		newSlice = append(newSlice, m)
	}
	_ = unstructured.SetNestedSlice(nc.Object, newSlice, "status", "conditions")

	return r.Status().Patch(ctx, nc, client.MergeFrom(base))
}

// getReadyCondition extracts and returns the existing Ready condition from the
// Unstructured object's status.conditions, or nil if not present.
func getReadyCondition(nc *unstructured.Unstructured) *metav1.Condition {
	conds, found, _ := unstructured.NestedSlice(nc.Object, "status", "conditions")
	if !found {
		return nil
	}
	for _, it := range conds {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		var c metav1.Condition
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &c); err == nil {
			if c.Type == conditionTypeReady {
				return &c
			}
		}
	}
	return nil
}
