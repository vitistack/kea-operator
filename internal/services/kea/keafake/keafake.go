// Package keafake is an in-memory Kea control API for tests. It keeps host
// reservations, leases and subnets, and answers the way Kea's host_cmds,
// lease_cmds and subnet_cmds hooks answer (result 3 = nothing found).
package keafake

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/vitistack/kea-operator/pkg/models/keamodels"
)

// Kea commands the fake understands.
const (
	CmdReservationAdd     = "reservation-add"
	CmdReservationDel     = "reservation-del"
	CmdReservationGet     = "reservation-get"
	CmdReservationGetByID = "reservation-get-by-id"
	CmdReservationGetAll  = "reservation-get-all"
	CmdLeaseGetByHW       = "lease4-get-by-hw-address"
	CmdSubnetList         = "subnet4-list"
	CmdSubnetGet          = "subnet4-get"
	CmdSubnetAdd          = "subnet4-add"
)

const (
	fieldSubnetID   = "subnet-id"
	fieldHWAddress  = "hw-address"
	fieldIPAddress  = "ip-address"
	fieldIdentifier = "identifier"
)

// Subnet is a Kea subnet with its router and DNS options.
type Subnet struct {
	ID      int
	CIDR    string
	Gateway string
	DNS     []string
}

// Server is the fake Kea. Set the exported fields before use; it is safe for
// concurrent Send calls.
type Server struct {
	mu sync.Mutex

	Hosts   []map[string]any
	Leases  []map[string]any
	Subnets []Subnet

	// RejectIPAdd, when set, makes reservation-add with an ip-address answer
	// result 1 with this text.
	RejectIPAdd string
	// Down lists commands answered with a transport error.
	Down map[string]bool

	calls []string
}

// Host returns a host reservation shaped like Kea's output (Host::toElement4):
// every default field is present, ip-address only when set.
func Host(mac string, subnetID int, ip string) map[string]any {
	h := map[string]any{
		"boot-file-name":  "",
		"client-classes":  []any{},
		"hostname":        "",
		fieldHWAddress:    mac,
		"next-server":     "0.0.0.0",
		"option-data":     []any{},
		"server-hostname": "",
		fieldSubnetID:     float64(subnetID),
	}
	if ip != "" {
		h[fieldIPAddress] = ip
	}
	return h
}

// Lease returns a lease shaped like lease4-get-by-hw-address output.
func Lease(mac, ip string, subnetID int) map[string]any {
	return map[string]any{
		"cltt":         float64(1000),
		"fqdn-fwd":     false,
		"fqdn-rev":     false,
		"hostname":     "",
		fieldHWAddress: mac,
		fieldIPAddress: ip,
		"state":        float64(0),
		fieldSubnetID:  float64(subnetID),
		"valid-lft":    float64(2592000),
	}
}

func argInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

func empty(text string) keamodels.Response {
	return keamodels.Response{Result: 3, Text: text}
}

func ok(text string, args map[string]any) keamodels.Response {
	return keamodels.Response{Result: 0, Text: text, Arguments: args}
}

