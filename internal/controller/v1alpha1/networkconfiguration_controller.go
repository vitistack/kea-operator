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
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/viper"
	"github.com/vitistack/common/pkg/loggers/vlog"
	viticommonconditions "github.com/vitistack/common/pkg/operator/conditions"
	viticommonfinalizers "github.com/vitistack/common/pkg/operator/finalizers"
	reconcileutil "github.com/vitistack/common/pkg/operator/reconcileutil"
	vitistackcrdsv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/kea-operator/internal/consts"
	keaservice "github.com/vitistack/kea-operator/internal/services/kea"
	subnetutil "github.com/vitistack/kea-operator/internal/util/subnet"
	"github.com/vitistack/kea-operator/pkg/interfaces/keainterface"
	"github.com/vitistack/kea-operator/pkg/models/keamodels"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
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

	// CleanupTimeout is how long deletion keeps retrying a failed Kea
	// reservation cleanup before removing the finalizer anyway.
	CleanupTimeout time.Duration
}

// skipCleanupAnnotation, set to "true" on a NetworkConfiguration being
// deleted, removes the finalizer without touching Kea.
const skipCleanupAnnotation = "vitistack.io/kea-skip-cleanup"

const (
	finalizerName              = "vitistack.io/networkconfiguration-finalizer"
	conditionTypeReady         = "Ready"
	conditionReasonReconciling = "Reconciling"
	conditionReasonConfigured  = "Configured"
	conditionReasonError       = "Error"

	// RequeueDelaySuccess is the resync interval after a successful reconcile.
	// The watch on NetworkConfiguration already triggers a reconcile on spec
	// changes, so this is just a periodic safety net to catch out-of-band
	// drift in Kea state. Keep it long to avoid hammering the Kea API.
	RequeueDelaySuccess = 5 * time.Minute
	// RequeueDelayError is the retry delay after a transient error. Short
	// enough to recover quickly from a flapping Kea peer, long enough not to
	// pile on if the Kea Control Agent is overloaded.
	RequeueDelayError = 30 * time.Second
	// RequeueDelayNetworkNamespaceMissing is the retry delay while an NC's
	// NetworkNamespace doesn't exist (yet). NetworkNamespaces aren't watched,
	// so without a retry the NC would wait for its own next change.
	RequeueDelayNetworkNamespaceMissing = 2 * time.Minute
	// DefaultCleanupTimeout is the CleanupTimeout used when none is configured.
	DefaultCleanupTimeout = 15 * time.Minute
)

// errNoNetworkNamespace reports that a namespace has no NetworkNamespace.
var errNoNetworkNamespace = errors.New("no NetworkNamespace found")

// deprecationWarned tracks namespaces for which the deprecation warning has already been logged,
// so we don't spam the logs on every reconcile loop.
var deprecationWarned sync.Map

