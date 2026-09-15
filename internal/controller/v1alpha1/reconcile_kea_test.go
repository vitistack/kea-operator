package v1alpha1

import (
	"context"
	"strings"
	"testing"
	"time"

	vitistackcrdsv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	keaservice "github.com/vitistack/kea-operator/internal/services/kea"
	"github.com/vitistack/kea-operator/internal/services/kea/keafake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// These tests drive Reconcile against a fake Kubernetes API and an in-memory
// Kea (keafake), and assert on the NetworkConfiguration status other operators
// read (kubevirt-operator copies its IPs into Machine public IPs) and on the
// reservations left in Kea.

const (
	rtNamespace = "tenant-a"
	rtNNName    = "nn-a"
	rtNCName    = "ctp1"
	rtIface     = "eth0"
	rtPrefix    = "100.64.15.0/24"
	rtGateway   = "100.64.15.1"
	rtDNS       = "100.64.0.53"
	rtSubnetID  = 43
	rtMAC       = "52:54:00:aa:bb:01"
	rtOtherMAC  = "52:54:00:aa:bb:02"
	rtLeaseIP   = "100.64.15.67"
	rtOldIP     = "100.64.15.50"
)

func rtNetworkNamespace() *vitistackcrdsv1alpha1.NetworkNamespace {
	return &vitistackcrdsv1alpha1.NetworkNamespace{
		ObjectMeta: metav1.ObjectMeta{Name: rtNNName, Namespace: rtNamespace},
		Spec: vitistackcrdsv1alpha1.NetworkNamespaceSpec{
			IPAllocation: &vitistackcrdsv1alpha1.NetworkNamespaceIPAllocation{Type: vitistackcrdsv1alpha1.IPAllocationTypeDHCP},
		},
		Status: vitistackcrdsv1alpha1.NetworkNamespaceStatus{IPv4Prefix: rtPrefix},
	}
}

// rtNetworkConfiguration returns an NC this operator has already claimed.
func rtNetworkConfiguration() *vitistackcrdsv1alpha1.NetworkConfiguration {
	return &vitistackcrdsv1alpha1.NetworkConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: rtNCName, Namespace: rtNamespace, Finalizers: []string{finalizerName}},
		Spec: vitistackcrdsv1alpha1.NetworkConfigurationSpec{
			NetworkNamespaceName: rtNNName,
			Provider:             vitistackcrdsv1alpha1.ProviderNameKea,
			NetworkInterfaces:    []vitistackcrdsv1alpha1.NetworkConfigurationInterface{{Name: rtIface, MacAddress: rtMAC}},
		},
	}
}

// withPreviousStatus sets the status an earlier reconcile would have written.
func withPreviousStatus(nc *vitistackcrdsv1alpha1.NetworkConfiguration) *vitistackcrdsv1alpha1.NetworkConfiguration {
	nc.Status.NetworkInterfaces = []vitistackcrdsv1alpha1.NetworkConfigurationInterface{{
		Name:          rtIface,
		MacAddress:    rtMAC,
		IPv4Addresses: []string{rtOldIP},
		IPv4Subnet:    rtPrefix,
		IPv4Gateway:   rtGateway,
		DNS:           []string{rtDNS},
		DHCPReserved:  true,
	}}
	return nc
}

func deleting(nc *vitistackcrdsv1alpha1.NetworkConfiguration, age time.Duration) *vitistackcrdsv1alpha1.NetworkConfiguration {
	ts := metav1.NewTime(time.Now().Add(-age))
	nc.DeletionTimestamp = &ts
	return nc
}

func rtKea() *keafake.Server {
	return &keafake.Server{Subnets: []keafake.Subnet{{ID: rtSubnetID, CIDR: rtPrefix, Gateway: rtGateway, DNS: []string{rtDNS}}}}
}

type rtEnv struct {
	r   *NetworkConfigurationReconciler
	c   client.Client
	kea *keafake.Server
}

