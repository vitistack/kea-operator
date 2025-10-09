package kea

import (
	"context"
	"fmt"
	"strings"

	"github.com/vitistack/kea-operator/pkg/interfaces/keainterface"
	"github.com/vitistack/kea-operator/pkg/models/keamodels"
)

// Service wraps Kea operations used by the controller.
type Service struct {
	Client keainterface.KeaClient
}

func New(client keainterface.KeaClient) *Service {
	return &Service{Client: client}
}

// GetSubnetID lists Kea subnets and returns the id of the subnet matching the given IPv4 CIDR prefix.
func (s *Service) GetSubnetID(ctx context.Context, ipv4Prefix string) (int, error) {
	req := keamodels.Request{Command: "subnet4-list", Args: map[string]any{}}
	resp, err := s.Client.Send(ctx, req)
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
	for _, snet := range subnets {
		m, ok := snet.(map[string]any)
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

// DeleteReservationForMAC removes a reservation for the given MAC and subnet.
func (s *Service) DeleteReservationForMAC(ctx context.Context, mac string, subnetID int) error {
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
	resp, err := s.Client.Send(ctx, delReq)
	if err != nil {
		return err
	}
	if resp.Result != 0 {
		return fmt.Errorf("kea reservation-del failed: %s", resp.Text)
	}
	return nil
}

// EnsureReservationForMACIP ensures a reservation exists for mac in the given subnet, with optional ip.
func (s *Service) EnsureReservationForMACIP(ctx context.Context, mac string, subnetID int, ipv4 string) error {
	mac = strings.ToLower(strings.TrimSpace(mac))
	if mac == "" {
		return fmt.Errorf("missing mac")
	}
	if s.macReservationExists(ctx, mac, subnetID) {
		return nil
	}
	reservation := map[string]any{
		"subnet-id":  subnetID,
		"hw-address": mac,
	}
	if ip := strings.TrimSpace(ipv4); ip != "" {
		reservation["ip-address"] = ip
	}
	addReq := keamodels.Request{
		Command: "reservation-add",
		Args: map[string]any{
			"reservation":      reservation,
			"operation-target": "all",
		},
	}
	addResp, addErr := s.Client.Send(ctx, addReq)
	if addErr != nil {
		return addErr
	}
	if addResp.Result != 0 {
		return fmt.Errorf("kea reservation-add failed: %s", addResp.Text)
	}
	return nil
}

// macReservationExists checks whether a reservation already exists for the given MAC + subnet.
func (s *Service) macReservationExists(ctx context.Context, mac string, subnetID int) bool {
	mac = strings.ToLower(strings.TrimSpace(mac))
	if mac == "" {
		return false
	}

	// 1. Primary: reservation-get-by-id (identifier-type + identifier) => hosts list
	primary := keamodels.Request{
		Command: "reservation-get-by-id",
		Args: map[string]any{
			"identifier-type": "hw-address",
			"identifier":      mac,
		},
	}
	if resp, err := s.Client.Send(ctx, primary); err == nil {
		if resp.Result == 0 { // success path returns hosts array
			if hosts, ok := resp.Arguments["hosts"].([]any); ok {
				for _, h := range hosts {
					hm, ok := h.(map[string]any)
					if !ok {
						continue
					}
					if hw, ok2 := hm["hw-address"].(string); ok2 && strings.EqualFold(hw, mac) {
						if sid, ok3 := hm["subnet-id"]; ok3 {
							switch v := sid.(type) {
							case float64:
								if int(v) != subnetID {
									continue
								}
							case int:
								if v != subnetID {
									continue
								}
							}
						}
						return true
					}
				}
			}
			return false
		}
		txt := strings.ToLower(resp.Text)
		if strings.Contains(txt, "not found") || strings.Contains(txt, "no host") || strings.Contains(txt, "0 ipv4 host") {
			return false
		}
	}

	// 2. Fallback: reservation-get-all (scan hosts list for match)
	fallback := keamodels.Request{Command: "reservation-get-all", Args: map[string]any{"subnet-id": subnetID}}
	resp2, err2 := s.Client.Send(ctx, fallback)
	if err2 != nil || resp2.Result != 0 {
		return false
	}
	if hosts, ok := resp2.Arguments["hosts"].([]any); ok {
		for _, h := range hosts {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if hw, ok2 := hm["hw-address"].(string); ok2 && strings.EqualFold(hw, mac) {
				return true
			}
		}
	}
	return false
}

// GetLeaseIPv4ForMAC tries to resolve an IPv4 lease for the given MAC.
// Returns ip, subnet-id (if available), error
func (s *Service) GetLeaseIPv4ForMAC(ctx context.Context, mac string) (string, int, error) {
	mac = strings.ToLower(strings.TrimSpace(mac))
	if mac == "" {
		return "", 0, fmt.Errorf("missing mac")
	}
	primary := keamodels.Request{
		Command: "lease4-get-by-hw-address",
		Args:    map[string]any{"hw-address": mac},
	}
	if resp, err := s.Client.Send(ctx, primary); err == nil {
		if resp.Result == 0 {
			// Kea can return leases as an array; pick the newest (largest cltt) that matches the MAC.
			if arr, ok := resp.Arguments["leases"].([]any); ok {
				bestIP := ""
				bestSID := 0
				var bestCLTT float64
				for _, elem := range arr {
					m, ok := elem.(map[string]any)
					if !ok {
						continue
					}
					hw, _ := m["hw-address"].(string)
					if !strings.EqualFold(strings.TrimSpace(hw), mac) {
						// Be defensive in case server returns extra entries
						continue
					}
					ip, _ := m["ip-address"].(string)
					if ip == "" {
						continue
					}
					// Prefer the highest cltt (most recent)
					cltt := 0.0
					switch v := m["cltt"].(type) {
					case float64:
						cltt = v
					case int:
						cltt = float64(v)
					}
					sid := 0
					switch v := m["subnet-id"].(type) {
					case float64:
						sid = int(v)
					case int:
						sid = v
					}
					if bestIP == "" || cltt > bestCLTT {
						bestIP = ip
						bestSID = sid
						bestCLTT = cltt
					}
				}
				if bestIP != "" {
					return bestIP, bestSID, nil
				}
			} else if l, ok := resp.Arguments["leases"].(map[string]any); ok {
				// Some deployments might return a single lease object; keep legacy support.
				ip := ""
				if v, ok2 := l["ip-address"].(string); ok2 {
					ip = v
				}
				sid := 0
				if v, ok2 := l["subnet-id"].(float64); ok2 {
					sid = int(v)
				} else if v2, ok3 := l["subnet-id"].(int); ok3 {
					sid = v2
				}
				if ip != "" {
					return ip, sid, nil
				}
			}
		}
	}

	// Fallback: reservation-get-by-id for any stored address
	fb := keamodels.Request{
		Command: "reservation-get-by-id",
		Args: map[string]any{
			"identifier-type": "hw-address",
			"identifier":      mac,
		},
	}
	if resp, err := s.Client.Send(ctx, fb); err == nil && resp.Result == 0 {
		if hosts, ok := resp.Arguments["hosts"].([]any); ok {
			for _, h := range hosts {
				hm, ok := h.(map[string]any)
				if !ok {
					continue
				}
				if v, ok2 := hm["ip-address"].(string); ok2 && v != "" {
					sid := 0
					if sv, ok3 := hm["subnet-id"].(float64); ok3 {
						sid = int(sv)
					} else if sv2, ok4 := hm["subnet-id"].(int); ok4 {
						sid = sv2
					}
					return v, sid, nil
				}
			}
		}
	}
	// Not finding a lease is not necessarily an error - the machine might not have booted yet
	// or the lease may have expired. Return empty values to let caller decide how to handle.
	return "", 0, fmt.Errorf("no lease found for MAC %s", mac)
}
