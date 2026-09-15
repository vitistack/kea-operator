package kea

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/vitistack/kea-operator/internal/services/kea/keafake"
)

const (
	testMAC     = "52:54:00:aa:bb:01"
	otherMAC    = "52:54:00:aa:bb:02"
	testSID     = 43
	testLeaseIP = "100.64.15.67"
)

func ensure(t *testing.T, kea *keafake.Server, mode PinMode, ip string) (ReservationResult, error) {
	t.Helper()
	svc := New(kea)
	svc.PinMode = mode
	return svc.EnsureReservationForMACIP(context.Background(), testMAC, testSID, ip)
}

func TestEnsureReservation_NoReservation_AddsWithLeaseIP(t *testing.T) {
	kea := &keafake.Server{}
	res, err := ensure(t, kea, PinModeOff, testLeaseIP)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Created || !res.Pinned {
		t.Fatalf("expected a new pinned reservation, got %+v", res)
	}
	if h := kea.HostFor(testMAC, testSID); h == nil || h[keaFieldIPAddress] != testLeaseIP {
		t.Fatalf("expected reservation %s -> %s, got %v", testMAC, testLeaseIP, h)
	}
}

func TestEnsureReservation_NoReservationNoLease_AddsMACOnly(t *testing.T) {
	kea := &keafake.Server{}
	res, err := ensure(t, kea, PinModeEnforce, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Created || res.Pinned {
		t.Fatalf("expected a new MAC-only reservation, got %+v", res)
	}
	h := kea.HostFor(testMAC, testSID)
	if h == nil {
		t.Fatal("expected a reservation to be created")
	}
	if _, hasIP := h[keaFieldIPAddress]; hasIP {
		t.Fatalf("expected no ip-address on MAC-only reservation, got %v", h)
	}
}

// The core bug: a MAC-only reservation made before the VM booted was never
// upgraded, so the VM's address stayed an ordinary dynamic lease.
func TestEnsureReservation_MACOnly_EnforcePinsLeaseIP(t *testing.T) {
	kea := &keafake.Server{Hosts: []map[string]any{keafake.Host(testMAC, testSID, "")}}
	res, err := ensure(t, kea, PinModeEnforce, testLeaseIP)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Upgraded || !res.Pinned {
		t.Fatalf("expected the reservation to be upgraded and pinned, got %+v", res)
	}
	if h := kea.HostFor(testMAC, testSID); h == nil || h[keaFieldIPAddress] != testLeaseIP {
		t.Fatalf("expected reservation %s -> %s, got %v", testMAC, testLeaseIP, h)
	}
	if got, want := kea.Writes(), []string{keafake.CmdReservationDel, keafake.CmdReservationAdd}; !slices.Equal(got, want) {
		t.Fatalf("writes = %v, want %v", got, want)
	}
}

func TestEnsureReservation_MACOnly_LogModeWritesNothing(t *testing.T) {
	kea := &keafake.Server{Hosts: []map[string]any{keafake.Host(testMAC, testSID, "")}}
	res, err := ensure(t, kea, PinModeLog, testLeaseIP)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.WouldPin || res.Pinned || res.Upgraded {
		t.Fatalf("expected would-pin only, got %+v", res)
	}
	if w := kea.Writes(); len(w) != 0 {
		t.Fatalf("log mode must not write, got %v", w)
	}
	if _, hasIP := kea.HostFor(testMAC, testSID)[keaFieldIPAddress]; hasIP {
		t.Fatal("log mode must leave the reservation MAC-only")
	}
}

// Off keeps today's behavior exactly: no writes and no extra lookups.
func TestEnsureReservation_MACOnly_OffModeIssuesNoNewCommands(t *testing.T) {
	kea := &keafake.Server{Hosts: []map[string]any{keafake.Host(testMAC, testSID, "")}}
	res, err := ensure(t, kea, PinModeOff, testLeaseIP)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Pinned || res.WouldPin {
		t.Fatalf("off mode must not pin, got %+v", res)
	}
	if w := kea.Writes(); len(w) != 0 {
		t.Fatalf("off mode must not write, got %v", w)
	}
	if slices.Contains(kea.Calls(), keafake.CmdReservationGet) {
		t.Fatalf("off mode must not look up IP ownership, calls: %v", kea.Calls())
	}
}

// The .67 case: the lease IP is already reserved for a different MAC. Pinning
// must not delete anything and must report whose reservation it is.
func TestEnsureReservation_MACOnly_IPReservedForOtherMAC_NotTouched(t *testing.T) {
	kea := &keafake.Server{Hosts: []map[string]any{
		keafake.Host(testMAC, testSID, ""),
		keafake.Host(otherMAC, testSID, testLeaseIP),
	}}
	res, err := ensure(t, kea, PinModeEnforce, testLeaseIP)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Pinned || res.Upgraded {
		t.Fatalf("expected no pin on conflict, got %+v", res)
	}
	if !strings.Contains(res.Warning, otherMAC) {
		t.Fatalf("expected warning naming %s, got %q", otherMAC, res.Warning)
	}
	if w := kea.Writes(); len(w) != 0 {
		t.Fatalf("conflict must not write, got %v", w)
	}
}

func TestEnsureReservation_ReservationWithSameIP_NoWrites(t *testing.T) {
	kea := &keafake.Server{Hosts: []map[string]any{keafake.Host(testMAC, testSID, testLeaseIP)}}
	res, err := ensure(t, kea, PinModeEnforce, testLeaseIP)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Pinned || res.Created || res.Upgraded || res.Warning != "" {
		t.Fatalf("expected an already-pinned reservation, got %+v", res)
	}
	if w := kea.Writes(); len(w) != 0 {
		t.Fatalf("expected no writes, got %v", w)
	}
}

// An existing IP reservation is never rewritten, even when the lease differs.
func TestEnsureReservation_ReservationWithDifferentIP_NotChanged(t *testing.T) {
	const reservedIP = "100.64.15.10"
	kea := &keafake.Server{Hosts: []map[string]any{keafake.Host(testMAC, testSID, reservedIP)}}
	res, err := ensure(t, kea, PinModeEnforce, testLeaseIP)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Pinned || res.Warning == "" {
		t.Fatalf("expected an unpinned result with a warning, got %+v", res)
	}
	if w := kea.Writes(); len(w) != 0 {
		t.Fatalf("expected no writes, got %v", w)
	}
	if got := kea.HostFor(testMAC, testSID)[keaFieldIPAddress]; got != reservedIP {
		t.Fatalf("reservation IP changed to %v", got)
	}
}

// A MAC-only reservation with its own settings (hand-made or from Puppet)
// is left alone: delete+add would drop those settings.
func TestEnsureReservation_MACOnlyWithHostname_NotTouched(t *testing.T) {
	custom := keafake.Host(testMAC, testSID, "")
	custom["hostname"] = "ctp1"
	kea := &keafake.Server{Hosts: []map[string]any{custom}}
	res, err := ensure(t, kea, PinModeEnforce, testLeaseIP)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Pinned || res.Warning == "" {
		t.Fatalf("expected an unpinned result with a warning, got %+v", res)
	}
	if w := kea.Writes(); len(w) != 0 {
		t.Fatalf("expected no writes, got %v", w)
	}
}

// If Kea refuses the pinned reservation after the MAC-only one was deleted,
// the MAC-only reservation is put back.
func TestEnsureReservation_PinRejected_RestoresMACOnly(t *testing.T) {
	kea := &keafake.Server{
		Hosts:       []map[string]any{keafake.Host(testMAC, testSID, "")},
		RejectIPAdd: "address is out of the subnet range",
	}
	_, err := ensure(t, kea, PinModeEnforce, testLeaseIP)
	if err == nil {
		t.Fatal("expected an error when Kea rejects the pinned reservation")
	}
	h := kea.HostFor(testMAC, testSID)
	if h == nil {
		t.Fatal("expected the MAC-only reservation to be restored")
	}
	if _, hasIP := h[keaFieldIPAddress]; hasIP {
		t.Fatalf("restored reservation must be MAC-only, got %v", h)
	}
}

// Before, a failed lookup read as "no reservation" and led to a blind add.
func TestEnsureReservation_LookupFails_NoWrites(t *testing.T) {
	kea := &keafake.Server{Down: map[string]bool{keafake.CmdReservationGetByID: true, keafake.CmdReservationGetAll: true}}
	if _, err := ensure(t, kea, PinModeEnforce, testLeaseIP); err == nil {
		t.Fatal("expected an error when reservations can't be read")
	}
	if w := kea.Writes(); len(w) != 0 {
		t.Fatalf("expected no writes after a failed lookup, got %v", w)
	}
}

func TestGetLeaseIPv4ForMAC_NoLeaseIsErrNoLease(t *testing.T) {
	svc := New(&keafake.Server{})
	_, _, err := svc.GetLeaseIPv4ForMAC(context.Background(), testMAC)
	if !errors.Is(err, ErrNoLease) {
		t.Fatalf("expected ErrNoLease, got %v", err)
	}
}

// "Kea unreachable" must be distinguishable from "VM not booted yet".
func TestGetLeaseIPv4ForMAC_KeaUnreachableIsNotErrNoLease(t *testing.T) {
	svc := New(&keafake.Server{Down: map[string]bool{keafake.CmdLeaseGetByHW: true, keafake.CmdReservationGetByID: true}})
	_, _, err := svc.GetLeaseIPv4ForMAC(context.Background(), testMAC)
	if err == nil || errors.Is(err, ErrNoLease) {
		t.Fatalf("expected a non-ErrNoLease error, got %v", err)
	}
}

func TestDeleteReservationForMAC_AlreadyGoneIsSuccess(t *testing.T) {
	svc := New(&keafake.Server{})
	if err := svc.DeleteReservationForMAC(context.Background(), testMAC, testSID); err != nil {
		t.Fatalf("expected deleting a missing reservation to succeed, got %v", err)
	}
}

func TestDeleteReservationForMAC_KeaUnreachableIsError(t *testing.T) {
	svc := New(&keafake.Server{Down: map[string]bool{keafake.CmdReservationDel: true}})
	if err := svc.DeleteReservationForMAC(context.Background(), testMAC, testSID); err == nil {
		t.Fatal("expected an error when Kea is unreachable")
	}
}