// +kubebuilder:rbac:groups=vitistack.io,resources=networkconfigurations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vitistack.io,resources=networkconfigurations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vitistack.io,resources=networkconfigurations/finalizers,verbs=update
// +kubebuilder:rbac:groups=vitistack.io,resources=networknamespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

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

	// Handle deletion before any triage: if we ever claimed this NC (it carries
	// our finalizer), cleanup must run even if its provider or NetworkNamespace
	// changed since. NCs without our finalizer were never ours.
	if !nc.GetDeletionTimestamp().IsZero() {
		if !viticommonfinalizers.Has(nc, finalizerName) {
			return ctrl.Result{}, nil
		}
		return r.handleDeletion(ctx, nc, log)
	}

	// Provider triage — if spec.provider is explicitly set to something other
	// than 'kea', this NetworkConfiguration belongs to another operator.
	// Skip silently so we don't spam logs for resources that aren't ours.
	if vitistackcrdsv1alpha1.IsProviderSet(nc.Spec.Provider) &&
		!vitistackcrdsv1alpha1.MatchesProvider(nc.Spec.Provider, vitistackcrdsv1alpha1.ProviderNameKea) {
		log.V(1).Info("skipping NetworkConfiguration, spec.provider is not 'kea'",
			"provider", nc.Spec.Provider, "name", nc.Name)
		return ctrl.Result{}, nil
	}

	// Fetch the NetworkNamespace silently. We need it to decide whether this
	// NC is actually for us (type=dhcp or unset default) before logging.
	nn, fallbackUsed, err := r.getNetworkNamespace(ctx, req.Namespace, nc.Spec.NetworkNamespaceName)
	if err != nil {
		// NN fetch failed — can't determine ownership. Log at V(1) so we don't
		// spam for resources that likely aren't ours, and retry: the NN may not
		// exist yet, and NetworkNamespaces aren't watched.
		retry := networkNamespaceRetryDelay(err)
		log.V(1).Info("unable to fetch NetworkNamespace for triage", "namespace", req.Namespace, "error", err.Error(), "retryIn", retry)
		return ctrl.Result{RequeueAfter: retry}, nil
	}

	// Type triage — if the NN has an explicit non-DHCP allocation type, this
	// resource belongs to another operator (e.g. static-ip-operator). Skip
	// silently without emitting any warnings or status updates.
	if nn.Spec.IPAllocation != nil && nn.Spec.IPAllocation.Type != vitistackcrdsv1alpha1.IPAllocationTypeDHCP {
		log.V(1).Info("skipping DHCP reconciliation, NetworkNamespace uses a different IP allocation type",
			"type", nn.Spec.IPAllocation.Type, "networkNamespace", nn.Name)
		return ctrl.Result{}, nil
	}

	// --- Past triage: this NC is ours (type=dhcp or defaulting to DHCP). ---

	// Warn (once) about the fallback NN lookup when we're actually handling
	// this NC, so we don't emit it for resources that belong elsewhere.
	if fallbackUsed {
		if _, alreadyWarned := deprecationWarned.LoadOrStore("nnname-"+req.Namespace, true); !alreadyWarned {
			log.Info("WARNING: NetworkConfiguration has no spec.networkNamespaceName set; "+
				"falling back to listing NetworkNamespaces in namespace for backward compatibility. "+
				"Please set spec.networkNamespaceName explicitly.",
				"namespace", req.Namespace)
		}
	}

	// Strict / warning for unset spec.provider on NC.
	if !vitistackcrdsv1alpha1.IsProviderSet(nc.Spec.Provider) {
		if viper.GetBool(consts.KEA_STRICT_DEFAULTS) {
			msg := "spec.provider is not set and KEA_STRICT_DEFAULTS is enabled; refusing to default to 'kea'. Set spec.provider to 'kea' explicitly."
			log.Info("WARNING: "+msg, "name", nc.Name, "namespace", nc.Namespace)
			_ = r.setCondition(ctx, nc, viticommonconditions.New(
				conditionTypeReady, metav1.ConditionFalse, conditionReasonError, msg, nc.GetGeneration(),
			))
			_ = r.updateStatus(ctx, nc, "Error", "Failed", msg, nil)
			return ctrl.Result{}, nil
		}
		if _, alreadyWarned := deprecationWarned.LoadOrStore("ncprov-"+nc.Namespace+"/"+nc.Name, true); !alreadyWarned {
			log.Info("WARNING: NetworkConfiguration does not have spec.provider set. "+
				"The kea-operator is handling it by default for backward compatibility. "+
				"Please set spec.provider to 'kea' explicitly. "+
				"Set KEA_STRICT_DEFAULTS=true to force migration.",
				"name", nc.Name, "namespace", nc.Namespace)
		}
	}

	// Strict / warning for nil spec.ipAllocation on NN.
	if nn.Spec.IPAllocation == nil {
		if viper.GetBool(consts.KEA_STRICT_DEFAULTS) {
			msg := fmt.Sprintf("NetworkNamespace %s has no spec.ipAllocation and KEA_STRICT_DEFAULTS is enabled; refusing to default to DHCP. Set spec.ipAllocation.type to 'dhcp' explicitly.", nn.Name)
			log.Info("WARNING: "+msg, "networkNamespace", nn.Name, "namespace", req.Namespace)
			_ = r.setCondition(ctx, nc, viticommonconditions.New(
				conditionTypeReady, metav1.ConditionFalse, conditionReasonError, msg, nc.GetGeneration(),
			))
			_ = r.updateStatus(ctx, nc, "Error", "Failed", msg, nil)
			return ctrl.Result{}, nil
		}
		if _, alreadyWarned := deprecationWarned.LoadOrStore("ipalloc-"+nn.Namespace+"/"+nn.Name, true); !alreadyWarned {
			log.Info("WARNING: NetworkNamespace does not have spec.ipAllocation set. "+
				"Defaulting to DHCP behavior for backward compatibility. "+
				"Please set spec.ipAllocation.type to 'dhcp' and set spec.provider to 'kea' on NetworkConfigurations. "+
				"Set KEA_STRICT_DEFAULTS=true to force migration.",
				"networkNamespace", nn.Name, "namespace", req.Namespace)
		}
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

	ipv4Prefix := nn.Status.IPv4Prefix
	if ipv4Prefix == "" {
		log.Error(nil, "NetworkNamespace missing status.IPv4Prefix", "namespace", req.Namespace, "networkNamespace", nn.Name)
		_ = r.updateStatus(ctx, nc, "Error", "Failed", fmt.Sprintf("NetworkNamespace %s missing status.IPv4Prefix", nn.Name), nil)
		return ctrl.Result{RequeueAfter: RequeueDelayError}, nil
	}

	// Extract MACs
	macs := extractMACsFromTypedNetworkConfiguration(nc)
	if len(macs) == 0 {
		log.Info("no MAC addresses found on NetworkConfiguration; skipping reservation", "name", nc.GetName(), "namespace", nc.GetNamespace())
		_ = r.updateStatus(ctx, nc, "Ready", "Success", "No MAC addresses to configure", nil)
		return ctrl.Result{}, nil
	}

	// Resolve or create Kea subnet
	poolCfg, err := subnetutil.CalculatePoolFromCIDR(ipv4Prefix)
	if err != nil {
		log.Error(err, "failed to calculate pool from CIDR", "ipv4Prefix", ipv4Prefix)
		_ = r.updateStatus(ctx, nc, "Error", "Failed", fmt.Sprintf("Invalid CIDR: %v", err), nil)
		return ctrl.Result{RequeueAfter: RequeueDelayError}, nil
	}

	// Get require-client-classes from configuration
	var requireClientClasses []string
	if classes := viper.GetString(consts.KEA_REQUIRE_CLIENT_CLASSES); classes != "" {
		for c := range strings.SplitSeq(classes, ",") {
			if trimmed := strings.TrimSpace(c); trimmed != "" {
				requireClientClasses = append(requireClientClasses, trimmed)
			}
		}
	}

	subnetCfg := keamodels.SubnetConfig{
		Subnet:               ipv4Prefix,
		Gateway:              poolCfg.Gateway,
		PoolStart:            poolCfg.PoolStart,
		PoolEnd:              poolCfg.PoolEnd,
		RequireClientClasses: requireClientClasses,
	}
	subnetID, created, err := r.Kea.GetOrCreateSubnet(ctx, subnetCfg)
	if err != nil {
		log.Error(err, "failed to get or create Kea subnet", "ipv4Prefix", ipv4Prefix)
		_ = r.setCondition(ctx, nc, viticommonconditions.New(
			conditionTypeReady, metav1.ConditionFalse, conditionReasonError, fmt.Sprintf("subnet error: %v", err), nc.GetGeneration(),
		))
		_ = r.updateStatus(ctx, nc, "Error", "Failed", fmt.Sprintf("Subnet error: %v", err), nil)
		return ctrl.Result{RequeueAfter: RequeueDelayError}, nil
	}
	if created {
		log.Info("created new Kea subnet", "subnet", ipv4Prefix, "subnetID", subnetID)
	}

	// Get subnet details (gateway, DNS, etc.). Subnet info lookup is non-fatal —
	// reservations still proceed without gateway/DNS, just with less status detail.
	subnetID, subnetInfo := r.resolveSubnetInfo(ctx, subnetID, ipv4Prefix, log)

	// Process MAC reservations
	outcomes, errs, warnings := r.processMACReservations(ctx, nc, macs, subnetID, ipv4Prefix, log)
	statusInterfaces := r.buildStatusInterfaces(nc, outcomes, ipv4Prefix, subnetInfo)
	return r.reportReservations(ctx, nc, len(macs), outcomes, errs, warnings, statusInterfaces), nil
}