// Send implements keainterface.KeaClient.
func (s *Server) Send(_ context.Context, cmd keamodels.Request) (keamodels.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, cmd.Command)
	if s.Down[cmd.Command] {
		return keamodels.Response{}, fmt.Errorf("all KEA servers failed: dial tcp: connection refused")
	}

	switch cmd.Command {
	case CmdReservationGetByID:
		mac, _ := cmd.Args[fieldIdentifier].(string)
		found := s.hostsWhere(func(h map[string]any) bool { return h[fieldHWAddress] == mac })
		if len(found) == 0 {
			return empty("0 IPv4 host(s) found."), nil
		}
		return ok(fmt.Sprintf("%d IPv4 host(s) found.", len(found)), map[string]any{"hosts": found}), nil

	case CmdReservationGetAll:
		sid := argInt(cmd.Args[fieldSubnetID])
		found := s.hostsWhere(func(h map[string]any) bool { return argInt(h[fieldSubnetID]) == sid })
		if len(found) == 0 {
			return empty("0 IPv4 host(s) found."), nil
		}
		return ok(fmt.Sprintf("%d IPv4 host(s) found.", len(found)), map[string]any{"hosts": found}), nil

	case CmdReservationGet:
		sid := argInt(cmd.Args[fieldSubnetID])
		ip, _ := cmd.Args[fieldIPAddress].(string)
		for _, h := range s.Hosts {
			if argInt(h[fieldSubnetID]) == sid && h[fieldIPAddress] == ip {
				return ok("Host found.", maps.Clone(h)), nil
			}
		}
		return empty("Host not found."), nil

	case CmdReservationAdd:
		return s.addReservation(cmd), nil

	case CmdReservationDel:
		mac, _ := cmd.Args[fieldIdentifier].(string)
		sid := argInt(cmd.Args[fieldSubnetID])
		for i, h := range s.Hosts {
			if argInt(h[fieldSubnetID]) == sid && h[fieldHWAddress] == mac {
				s.Hosts = slices.Delete(s.Hosts, i, i+1)
				return ok("Host deleted.", nil), nil
			}
		}
		return empty("Host not deleted (not found)."), nil

	case CmdLeaseGetByHW:
		mac, _ := cmd.Args[fieldHWAddress].(string)
		var found []any
		for _, l := range s.Leases {
			if l[fieldHWAddress] == mac {
				found = append(found, maps.Clone(l))
			}
		}
		if len(found) == 0 {
			return empty("0 IPv4 lease(s) found."), nil
		}
		return ok(fmt.Sprintf("%d IPv4 lease(s) found.", len(found)), map[string]any{"leases": found}), nil

	case CmdSubnetList:
		if len(s.Subnets) == 0 {
			return empty("0 IPv4 subnets found"), nil
		}
		subs := make([]any, 0, len(s.Subnets))
		for _, sn := range s.Subnets {
			subs = append(subs, map[string]any{"id": float64(sn.ID), "subnet": sn.CIDR})
		}
		return ok(fmt.Sprintf("%d IPv4 subnets found", len(subs)), map[string]any{"subnets": subs}), nil

	case CmdSubnetGet:
		id := argInt(cmd.Args["id"])
		for _, sn := range s.Subnets {
			if sn.ID == id {
				return ok("Info about IPv4 subnet found", map[string]any{"subnet4": []any{subnetJSON(sn)}}), nil
			}
		}
		return empty(fmt.Sprintf("No subnet with id %d found", id)), nil

	case CmdSubnetAdd:
		arr, _ := cmd.Args["subnet4"].([]map[string]any)
		if len(arr) == 0 {
			return keamodels.Response{Result: 1, Text: "missing subnet4"}, nil
		}
		cidr, _ := arr[0]["subnet"].(string)
		s.Subnets = append(s.Subnets, Subnet{ID: argInt(arr[0]["id"]), CIDR: cidr})
		return ok("IPv4 subnet added", nil), nil
	}
	return keamodels.Response{Result: 2, Text: "'" + cmd.Command + "' command not supported."}, nil
}

func (s *Server) addReservation(cmd keamodels.Request) keamodels.Response {
	r, _ := cmd.Args["reservation"].(map[string]any)
	mac, _ := r[fieldHWAddress].(string)
	sid := argInt(r[fieldSubnetID])
	ip, _ := r[fieldIPAddress].(string)
	if ip != "" && s.RejectIPAdd != "" {
		return keamodels.Response{Result: 1, Text: s.RejectIPAdd}
	}
	for _, h := range s.Hosts {
		if argInt(h[fieldSubnetID]) != sid {
			continue
		}
		if h[fieldHWAddress] == mac {
			return keamodels.Response{Result: 1, Text: "Host already exists."}
		}
		if ip != "" && h[fieldIPAddress] == ip {
			return keamodels.Response{Result: 1, Text: "Host with address " + ip + " already exists."}
		}
	}
	s.Hosts = append(s.Hosts, Host(mac, sid, ip))
	return ok("Host added.", nil)
}

func subnetJSON(sn Subnet) map[string]any {
	var opts []any
	if sn.Gateway != "" {
		opts = append(opts, map[string]any{"code": float64(3), "name": "routers", "data": sn.Gateway})
	}
	if len(sn.DNS) > 0 {
		opts = append(opts, map[string]any{"code": float64(6), "name": "domain-name-servers", "data": strings.Join(sn.DNS, ", ")})
	}
	return map[string]any{"id": float64(sn.ID), "subnet": sn.CIDR, "option-data": opts}
}

func (s *Server) hostsWhere(match func(map[string]any) bool) []any {
	var out []any
	for _, h := range s.Hosts {
		if match(h) {
			out = append(out, maps.Clone(h))
		}
	}
	return out
}

// Calls returns every command received, in order.
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// Writes returns the reservation-changing commands received, in order.
func (s *Server) Writes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.calls {
		if c == CmdReservationAdd || c == CmdReservationDel {
			out = append(out, c)
		}
	}
	return out
}

// HostFor returns the stored reservation for mac in subnetID, or nil.
func (s *Server) HostFor(mac string, subnetID int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.Hosts {
		if h[fieldHWAddress] == mac && argInt(h[fieldSubnetID]) == subnetID {
			return maps.Clone(h)
		}
	}
	return nil
}