func newRTEnv(t *testing.T, kea *keafake.Server, mode keaservice.PinMode, objs ...client.Object) rtEnv {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(vitistackcrdsv1alpha1.AddToScheme(s))
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&vitistackcrdsv1alpha1.NetworkConfiguration{}).
		Build()
	svc := keaservice.New(kea)
	svc.PinMode = mode
	return rtEnv{r: &NetworkConfigurationReconciler{Client: c, Scheme: s, KeaClient: kea, Kea: svc}, c: c, kea: kea}
}

func (e rtEnv) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	res, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: rtNamespace, Name: rtNCName}})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	return res
}

func (e rtEnv) getNC(t *testing.T) *vitistackcrdsv1alpha1.NetworkConfiguration {
	t.Helper()
	nc := &vitistackcrdsv1alpha1.NetworkConfiguration{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: rtNamespace, Name: rtNCName}, nc); err != nil {
		t.Fatalf("get NetworkConfiguration: %v", err)
	}
	return nc
}

func (e rtEnv) ncGone(t *testing.T) bool {
	t.Helper()
	err := e.c.Get(context.Background(), types.NamespacedName{Namespace: rtNamespace, Name: rtNCName}, &vitistackcrdsv1alpha1.NetworkConfiguration{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get NetworkConfiguration: %v", err)
	}
	return apierrors.IsNotFound(err)
}

func statusIface(t *testing.T, nc *vitistackcrdsv1alpha1.NetworkConfiguration) vitistackcrdsv1alpha1.NetworkConfigurationInterface {
	t.Helper()
	for _, iface := range nc.Status.NetworkInterfaces {
		if iface.MacAddress == rtMAC {
			return iface
		}
	}
	t.Fatalf("no status interface for %s in %+v", rtMAC, nc.Status.NetworkInterfaces)
	return vitistackcrdsv1alpha1.NetworkConfigurationInterface{}
}

func ipsOf(iface vitistackcrdsv1alpha1.NetworkConfigurationInterface) string {
	return strings.Join(iface.IPv4Addresses, ",")
}

// --- status must never lose an IP because Kea had a problem ---

func TestReconcile_KeaUnreachable_KeepsLastKnownIP(t *testing.T) {
	kea := rtKea()
	kea.Down = map[string]bool{keafake.CmdLeaseGetByHW: true}
	e := newRTEnv(t, kea, keaservice.PinModeLog, rtNetworkNamespace(), withPreviousStatus(rtNetworkConfiguration()))

	res := e.reconcile(t)

	if got := ipsOf(statusIface(t, e.getNC(t))); got != rtOldIP {
		t.Fatalf("status IPs = %q, want last known %q", got, rtOldIP)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected a retry after a Kea error")
	}
}

func TestReconcile_ReservationFails_KeepsLeaseIP(t *testing.T) {
	kea := rtKea()
	kea.Leases = []map[string]any{keafake.Lease(rtMAC, rtLeaseIP, rtSubnetID)}
	kea.Down = map[string]bool{keafake.CmdReservationGetByID: true, keafake.CmdReservationGetAll: true}
	e := newRTEnv(t, kea, keaservice.PinModeLog, rtNetworkNamespace(), withPreviousStatus(rtNetworkConfiguration()))

	e.reconcile(t)

	nc := e.getNC(t)
	if got := ipsOf(statusIface(t, nc)); got != rtLeaseIP {
		t.Fatalf("status IPs = %q, want the lease IP %q", got, rtLeaseIP)
	}
	if nc.Status.Phase != "Error" {
		t.Fatalf("phase = %q, want Error", nc.Status.Phase)
	}
}

func TestReconcile_SubnetDetailsUnavailable_KeepsGatewayAndDNS(t *testing.T) {
	kea := rtKea()
	kea.Leases = []map[string]any{keafake.Lease(rtMAC, rtLeaseIP, rtSubnetID)}
	kea.Down = map[string]bool{keafake.CmdSubnetGet: true}
	e := newRTEnv(t, kea, keaservice.PinModeLog, rtNetworkNamespace(), withPreviousStatus(rtNetworkConfiguration()))

	e.reconcile(t)

	iface := statusIface(t, e.getNC(t))
	if iface.IPv4Gateway != rtGateway || strings.Join(iface.DNS, ",") != rtDNS {
		t.Fatalf("gateway/DNS = %q/%v, want %q/[%s]", iface.IPv4Gateway, iface.DNS, rtGateway, rtDNS)
	}
	if got := ipsOf(iface); got != rtLeaseIP {
		t.Fatalf("status IPs = %q, want the current lease IP %q", got, rtLeaseIP)
	}
}

// --- reservations and what status reports about them ---

// A stale lease from another network must not decide the reservation's subnet.
func TestReconcile_LeaseInOtherSubnet_ReservesInResolvedSubnet(t *testing.T) {
	const staleSubnetID = 99
	kea := rtKea()
	kea.Leases = []map[string]any{keafake.Lease(rtMAC, "10.9.9.9", staleSubnetID)}
	e := newRTEnv(t, kea, keaservice.PinModeLog, rtNetworkNamespace(), rtNetworkConfiguration())

	e.reconcile(t)

	if kea.HostFor(rtMAC, rtSubnetID) == nil {
		t.Fatalf("expected a reservation in subnet %d", rtSubnetID)
	}
	if kea.HostFor(rtMAC, staleSubnetID) != nil {
		t.Fatalf("reservation created in the stale lease's subnet %d", staleSubnetID)
	}
}

// Kea keeps a lease's subnet-id when its subnets are renumbered, so a lease
// whose IP is inside the prefix can name another network's subnet. The prefix
// decides the subnet; the lease's subnet-id must not.
func TestReconcile_LeaseWithStaleSubnetID_ReservesInResolvedSubnet(t *testing.T) {
	const renumberedSubnetID = 28
	kea := rtKea()
	kea.Subnets = append(kea.Subnets, keafake.Subnet{ID: renumberedSubnetID, CIDR: "100.64.59.0/24"})
	kea.Leases = []map[string]any{keafake.Lease(rtMAC, rtLeaseIP, renumberedSubnetID)}
	e := newRTEnv(t, kea, keaservice.PinModeLog, rtNetworkNamespace(), rtNetworkConfiguration())

	e.reconcile(t)

	nc := e.getNC(t)
	if nc.Status.Phase != "Ready" {
		t.Fatalf("phase = %q (%s), want Ready", nc.Status.Phase, nc.Status.Message)
	}
	if h := kea.HostFor(rtMAC, rtSubnetID); h == nil || h["ip-address"] != rtLeaseIP {
		t.Fatalf("reservation in subnet %d = %v, want one holding %s", rtSubnetID, h, rtLeaseIP)
	}
	if kea.HostFor(rtMAC, renumberedSubnetID) != nil {
		t.Fatalf("reservation created in the lease's stale subnet %d", renumberedSubnetID)
	}
}

// DHCPReserved used to be true for MAC-only reservations, which pin nothing.
func TestReconcile_MACOnlyReservation_NotReportedReserved(t *testing.T) {
	kea := rtKea()
	kea.Hosts = []map[string]any{keafake.Host(rtMAC, rtSubnetID, "")}
	kea.Leases = []map[string]any{keafake.Lease(rtMAC, rtLeaseIP, rtSubnetID)}
	e := newRTEnv(t, kea, keaservice.PinModeLog, rtNetworkNamespace(), rtNetworkConfiguration())

	e.reconcile(t)

	iface := statusIface(t, e.getNC(t))
	if iface.DHCPReserved {
		t.Fatal("DHCPReserved must be false while the reservation holds no IP")
	}
	if got := ipsOf(iface); got != rtLeaseIP {
		t.Fatalf("status IPs = %q, want the lease IP %q", got, rtLeaseIP)
	}
}

func TestReconcile_EnforcePinsReservation_ReportsReserved(t *testing.T) {
	kea := rtKea()
	kea.Hosts = []map[string]any{keafake.Host(rtMAC, rtSubnetID, "")}
	kea.Leases = []map[string]any{keafake.Lease(rtMAC, rtLeaseIP, rtSubnetID)}
	e := newRTEnv(t, kea, keaservice.PinModeEnforce, rtNetworkNamespace(), rtNetworkConfiguration())

	e.reconcile(t)

	if h := kea.HostFor(rtMAC, rtSubnetID); h == nil || h["ip-address"] != rtLeaseIP {
		t.Fatalf("expected reservation pinned to %s, got %v", rtLeaseIP, h)
	}
	if iface := statusIface(t, e.getNC(t)); !iface.DHCPReserved || ipsOf(iface) != rtLeaseIP {
		t.Fatalf("expected reserved %s in status, got %+v", rtLeaseIP, iface)
	}
}

// A conflict is surfaced in the status message but is not a reconcile error.
func TestReconcile_PinConflict_ReportedInMessage(t *testing.T) {
	kea := rtKea()
	kea.Hosts = []map[string]any{
		keafake.Host(rtMAC, rtSubnetID, ""),
		keafake.Host(rtOtherMAC, rtSubnetID, rtLeaseIP),
	}
	kea.Leases = []map[string]any{keafake.Lease(rtMAC, rtLeaseIP, rtSubnetID)}
	e := newRTEnv(t, kea, keaservice.PinModeEnforce, rtNetworkNamespace(), rtNetworkConfiguration())

	e.reconcile(t)

	nc := e.getNC(t)
	if !strings.Contains(nc.Status.Message, rtOtherMAC) {
		t.Fatalf("status message %q should name %s", nc.Status.Message, rtOtherMAC)
	}
	if nc.Status.Phase != "Ready" {
		t.Fatalf("phase = %q, want Ready", nc.Status.Phase)
	}
	if got := ipsOf(statusIface(t, nc)); got != rtLeaseIP {
		t.Fatalf("status IPs = %q, want %q", got, rtLeaseIP)
	}
}

// Before, a missing NetworkNamespace ended the reconcile with no retry, and
// nothing re-triggered it (the controller doesn't watch NetworkNamespaces).
func TestReconcile_NetworkNamespaceMissing_Requeues(t *testing.T) {
	e := newRTEnv(t, rtKea(), keaservice.PinModeLog, rtNetworkConfiguration())
	if res := e.reconcile(t); res.RequeueAfter == 0 {
		t.Fatal("expected a retry while the NetworkNamespace is missing")
	}
}

// --- deletion ---

func TestReconcile_Deletion_RemovesReservationThenFinalizer(t *testing.T) {
	kea := rtKea()
	kea.Hosts = []map[string]any{keafake.Host(rtMAC, rtSubnetID, rtLeaseIP)}
	e := newRTEnv(t, kea, keaservice.PinModeLog, rtNetworkNamespace(), deleting(rtNetworkConfiguration(), time.Minute))

	e.reconcile(t)

	if kea.HostFor(rtMAC, rtSubnetID) != nil {
		t.Fatal("expected the reservation to be deleted")
	}
	if !e.ncGone(t) {
		t.Fatal("expected the finalizer to be removed")
	}
}

// Before, the finalizer was removed even when cleanup failed, leaking the
// reservation — and with pinning, its IP — forever.
func TestReconcile_Deletion_KeaUnreachable_KeepsFinalizerAndRetries(t *testing.T) {
	kea := rtKea()
	kea.Hosts = []map[string]any{keafake.Host(rtMAC, rtSubnetID, rtLeaseIP)}
	kea.Down = map[string]bool{keafake.CmdSubnetList: true}
	e := newRTEnv(t, kea, keaservice.PinModeLog, rtNetworkNamespace(), deleting(rtNetworkConfiguration(), time.Minute))

	res := e.reconcile(t)

	if e.ncGone(t) {
		t.Fatal("finalizer removed although cleanup failed and the timeout hasn't passed")
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected a retry")
	}
}

func TestReconcile_Deletion_GivesUpAfterTimeout(t *testing.T) {
	kea := rtKea()
	kea.Down = map[string]bool{keafake.CmdSubnetList: true}
	e := newRTEnv(t, kea, keaservice.PinModeLog, rtNetworkNamespace(), deleting(rtNetworkConfiguration(), 20*time.Minute))
	e.r.CleanupTimeout = 15 * time.Minute

	e.reconcile(t)

	if !e.ncGone(t) {
		t.Fatal("expected the finalizer to be removed once the cleanup timeout passed")
	}
}

func TestReconcile_Deletion_SkipAnnotation_NoKeaCalls(t *testing.T) {
	kea := rtKea()
	kea.Down = map[string]bool{keafake.CmdSubnetList: true, keafake.CmdReservationDel: true}
	nc := deleting(rtNetworkConfiguration(), time.Minute)
	nc.Annotations = map[string]string{skipCleanupAnnotation: "true"}
	e := newRTEnv(t, kea, keaservice.PinModeLog, rtNetworkNamespace(), nc)

	e.reconcile(t)

	if !e.ncGone(t) {
		t.Fatal("expected the finalizer to be removed")
	}
	if calls := kea.Calls(); len(calls) != 0 {
		t.Fatalf("expected no Kea calls, got %v", calls)
	}
}

// Subnet gone from Kea (e.g. wiped by a restart): nothing to clean up.
func TestReconcile_Deletion_SubnetGone_RemovesFinalizer(t *testing.T) {
	kea := &keafake.Server{Subnets: []keafake.Subnet{{ID: 1, CIDR: "10.0.0.0/24"}}}
	e := newRTEnv(t, kea, keaservice.PinModeLog, rtNetworkNamespace(), deleting(rtNetworkConfiguration(), time.Minute))

	e.reconcile(t)

	if !e.ncGone(t) {
		t.Fatal("expected the finalizer to be removed")
	}
}

// Before, provider triage ran first, so an NC we had claimed but whose
// provider later changed could never lose our finalizer.
func TestReconcile_Deletion_OtherProviderWithOurFinalizer_CleansUp(t *testing.T) {
	kea := rtKea()
	kea.Hosts = []map[string]any{keafake.Host(rtMAC, rtSubnetID, rtLeaseIP)}
	nc := deleting(rtNetworkConfiguration(), time.Minute)
	nc.Spec.Provider = vitistackcrdsv1alpha1.ProviderNameStaticIP
	e := newRTEnv(t, kea, keaservice.PinModeLog, rtNetworkNamespace(), nc)

	e.reconcile(t)

	if kea.HostFor(rtMAC, rtSubnetID) != nil {
		t.Fatal("expected the reservation to be deleted")
	}
	if !e.ncGone(t) {
		t.Fatal("expected the finalizer to be removed")
	}
}

func TestReconcile_Deletion_NotOurFinalizer_NoKeaCalls(t *testing.T) {
	kea := rtKea()
	nc := deleting(rtNetworkConfiguration(), time.Minute)
	nc.Spec.Provider = vitistackcrdsv1alpha1.ProviderNameStaticIP
	nc.Finalizers = []string{"static-ip.vitistack.io/finalizer"}
	e := newRTEnv(t, kea, keaservice.PinModeLog, rtNetworkNamespace(), nc)

	e.reconcile(t)

	if calls := kea.Calls(); len(calls) != 0 {
		t.Fatalf("expected no Kea calls for an NC we never claimed, got %v", calls)
	}
	if e.ncGone(t) {
		t.Fatal("another operator's finalizer must not be touched")
	}
}

// NetworkNamespace already deleted: the subnet recorded in status is enough.
func TestReconcile_Deletion_NetworkNamespaceGone_UsesStatusSubnet(t *testing.T) {
	kea := rtKea()
	kea.Hosts = []map[string]any{keafake.Host(rtMAC, rtSubnetID, rtOldIP)}
	e := newRTEnv(t, kea, keaservice.PinModeLog, deleting(withPreviousStatus(rtNetworkConfiguration()), time.Minute))

	e.reconcile(t)

	if kea.HostFor(rtMAC, rtSubnetID) != nil {
		t.Fatal("expected the reservation to be deleted using the status subnet")
	}
	if !e.ncGone(t) {
		t.Fatal("expected the finalizer to be removed")
	}
}