// reportReservations writes the outcome of processMACReservations to status
// and picks when to reconcile next.
func (r *NetworkConfigurationReconciler) reportReservations(ctx context.Context, nc *vitistackcrdsv1alpha1.NetworkConfiguration, totalMACs int, outcomes map[string]macOutcome, errs, warnings []string, statusInterfaces []vitistackcrdsv1alpha1.NetworkConfigurationInterface) ctrl.Result {
	resolvedIPs := 0
	for _, o := range outcomes {
		if o.ip != "" {
			resolvedIPs++
		}
	}

	// Handle errors
	if len(errs) > 0 {
		_ = r.setCondition(ctx, nc, viticommonconditions.New(
			conditionTypeReady, metav1.ConditionFalse, conditionReasonError, fmt.Sprintf("reservation errors: %s", strings.Join(errs, "; ")), nc.GetGeneration(),
		))
		_ = r.updateStatus(ctx, nc, "Error", "Failed", withWarnings(strings.Join(errs, "; "), warnings), statusInterfaces)
		return ctrl.Result{RequeueAfter: RequeueDelayError}
	}

	// Build success message
	statusMsg := withWarnings(r.buildSuccessMessage(totalMACs, resolvedIPs), warnings)

	_ = r.setCondition(ctx, nc, viticommonconditions.New(
		conditionTypeReady, metav1.ConditionTrue, conditionReasonConfigured, "configured", nc.GetGeneration(),
	))
	_ = r.updateStatus(ctx, nc, "Ready", "Success", statusMsg, statusInterfaces)

	// If not every MAC has resolved to an IP yet, the reservation is still
	// settling. Re-check on the short interval instead of waiting the full
	// success resync, so the IP lands in status — and therefore in the
	// downstream Machine's public IPs — in seconds rather than minutes.
	if resolvedIPs < totalMACs {
		return ctrl.Result{RequeueAfter: RequeueDelayError}
	}
	return ctrl.Result{RequeueAfter: RequeueDelaySuccess}
}

