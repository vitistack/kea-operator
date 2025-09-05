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

package controller

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	viticommonconditions "github.com/vitistack/common/pkg/operator/conditions"
	viticommonfinalizers "github.com/vitistack/common/pkg/operator/finalizers"
	reconcileutil "github.com/vitistack/common/pkg/operator/reconcileutil"
	"github.com/vitistack/kea-operator/pkg/interfaces/keainterface"
	"github.com/vitistack/kea-operator/pkg/models/keamodels"
)

// NetworkConfigurationReconciler reconciles a NetworkConfiguration object
type NetworkConfigurationReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	KeaClient keainterface.KeaClient
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

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the NetworkConfiguration object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *NetworkConfigurationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Reconciling NetworkConfiguration")

	// 1) Fetch the NetworkConfiguration
	nc := &unstructured.Unstructured{}
	nc.SetGroupVersionKind(schema.GroupVersionKind{Group: "vitistack.io", Version: "v1alpha1", Kind: "NetworkConfiguration"})
	if err := r.Get(ctx, req.NamespacedName, nc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Ensure finalizer on non-deleted objects
	if nc.GetDeletionTimestamp().IsZero() {
		if !viticommonfinalizers.Has(nc, finalizerName) {
			if err := viticommonfinalizers.Ensure(ctx, r.Client, nc, finalizerName); err != nil {
				return reconcileutil.Requeue(err)
			}
			return ctrl.Result{}, nil
		}
	} else {
		// Handle deletion: best-effort cleanup then remove finalizer
		if err := r.cleanupReservations(ctx, nc); err != nil {
			log.Error(err, "cleanup during deletion failed")
		}
		if err := viticommonfinalizers.Remove(ctx, r.Client, nc, finalizerName); err != nil {
			return reconcileutil.Requeue(err)
		}
		return ctrl.Result{}, nil
	}

	// Mark Reconciling only if not already recorded for current generation to avoid status update loop
	if !hasReadyReconcilingForGeneration(nc, nc.GetGeneration()) {
		_ = r.setCondition(ctx, nc, viticommonconditions.New(
			conditionTypeReady, metav1.ConditionFalse, conditionReasonReconciling, "reconciling", nc.GetGeneration(),
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
	macs := extractMACsFromNetworkConfiguration(nc)
	if len(macs) == 0 {
		log.Info("no MAC addresses found on NetworkConfiguration; skipping reservation", "name", nc.GetName(), "namespace", nc.GetNamespace())
		// No error; just exit without requeue
		return ctrl.Result{}, nil
	}

	// 4) Find subnet-id for this prefix in Kea
	subnetID, err := r.getKeaSubnetID(ctx, ipv4Prefix)
	if err != nil {
		log.Error(err, "failed to resolve Kea subnet id", "ipv4Prefix", ipv4Prefix)
		txt := strings.ToLower(err.Error())
		_ = r.setCondition(ctx, nc, viticommonconditions.New(
			conditionTypeReady, metav1.ConditionFalse, conditionReasonError, fmt.Sprintf("resolve subnet: %v", err), nc.GetGeneration(),
		))
		// Do not hot-loop if command unsupported; just return without requeue (will reconcile on next event or resync)
		if strings.Contains(txt, "unsupported kea command") || strings.Contains(txt, "not supported") {
			return ctrl.Result{}, nil
		}
		// Otherwise requeue (transient error)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// 5) Ensure reservations per MAC in Kea (idempotent)
	for _, mac := range macs {
		if err := r.ensureKeaReservationForMAC(ctx, mac, subnetID); err != nil {
			log.Error(err, "failed to ensure Kea reservation for MAC", "mac", mac, "subnetID", subnetID)
			// continue
		}
	}

	// Success: set Ready True
	_ = r.setCondition(ctx, nc, viticommonconditions.New(
		conditionTypeReady, metav1.ConditionTrue, conditionReasonConfigured, "configured", nc.GetGeneration(),
	))
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func NewNetworkConfigurationReconciler(mgr ctrl.Manager, keaClient keainterface.KeaClient) *NetworkConfigurationReconciler {
	return &NetworkConfigurationReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		KeaClient: keaClient,
	}
}

func (r *NetworkConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Watch NetworkConfiguration as unstructured to avoid scheme coupling
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "vitistack.io", Version: "v1alpha1", Kind: "NetworkConfiguration"})
	return ctrl.NewControllerManagedBy(mgr).
		For(u).
		Named("networkconfiguration").
		Complete(r)
}

// getIPv4PrefixFromNetworkNamespace returns status.ipv4_prefix from the NetworkNamespace
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
	nn := nnList.Items[0]
	if v, found, _ := unstructured.NestedString(nn.Object, "status", "ipv4_prefix"); found && v != "" {
		return v, nil
	}
	return "", fmt.Errorf("NetworkNamespace missing status.ipv4_prefix in namespace %s", namespace)
}

// extractMACsFromNetworkConfiguration attempts to read MAC addresses from the NetworkConfiguration CR (spec or status).
// It tries several common field shapes and validates values as MAC addresses.
func extractMACsFromNetworkConfiguration(nc *unstructured.Unstructured) []string {
	// Work directly with unstructured for flexible traversal
	objMap := nc.Object

	// Utility to normalize and validate MACs
	addMAC := func(dst map[string]struct{}, val string) {
		s := strings.ToLower(strings.TrimSpace(val))
		if s == "" {
			return
		}
		if _, err := net.ParseMAC(s); err != nil {
			// try replacing '-' with ':'
			s2 := strings.ReplaceAll(s, "-", ":")
			if _, err2 := net.ParseMAC(s2); err2 != nil {
				return
			}
			s = s2
		}
		dst[s] = struct{}{}
	}

	found := map[string]struct{}{}

	// Candidate paths to look for arrays of MAC strings
	paths := [][]string{
		{"spec", "networkInterfaces"},
		{"status", "networkInterfaces"},
		{"spec", "macs"},
		{"status", "macs"},
	}

	for _, p := range paths {
		// If the path is an array of interface objects, search per-item common keys
		if arr, ok, _ := unstructured.NestedSlice(objMap, p...); ok {
			for _, it := range arr {
				switch v := it.(type) {
				case string:
					addMAC(found, v)
				case map[string]any:
					// common keys
					for _, k := range []string{"mac", "macAddress", "hwAddress", "hw-address", "macs"} {
						if val, ok := v[k]; ok {
							switch vv := val.(type) {
							case string:
								addMAC(found, vv)
							case []any:
								for _, e := range vv {
									if s, ok := e.(string); ok {
										addMAC(found, s)
									}
								}
							case []string:
								for _, s := range vv {
									addMAC(found, s)
								}
							}
						}
					}
				}
			}
		}
	}

	// Also check top-level convenience fields
	for _, key := range []string{"spec", "status"} {
		if m, ok := objMap[key].(map[string]any); ok {
			if val, ok2 := m["mac"].(string); ok2 {
				addMAC(found, val)
			}
			if val, ok2 := m["macAddress"].(string); ok2 {
				addMAC(found, val)
			}
		}
	}

	// Convert set to slice
	out := make([]string, 0, len(found))
	for k := range found {
		out = append(out, k)
	}
	return out
}

// ensureKeaLeaseForMAC ensures a lease exists for the given MAC; if missing, it adds one using an IP from the prefix
// Note: legacy lease-based helpers were removed in favor of reservation-based flow.

// cleanupReservations best-effort removal of reservations on delete.

func (r *NetworkConfigurationReconciler) cleanupReservations(ctx context.Context, nc *unstructured.Unstructured) error {
	ipv4Prefix, err := r.getIPv4PrefixFromNetworkNamespace(ctx, nc.GetNamespace())
	if err != nil {
		return err
	}
	subnetID, err := r.getKeaSubnetID(ctx, ipv4Prefix)
	if err != nil {
		return err
	}
	macs := extractMACsFromNetworkConfiguration(nc)
	for _, mac := range macs {
		_ = r.deleteKeaReservationForMAC(ctx, mac, subnetID)
	}
	return nil
}

func (r *NetworkConfigurationReconciler) deleteKeaReservationForMAC(ctx context.Context, mac string, subnetID int) error {
	mac = strings.ToLower(strings.TrimSpace(mac))
	if mac == "" {
		return fmt.Errorf("missing mac")
	}
	delReq := keamodels.Request{
		Command: "reservation-del",
		Args: map[string]any{
			"subnet-id":        subnetID,
			"identifier-type":  "hw-address",
			"identifier":       mac,
			"operation-target": "all",
		},
	}
	resp, err := r.KeaClient.Send(ctx, delReq)
	if err != nil {
		return err
	}
	if resp.Result != 0 {
		return fmt.Errorf("kea reservation-del failed: %s", resp.Text)
	}
	return nil
}

// setCondition patches the status with the given condition using common conditions helper.
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

// getKeaSubnetID lists Kea subnets and returns the id of the subnet matching the given IPv4 CIDR prefix.
func (r *NetworkConfigurationReconciler) getKeaSubnetID(ctx context.Context, ipv4Prefix string) (int, error) {
	req := keamodels.Request{Command: "subnet4-list", Args: map[string]any{}}
	resp, err := r.KeaClient.Send(ctx, req)
	if err != nil {
		return 0, err
	}
	if resp.Result != 0 {
		// If command unsupported we should not hot-loop endlessly.
		if strings.Contains(strings.ToLower(resp.Text), "not supported") {
			return 0, fmt.Errorf("unsupported kea command subnet4-list: %s", resp.Text)
		}
		return 0, fmt.Errorf("kea subnet4-list failed: %s", resp.Text)
	}
	subnets, ok := resp.Arguments["subnets"].([]any)
	if !ok {
		return 0, fmt.Errorf("unexpected subnet4-list response shape")
	}
	for _, s := range subnets {
		m, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if sub, ok := m["subnet"].(string); ok && sub == ipv4Prefix {
			switch idv := m["id"].(type) {
			case float64:
				return int(idv), nil
			case int:
				return idv, nil
			}
		}
	}
	return 0, fmt.Errorf("no matching Kea subnet for prefix %s", ipv4Prefix)
}

// ensureKeaReservationForMAC ensures a reservation for the MAC exists in the given subnet.
// If not found, it adds one without explicit IP (server assigns), targeting both memory and DB (operation-target=all).
func (r *NetworkConfigurationReconciler) ensureKeaReservationForMAC(ctx context.Context, mac string, subnetID int) error {
	mac = strings.ToLower(strings.TrimSpace(mac))
	if mac == "" {
		return fmt.Errorf("missing mac")
	}
	// Check existing reservation by hardware address
	getReq := keamodels.Request{
		Command: "reservation-get-by-id",
		Args: map[string]any{
			"identifier-type": "hw-address",
			"identifier":      mac,
		},
	}
	resp, err := r.KeaClient.Send(ctx, getReq)
	if err == nil && resp.Result == 0 {
		// Exists
		return nil
	}
	// Add reservation
	addReq := keamodels.Request{
		Command: "reservation-add",
		Args: map[string]any{
			"reservation": map[string]any{
				"subnet-id":  subnetID,
				"hw-address": mac,
			},
			"operation-target": "all",
		},
	}
	addResp, addErr := r.KeaClient.Send(ctx, addReq)
	if addErr != nil {
		return addErr
	}
	if addResp.Result != 0 {
		return fmt.Errorf("kea reservation-add failed: %s", addResp.Text)
	}
	return nil
}

// hasReadyReconcilingForGeneration returns true if the Ready condition already reflects
// a Reconciling (False/Reconciling) state for the provided generation, preventing redundant status patches.
func hasReadyReconcilingForGeneration(nc *unstructured.Unstructured, generation int64) bool {
	conds, found, _ := unstructured.NestedSlice(nc.Object, "status", "conditions")
	if !found {
		return false
	}
	for _, it := range conds {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		typeStr, _ := m["type"].(string)
		statusStr, _ := m["status"].(string)
		reasonStr, _ := m["reason"].(string)
		// ObservedGeneration may be encoded as float64 in unstructured
		var obsGen int64
		switch v := m["observedGeneration"].(type) {
		case int64:
			obsGen = v
		case int32:
			obsGen = int64(v)
		case float64:
			obsGen = int64(v)
		}
		if typeStr == conditionTypeReady && statusStr == string(metav1.ConditionFalse) && reasonStr == conditionReasonReconciling && obsGen == generation {
			return true
		}
	}
	return false
}
