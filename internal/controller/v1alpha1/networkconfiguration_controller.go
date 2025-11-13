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
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/vitistack/common/pkg/loggers/vlog"
	viticommonconditions "github.com/vitistack/common/pkg/operator/conditions"
	viticommonfinalizers "github.com/vitistack/common/pkg/operator/finalizers"
	reconcileutil "github.com/vitistack/common/pkg/operator/reconcileutil"
	vitistackcrdsv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	keaservice "github.com/vitistack/kea-operator/internal/services/kea"
	"github.com/vitistack/kea-operator/pkg/interfaces/keainterface"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// NetworkConfigurationReconciler reconciles vitistack.io/v1alpha1 NetworkConfiguration
// resources. It works with the generated typed CR to ensure DHCP reservations in Kea
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

// Reconcile fetches the NetworkConfiguration Custom Resource, reads MAC addresses
// from spec.networkInterfaces[].macAddress, looks up the NetworkNamespace IPv4
// prefix, resolves the Kea subnet-id, and for each MAC requires an existing Kea
// lease or creates a reservation for that IP within the subnet. Status conditions
// and fields are patched directly on the typed object.
func (r *NetworkConfigurationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	nc := &vitistackcrdsv1alpha1.NetworkConfiguration{}
	if err := r.Get(ctx, req.NamespacedName, nc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !nc.GetDeletionTimestamp().IsZero() {
		return r.handleDeletion(ctx, nc, log)
	}

	// Ensure finalizer
	if !viticommonfinalizers.Has(nc, finalizerName) {
		if err := viticommonfinalizers.Ensure(ctx, r.Client, nc, finalizerName); err != nil {
			return reconcileutil.Requeue(err)
		}
		return ctrl.Result{}, nil
	}

	// Set reconciling status
	if ready := getReadyCondition(nc); ready == nil || ready.ObservedGeneration != nc.GetGeneration() {
		_ = r.setCondition(ctx, nc, viticommonconditions.New(
			conditionTypeReady, metav1.ConditionFalse, conditionReasonReconciling, "reconciling", nc.GetGeneration(),
		))
		_ = r.updateStatus(ctx, nc, "Reconciling", "InProgress", "Reconciliation in progress", nil)
	}

	// Get IPv4 prefix from NetworkNamespace
	ipv4Prefix, err := r.getIPv4PrefixFromNetworkNamespace(ctx, req.Namespace)
	if err != nil {
		log.Error(err, "failed to get NetworkNamespace ipv4_prefix", "namespace", req.Namespace)
		_ = r.updateStatus(ctx, nc, "Error", "Failed", fmt.Sprintf("NetworkNamespace not found: %v", err), nil)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Extract MACs
	macs := extractMACsFromTypedNetworkConfiguration(nc)
	if len(macs) == 0 {
		log.Info("no MAC addresses found on NetworkConfiguration; skipping reservation", "name", nc.GetName(), "namespace", nc.GetNamespace())
		_ = r.updateStatus(ctx, nc, "Ready", "Success", "No MAC addresses to configure", nil)
		return ctrl.Result{}, nil
	}

	// Resolve Kea subnet
	subnetID, err := r.Kea.GetSubnetID(ctx, ipv4Prefix)
	if err != nil {
		return r.handleSubnetResolutionError(ctx, nc, ipv4Prefix, err, log)
	}

	// Get subnet details (gateway, DNS, etc.)
	subnetInfo, err := r.Kea.GetSubnetInfo(ctx, subnetID)
	if err != nil {
		log.Error(err, "failed to get subnet details", "subnetID", subnetID)
		// Non-fatal - continue without detailed subnet info
	}

	// Process MAC reservations
	macToIP, macToSubnetID, errs := r.processMACReservations(ctx, macs, subnetID, ipv4Prefix, log)

	// Build status interfaces
	statusInterfaces := r.buildStatusInterfaces(nc, macToIP, macToSubnetID, ipv4Prefix, subnetInfo)

	// Handle errors
	if len(errs) > 0 {
		_ = r.setCondition(ctx, nc, viticommonconditions.New(
			conditionTypeReady, metav1.ConditionFalse, conditionReasonError, fmt.Sprintf("reservation errors: %s", strings.Join(errs, "; ")), nc.GetGeneration(),
		))
		_ = r.updateStatus(ctx, nc, "Error", "Failed", strings.Join(errs, "; "), statusInterfaces)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Build success message
	statusMsg := r.buildSuccessMessage(len(macs), len(macToIP))

	_ = r.setCondition(ctx, nc, viticommonconditions.New(
		conditionTypeReady, metav1.ConditionTrue, conditionReasonConfigured, "configured", nc.GetGeneration(),
	))
	_ = r.updateStatus(ctx, nc, "Ready", "Success", statusMsg, statusInterfaces)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// handleDeletion handles the deletion of a NetworkConfiguration
func (r *NetworkConfigurationReconciler) handleDeletion(ctx context.Context, nc *vitistackcrdsv1alpha1.NetworkConfiguration, log logr.Logger) (ctrl.Result, error) {
	if err := r.cleanupReservations(ctx, nc); err != nil {
		log.V(1).Info("reservation cleanup during deletion encountered an issue", "error", err)
	}
	if err := viticommonfinalizers.Remove(ctx, r.Client, nc, finalizerName); err != nil {
		return reconcileutil.Requeue(err)
	}
	return ctrl.Result{}, nil
}

// handleSubnetResolutionError handles errors when resolving the Kea subnet
func (r *NetworkConfigurationReconciler) handleSubnetResolutionError(ctx context.Context, nc *vitistackcrdsv1alpha1.NetworkConfiguration, ipv4Prefix string, err error, log logr.Logger) (ctrl.Result, error) {
	log.Error(err, "failed to resolve Kea subnet id", "ipv4Prefix", ipv4Prefix)
	txt := strings.ToLower(err.Error())
	_ = r.setCondition(ctx, nc, viticommonconditions.New(
		conditionTypeReady, metav1.ConditionFalse, conditionReasonError, fmt.Sprintf("resolve subnet: %v", err), nc.GetGeneration(),
	))
	_ = r.updateStatus(ctx, nc, "Error", "Failed", fmt.Sprintf("Subnet resolution failed: %v", err), nil)
	if strings.Contains(txt, "unsupported kea command") || strings.Contains(txt, "not supported") {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// processMACReservations processes all MAC address reservations
func (r *NetworkConfigurationReconciler) processMACReservations(ctx context.Context, macs []string, subnetID int, ipv4Prefix string, log logr.Logger) (map[string]string, map[string]int, []string) {
	macToIP := make(map[string]string)
	macToSubnetID := make(map[string]int)
	var errs []string

	var ipnet *net.IPNet
	if _, n, e := net.ParseCIDR(strings.TrimSpace(ipv4Prefix)); e == nil {
		ipnet = n
	}

	for _, mac := range macs {
		ip, leaseSubnetID, _ := r.Kea.GetLeaseIPv4ForMAC(ctx, mac)

		sid := subnetID
		if leaseSubnetID > 0 {
			sid = leaseSubnetID
		}

		if ip != "" && ipnet != nil {
			if p := net.ParseIP(ip); p == nil || p.To4() == nil || !ipnet.Contains(p) {
				log.Info("lease IP not within expected prefix, will create MAC-only reservation",
					"mac", mac, "leaseIP", ip, "expectedPrefix", ipv4Prefix)
				ip = ""
			}
		}

		if err := r.Kea.EnsureReservationForMACIP(ctx, mac, sid, ip); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", mac, err))
			continue
		}

		macToSubnetID[mac] = sid
		if ip != "" {
			macToIP[mac] = ip
			log.Info("configured DHCP reservation with IP", "mac", mac, "ip", ip, "subnetID", sid, "subnet", ipv4Prefix)
		} else {
			log.Info("created MAC-only reservation, IP will be auto-allocated on DHCP request", "mac", mac, "subnetID", sid, "subnet", ipv4Prefix)
		}
	}

	return macToIP, macToSubnetID, errs
}

// buildStatusInterfaces builds the status interface array with all available information
func (r *NetworkConfigurationReconciler) buildStatusInterfaces(nc *vitistackcrdsv1alpha1.NetworkConfiguration, macToIP map[string]string, macToSubnetID map[string]int, ipv4Prefix string, subnetInfo *keaservice.SubnetInfo) []vitistackcrdsv1alpha1.NetworkConfigurationInterface {
	statusInterfaces := make([]vitistackcrdsv1alpha1.NetworkConfigurationInterface, 0, len(nc.Spec.NetworkInterfaces))

	for _, iface := range nc.Spec.NetworkInterfaces {
		normalizedMAC := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(iface.MacAddress, "-", ":")))
		statusIface := vitistackcrdsv1alpha1.NetworkConfigurationInterface{
			Name:         iface.Name,
			MacAddress:   iface.MacAddress,
			Vlan:         iface.Vlan,
			DHCPReserved: false,
		}

		// Check if reservation was successfully created
		if _, ok := macToSubnetID[normalizedMAC]; ok {
			statusIface.DHCPReserved = true
		}

		// Set IP and subnet info
		if ip, ok := macToIP[normalizedMAC]; ok {
			statusIface.IPv4Addresses = []string{ip}
			statusIface.IPv4Subnet = ipv4Prefix
		} else {
			// Still set subnet even if no IP yet
			statusIface.IPv4Subnet = ipv4Prefix
		}

		// Add gateway and DNS from subnet info if available
		if subnetInfo != nil {
			if subnetInfo.Gateway != "" {
				statusIface.IPv4Gateway = subnetInfo.Gateway
			}
			if len(subnetInfo.DNS) > 0 {
				statusIface.DNS = subnetInfo.DNS
			}
		}

		statusInterfaces = append(statusInterfaces, statusIface)
	}

	return statusInterfaces
}

// buildSuccessMessage creates a human-readable success message based on reservation results
func (r *NetworkConfigurationReconciler) buildSuccessMessage(totalMACs, resolvedIPs int) string {
	if resolvedIPs == totalMACs {
		return fmt.Sprintf("All %d MAC reservations configured with assigned IPs", totalMACs)
	} else if resolvedIPs > 0 {
		return fmt.Sprintf("%d MAC reservations configured (%d with IPs, %d will get IPs on DHCP request)", totalMACs, resolvedIPs, totalMACs-resolvedIPs)
	}
	return fmt.Sprintf("All %d MAC reservations configured (IPs will be auto-allocated on DHCP request)", totalMACs)
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

// SetupWithManager registers the controller with the manager using the typed
// NetworkConfiguration resource.
func (r *NetworkConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vitistackcrdsv1alpha1.NetworkConfiguration{}).
		Named("networkconfiguration").
		Complete(r)
}

// getIPv4PrefixFromNetworkNamespace returns the NetworkNamespace.Status.IPv4Prefix
// for the provided Kubernetes namespace by listing the typed NetworkNamespace objects.
func (r *NetworkConfigurationReconciler) getIPv4PrefixFromNetworkNamespace(ctx context.Context, namespace string) (string, error) {
	nnList := &vitistackcrdsv1alpha1.NetworkNamespaceList{}
	if err := r.List(ctx, nnList, client.InNamespace(namespace)); err != nil {
		return "", err
	}
	if len(nnList.Items) == 0 {
		return "", fmt.Errorf("no NetworkNamespace found in namespace %s", namespace)
	}
	nn := nnList.Items[0]
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

// cleanupReservations performs a best-effort removal of reservations on delete.
// It reads MACs from the typed NetworkConfiguration, resolves the subnet-id for
// the namespace prefix, and issues reservation deletions in Kea.
func (r *NetworkConfigurationReconciler) cleanupReservations(ctx context.Context, nc *vitistackcrdsv1alpha1.NetworkConfiguration) error {
	ipv4Prefix, err := r.getIPv4PrefixFromNetworkNamespace(ctx, nc.GetNamespace())
	if err != nil {
		vlog.Debug("skipping reservation cleanup, NetworkNamespace not available",
			"namespace", nc.GetNamespace(), "error", err)
		return err
	}
	subnetID, err := r.Kea.GetSubnetID(ctx, ipv4Prefix)
	if err != nil {
		vlog.Debug("skipping reservation cleanup, subnet not found in KEA",
			"ipv4Prefix", ipv4Prefix, "error", err)
		return err
	}
	macs := extractMACsFromTypedNetworkConfiguration(nc)
	for _, mac := range macs {
		_ = r.Kea.DeleteReservationForMAC(ctx, mac, subnetID)
	}
	return nil
}

// setCondition patches the status.conditions on the provided Unstructured object
// using the common conditions helper, and avoids no-op patches when the condition
// did not meaningfully change.
func (r *NetworkConfigurationReconciler) setCondition(ctx context.Context, nc *vitistackcrdsv1alpha1.NetworkConfiguration, cond metav1.Condition) error {
	base := nc.DeepCopy()
	prev := findCondition(base.Status.Conditions, cond.Type)

	updated := nc.DeepCopy()
	viticommonconditions.SetOrUpdateCondition(&updated.Status.Conditions, &cond)
	cur := findCondition(updated.Status.Conditions, cond.Type)

	if prev != nil && cur != nil {
		if prev.Status == cur.Status && prev.Reason == cur.Reason && prev.Message == cur.Message && prev.ObservedGeneration == cur.ObservedGeneration {
			return nil
		}
	}

	if err := r.Status().Patch(ctx, updated, client.MergeFrom(base)); err != nil {
		return err
	}

	nc.Status.Conditions = updated.Status.Conditions
	nc.SetResourceVersion(updated.GetResourceVersion())
	return nil
}

func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

func getReadyCondition(nc *vitistackcrdsv1alpha1.NetworkConfiguration) *metav1.Condition {
	return findCondition(nc.Status.Conditions, conditionTypeReady)
}

// updateStatus updates the full status subresource including phase, status, message,
// created timestamp, and network interfaces with their resolved IPs.
func (r *NetworkConfigurationReconciler) updateStatus(ctx context.Context, nc *vitistackcrdsv1alpha1.NetworkConfiguration, phase, status, message string, networkInterfaces []vitistackcrdsv1alpha1.NetworkConfigurationInterface) error {
	base := nc.DeepCopy()
	updated := nc.DeepCopy()
	changed := false

	if phase != "" && updated.Status.Phase != phase {
		updated.Status.Phase = phase
		changed = true
	}
	if status != "" && updated.Status.Status != status {
		updated.Status.Status = status
		changed = true
	}
	if message != "" && updated.Status.Message != message {
		updated.Status.Message = message
		changed = true
	}
	if updated.Status.Created.IsZero() {
		updated.Status.Created = metav1.Now()
		changed = true
	}
	if len(networkInterfaces) > 0 {
		if !reflect.DeepEqual(updated.Status.NetworkInterfaces, networkInterfaces) {
			updated.Status.NetworkInterfaces = networkInterfaces
			changed = true
		}
	}

	if !changed {
		return nil
	}

	if err := r.Status().Patch(ctx, updated, client.MergeFrom(base)); err != nil {
		return err
	}

	nc.Status = updated.Status
	nc.SetResourceVersion(updated.GetResourceVersion())
	return nil
}