// handleDeletion removes this NC's reservations from Kea, then the finalizer.
// When cleanup fails it retries until CleanupTimeout has passed since deletion
// started, then removes the finalizer anyway so a Kea outage can't block
// teardown indefinitely. The skip-cleanup annotation removes it at once.
func (r *NetworkConfigurationReconciler) handleDeletion(ctx context.Context, nc *vitistackcrdsv1alpha1.NetworkConfiguration, log logr.Logger) (ctrl.Result, error) {
	if nc.GetAnnotations()[skipCleanupAnnotation] == "true" {
		log.Info("skipping Kea reservation cleanup on request", "annotation", skipCleanupAnnotation, "name", nc.Name, "namespace", nc.Namespace)
		return r.removeFinalizer(ctx, nc)
	}
	if err := r.cleanupReservations(ctx, nc); err != nil {
		timeout := r.cleanupTimeout()
		if waited := time.Since(nc.GetDeletionTimestamp().Time); waited < timeout {
			log.Info("Kea reservation cleanup failed, retrying before removing the finalizer",
				"name", nc.Name, "namespace", nc.Namespace, "error", err.Error(), "givingUpIn", (timeout - waited).Round(time.Second))
			_ = r.updateStatus(ctx, nc, "Deleting", "InProgress", fmt.Sprintf("Waiting for Kea reservation cleanup: %v", err), nil)
			return ctrl.Result{RequeueAfter: RequeueDelayError}, nil
		}
		log.Error(err, "Kea reservation cleanup still failing, removing the finalizer anyway; reservations may remain in Kea",
			"name", nc.Name, "namespace", nc.Namespace, "timeout", timeout)
	}
	return r.removeFinalizer(ctx, nc)
}

func (r *NetworkConfigurationReconciler) removeFinalizer(ctx context.Context, nc *vitistackcrdsv1alpha1.NetworkConfiguration) (ctrl.Result, error) {
	if err := viticommonfinalizers.Remove(ctx, r.Client, nc, finalizerName); client.IgnoreNotFound(err) != nil {
		return reconcileutil.Requeue(err)
	}
	return ctrl.Result{}, nil
}

func (r *NetworkConfigurationReconciler) cleanupTimeout() time.Duration {
	if r.CleanupTimeout > 0 {
		return r.CleanupTimeout
	}
	return DefaultCleanupTimeout
}

// resolveSubnetInfo fetches subnet details for subnetID, transparently recovering
// from a stale ID. When the previous reconcile ran against the secondary Kea peer
// during a failover, the ID we have may not exist on the primary; we re-resolve
// from CIDR and retry once. Returns the (possibly updated) subnet ID and the
// info, or a nil info if it can't be resolved (non-fatal).
func (r *NetworkConfigurationReconciler) resolveSubnetInfo(ctx context.Context, subnetID int, ipv4Prefix string, log logr.Logger) (int, *keaservice.SubnetInfo) {
	subnetInfo, err := r.Kea.GetSubnetInfo(ctx, subnetID)
	if err == nil {
		return subnetID, subnetInfo
	}
	errLower := strings.ToLower(err.Error())
	if strings.Contains(errLower, "no subnet with id") || strings.Contains(errLower, "not found") {
		if reID, reErr := r.Kea.GetSubnetID(ctx, ipv4Prefix); reErr == nil && reID != subnetID {
			log.Info("stale subnet ID, re-resolved from CIDR", "oldID", subnetID, "newID", reID, "cidr", ipv4Prefix)
			if info, err2 := r.Kea.GetSubnetInfo(ctx, reID); err2 == nil {
				return reID, info
			}
			subnetID = reID
		}
	}
	log.Error(err, "failed to get subnet details", "subnetID", subnetID)
	return subnetID, nil
}

// macOutcome is what status reports for one MAC.
type macOutcome struct {
	ip       string // address the machine holds; "" when unknown
	reserved bool   // the Kea reservation holds ip
}

// processMACReservations ensures a Kea reservation for each MAC and returns
// what status should report per MAC, plus per-MAC errors and warnings.
//
// When Kea fails for a MAC, its outcome keeps the lease IP if known, else the
// IP status reported before: other operators copy these IPs into Machine
// status, so a Kea problem must never make them disappear.
func (r *NetworkConfigurationReconciler) processMACReservations(ctx context.Context, nc *vitistackcrdsv1alpha1.NetworkConfiguration, macs []string, subnetID int, ipv4Prefix string, log logr.Logger) (map[string]macOutcome, []string, []string) {
	outcomes := make(map[string]macOutcome, len(macs))
	var errs, warnings []string
	previous := previousStatusByMAC(nc)

	var ipnet *net.IPNet
	if _, n, e := net.ParseCIDR(strings.TrimSpace(ipv4Prefix)); e == nil {
		ipnet = n
	}

	for _, mac := range macs {
		// The lease's subnet-id is ignored: Kea keeps it when subnets are
		// renumbered (they are re-created with new IDs after a Kea restart),
		// so it can name another network's subnet. subnetID was resolved from
		// the prefix just now and is the one to reserve in.
		ip, _, err := r.Kea.GetLeaseIPv4ForMAC(ctx, mac)
		if err != nil && !errors.Is(err, keaservice.ErrNoLease) {
			errs = append(errs, fmt.Sprintf("%s: %v", mac, err))
			outcomes[mac] = lastKnownOutcome(previous[mac], "")
			continue
		}

		// A stale lease from another network must not be pinned here.
		if ip != "" && ipnet != nil {
			if p := net.ParseIP(ip); p == nil || p.To4() == nil || !ipnet.Contains(p) {
				log.Info("lease IP not within expected prefix, will create MAC-only reservation",
					"mac", mac, "leaseIP", ip, "expectedPrefix", ipv4Prefix)
				ip = ""
			}
		}

		res, err := r.Kea.EnsureReservationForMACIP(ctx, mac, subnetID, ip)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", mac, err))
			outcomes[mac] = lastKnownOutcome(previous[mac], ip)
			continue
		}
		if res.Warning != "" {
			warnings = append(warnings, fmt.Sprintf("%s: %s", mac, res.Warning))
			log.Info("DHCP reservation not pinned", "mac", mac, "leaseIP", ip, "subnetID", subnetID, "reason", res.Warning)
		}
		logReservationResult(log, mac, ip, subnetID, ipv4Prefix, res)
		outcomes[mac] = macOutcome{ip: ip, reserved: res.Pinned}
	}

	return outcomes, errs, warnings
}

func logReservationResult(log logr.Logger, mac, ip string, sid int, ipv4Prefix string, res keaservice.ReservationResult) {
	kv := []any{"mac", mac, "ip", ip, "subnetID", sid, "subnet", ipv4Prefix}
	switch {
	case res.Upgraded:
		log.Info("pinned DHCP reservation to the lease IP", kv...)
	case res.WouldPin:
		log.Info("would pin DHCP reservation to the lease IP (set "+consts.KEA_PIN_RESERVATIONS+"=enforce to apply)", kv...)
	case res.Created && ip != "":
		log.Info("configured DHCP reservation with IP", kv...)
	case res.Created:
		log.Info("created MAC-only reservation, IP will be auto-allocated on DHCP request", kv...)
	case ip != "":
		log.V(1).Info("DHCP reservation already exists", append(kv, "pinned", res.Pinned)...)
	default:
		log.V(1).Info("MAC-only reservation already exists", kv...)
	}
}

// lastKnownOutcome is the outcome for a MAC Kea failed on: the lease IP when
// known, else what status reported before.
func lastKnownOutcome(prev vitistackcrdsv1alpha1.NetworkConfigurationInterface, leaseIP string) macOutcome {
	prevIP := ""
	if len(prev.IPv4Addresses) > 0 {
		prevIP = prev.IPv4Addresses[0]
	}
	if leaseIP != "" {
		return macOutcome{ip: leaseIP, reserved: prev.DHCPReserved && prevIP == leaseIP}
	}
	return macOutcome{ip: prevIP, reserved: prev.DHCPReserved}
}

// previousStatusByMAC indexes the status interfaces an earlier reconcile wrote by normalized MAC.
func previousStatusByMAC(nc *vitistackcrdsv1alpha1.NetworkConfiguration) map[string]vitistackcrdsv1alpha1.NetworkConfigurationInterface {
	out := make(map[string]vitistackcrdsv1alpha1.NetworkConfigurationInterface, len(nc.Status.NetworkInterfaces))
	for _, iface := range nc.Status.NetworkInterfaces {
		out[normalizeMAC(iface.MacAddress)] = iface
	}
	return out
}

func normalizeMAC(mac string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(mac, "-", ":")))
}

// withWarnings appends reservation warnings to a status message.
func withWarnings(msg string, warnings []string) string {
	if len(warnings) == 0 {
		return msg
	}
	return msg + "; warnings: " + strings.Join(warnings, "; ")
}

// buildStatusInterfaces builds the status interface array with all available information
func (r *NetworkConfigurationReconciler) buildStatusInterfaces(nc *vitistackcrdsv1alpha1.NetworkConfiguration, outcomes map[string]macOutcome, ipv4Prefix string, subnetInfo *keaservice.SubnetInfo) []vitistackcrdsv1alpha1.NetworkConfigurationInterface {
	statusInterfaces := make([]vitistackcrdsv1alpha1.NetworkConfigurationInterface, 0, len(nc.Spec.NetworkInterfaces))
	previous := previousStatusByMAC(nc)

	for _, iface := range nc.Spec.NetworkInterfaces {
		normalizedMAC := normalizeMAC(iface.MacAddress)
		statusIface := vitistackcrdsv1alpha1.NetworkConfigurationInterface{
			Name:       iface.Name,
			MacAddress: iface.MacAddress,
			Vlan:       iface.Vlan,
			IPv4Subnet: ipv4Prefix,
		}

		if o, ok := outcomes[normalizedMAC]; ok {
			statusIface.DHCPReserved = o.reserved
			if o.ip != "" {
				statusIface.IPv4Addresses = []string{o.ip}
			}
		}

		// Add gateway and DNS from subnet info if available; if Kea couldn't
		// provide it this time, keep what status already reported.
		if subnetInfo != nil {
			if subnetInfo.Gateway != "" {
				statusIface.IPv4Gateway = subnetInfo.Gateway
			}
			if len(subnetInfo.DNS) > 0 {
				statusIface.DNS = subnetInfo.DNS
			}
		} else if prev, ok := previous[normalizedMAC]; ok {
			statusIface.IPv4Gateway = prev.IPv4Gateway
			statusIface.DNS = prev.DNS
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
	kea := keaservice.New(keaClient)
	kea.PinMode = pinModeFromEnv()
	return &NetworkConfigurationReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		KeaClient:      keaClient,
		Kea:            kea,
		CleanupTimeout: cleanupTimeoutFromEnv(),
	}
}

// pinModeFromEnv reads KEA_PIN_RESERVATIONS (off, log or enforce). Anything
// else falls back to log, the mode that never writes.
func pinModeFromEnv() keaservice.PinMode {
	raw := strings.ToLower(strings.TrimSpace(viper.GetString(consts.KEA_PIN_RESERVATIONS)))
	switch mode := keaservice.PinMode(raw); mode {
	case keaservice.PinModeOff, keaservice.PinModeLog, keaservice.PinModeEnforce:
		return mode
	}
	vlog.Warn("invalid " + consts.KEA_PIN_RESERVATIONS + " value '" + raw + "', using 'log'")
	return keaservice.PinModeLog
}

// cleanupTimeoutFromEnv reads KEA_CLEANUP_TIMEOUT as a Go duration (e.g. 15m).
func cleanupTimeoutFromEnv() time.Duration {
	raw := strings.TrimSpace(viper.GetString(consts.KEA_CLEANUP_TIMEOUT))
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if raw != "" {
		vlog.Warn("invalid " + consts.KEA_CLEANUP_TIMEOUT + " value '" + raw + "', using " + DefaultCleanupTimeout.String())
	}
	return DefaultCleanupTimeout
}

// SetupWithManager registers the controller with the manager using the typed
// NetworkConfiguration resource.
func (r *NetworkConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vitistackcrdsv1alpha1.NetworkConfiguration{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrentReconciles()}).
		Named("networkconfiguration").
		Complete(r)
}

// maxConcurrentReconciles returns the number of parallel reconciliations per
// controller, read from MAX_CONCURRENT_RECONCILES. Defaults to 5 when unset or
// invalid; never returns less than 1. The workqueue serializes by object key,
// so concurrency only applies across distinct objects.
func maxConcurrentReconciles() int {
	const defaultMaxConcurrent = 5
	v := strings.TrimSpace(os.Getenv(consts.MAX_CONCURRENT_RECONCILES))
	if v == "" {
		return defaultMaxConcurrent
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return defaultMaxConcurrent
	}
	return n
}

// getNetworkNamespace fetches the NetworkNamespace for the given Kubernetes
// namespace. When networkNamespaceName is specified, it fetches that specific
// NetworkNamespace by name. Otherwise, it falls back to listing all
// NetworkNamespaces in the namespace and picking the first one (legacy
// behaviour). The second return value reports whether the fallback list path
// was used, so callers can decide whether/when to warn — this lets the caller
// silently triage resources that belong to another operator without emitting
// spurious deprecation warnings.
func (r *NetworkConfigurationReconciler) getNetworkNamespace(ctx context.Context, namespace, networkNamespaceName string) (*vitistackcrdsv1alpha1.NetworkNamespace, bool, error) {
	var nn vitistackcrdsv1alpha1.NetworkNamespace

	if networkNamespaceName != "" {
		if err := r.Get(ctx, client.ObjectKey{Name: networkNamespaceName, Namespace: namespace}, &nn); err != nil {
			return nil, false, fmt.Errorf("failed to get NetworkNamespace %s in namespace %s: %w", networkNamespaceName, namespace, err)
		}
		return &nn, false, nil
	}

	nnList := &vitistackcrdsv1alpha1.NetworkNamespaceList{}
	if err := r.List(ctx, nnList, client.InNamespace(namespace)); err != nil {
		return nil, true, err
	}
	if len(nnList.Items) == 0 {
		return nil, true, fmt.Errorf("%w in namespace %s", errNoNetworkNamespace, namespace)
	}
	return &nnList.Items[0], true, nil
}

// isNetworkNamespaceMissing reports whether getNetworkNamespace failed because
// the NetworkNamespace doesn't exist, rather than because the API failed.
func isNetworkNamespaceMissing(err error) bool {
	return apierrors.IsNotFound(err) || errors.Is(err, errNoNetworkNamespace)
}

// networkNamespaceRetryDelay picks the retry delay after getNetworkNamespace failed.
func networkNamespaceRetryDelay(err error) time.Duration {
	if isNetworkNamespaceMissing(err) {
		return RequeueDelayNetworkNamespaceMissing
	}
	return RequeueDelayError
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

// cleanupReservations removes this NC's reservations from Kea. It returns nil
// when nothing is left to clean up (no MACs, no known subnet, subnet or
// reservation already gone) and an error only when Kea or the Kubernetes API
// couldn't be asked — the caller retries those.
func (r *NetworkConfigurationReconciler) cleanupReservations(ctx context.Context, nc *vitistackcrdsv1alpha1.NetworkConfiguration) error {
	log := logf.FromContext(ctx)
	macs := extractMACsFromTypedNetworkConfiguration(nc)
	if len(macs) == 0 {
		return nil
	}
	prefix, err := r.cleanupPrefix(ctx, nc)
	if err != nil {
		return err
	}
	if prefix == "" {
		log.Info("no subnet known for NetworkConfiguration, nothing to clean up in Kea", "name", nc.Name, "namespace", nc.Namespace)
		return nil
	}
	subnetID, err := r.Kea.GetSubnetID(ctx, prefix)
	if errors.Is(err, keaservice.ErrSubnetNotFound) {
		log.Info("subnet not found in Kea, nothing to clean up", "ipv4Prefix", prefix, "name", nc.Name, "namespace", nc.Namespace)
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolving Kea subnet for %s: %w", prefix, err)
	}
	var failed []string
	for _, mac := range macs {
		if err := r.Kea.DeleteReservationForMAC(ctx, mac, subnetID); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", mac, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("deleting reservations: %s", strings.Join(failed, "; "))
	}
	return nil
}

// cleanupPrefix returns the subnet this NC's reservations live in: the
// NetworkNamespace's prefix or, once the NetworkNamespace is gone, the subnet
// recorded in status. Returns "" when neither is known.
func (r *NetworkConfigurationReconciler) cleanupPrefix(ctx context.Context, nc *vitistackcrdsv1alpha1.NetworkConfiguration) (string, error) {
	nn, _, err := r.getNetworkNamespace(ctx, nc.GetNamespace(), nc.Spec.NetworkNamespaceName)
	if err != nil && !isNetworkNamespaceMissing(err) {
		return "", err
	}
	if err == nil && nn.Status.IPv4Prefix != "" {
		return nn.Status.IPv4Prefix, nil
	}
	for _, iface := range nc.Status.NetworkInterfaces {
		if iface.IPv4Subnet != "" {
			return iface.IPv4Subnet, nil
		}
	}
	return "", nil
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
