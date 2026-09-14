package kea

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/vitistack/kea-operator/pkg/interfaces/keainterface"
	"github.com/vitistack/kea-operator/pkg/models/keamodels"
)

// Kea Control Agent JSON field names. Kept as constants so a typo can't drift
// between the request builder and the response parser, and so goconst doesn't
// flag the repeated literals. keaFieldHWAddress doubles as the value passed
// for identifier-type — it's the same string in both contexts.
const (
	keaFieldSubnetID       = "subnet-id"
	keaFieldHWAddress      = "hw-address"
	keaFieldIPAddress      = "ip-address"
	keaFieldIdentifier     = "identifier"
	keaFieldIdentifierType = "identifier-type"
)

// Kea control API result codes.
const (
	keaResultSuccess     = 0
	keaResultUnsupported = 2
	keaResultEmpty       = 3 // the command worked but found nothing
)

// ErrNoLease reports that Kea answered and holds no lease for a MAC (e.g. the
// machine hasn't booted yet), as opposed to Kea being unreachable.
var ErrNoLease = errors.New("no lease found")

// ErrSubnetNotFound reports that Kea answered and has no subnet for a prefix.
var ErrSubnetNotFound = errors.New("no matching Kea subnet")

// PinMode controls whether an existing MAC-only reservation is upgraded to hold
// the MAC's current lease IP.
type PinMode string

const (
	// PinModeOff never upgrades MAC-only reservations (the historical behavior).
	PinModeOff PinMode = "off"
	// PinModeLog reports what would be pinned, and conflicts, without writing.
	PinModeLog PinMode = "log"
	// PinModeEnforce upgrades MAC-only reservations to hold the lease IP.
	PinModeEnforce PinMode = "enforce"
)

// ReservationResult describes what EnsureReservationForMACIP found and did.
type ReservationResult struct {
	Created  bool   // a reservation was added for a MAC that had none
	Upgraded bool   // an existing MAC-only reservation now holds the lease IP
	Pinned   bool   // the MAC's reservation holds its IP
	WouldPin bool   // log mode: the reservation would have been upgraded
	Warning  string // why the lease IP is not pinned, when that needs attention
}

// Service wraps Kea operations used by the controller.
type Service struct {
	Client keainterface.KeaClient

	// PinMode governs upgrading MAC-only reservations; the zero value behaves as PinModeOff.
	PinMode PinMode

	// subnetLocks serializes GetOrCreateSubnet for the same subnet CIDR within
	// this process. Multiple NetworkConfigurations that share a NetworkNamespace
	// prefix reconcile concurrently — the workqueue only serializes per object
	// key — so without this each would independently issue subnet4-add for the
	// same prefix. Keyed by CIDR; the number of entries is bounded by the number
	// of distinct subnets.
	subnetLocks sync.Map // map[string]*sync.Mutex
}

func New(client keainterface.KeaClient) *Service {
	return &Service{Client: client}
}

// subnetLock returns the per-CIDR mutex used to serialize subnet get-or-create.
func (s *Service) subnetLock(cidr string) *sync.Mutex {
	m, _ := s.subnetLocks.LoadOrStore(cidr, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// CreateSubnet creates a new IPv4 subnet in Kea DHCP server.
// Returns the subnet ID of the newly created subnet.
func (s *Service) CreateSubnet(ctx context.Context, cfg keamodels.SubnetConfig) (int, error) {
	if cfg.Subnet == "" {
		return 0, fmt.Errorf("subnet CIDR is required")
	}

	// Determine subnet ID - use provided ID or generate the next available one
	subnetID := cfg.ID
	if subnetID <= 0 {
		subnetID = s.getNextSubnetID(ctx)
	}

	// Build the subnet4 configuration
	subnet4 := map[string]any{
		"subnet": cfg.Subnet,
		"id":     subnetID,
	}

	// Set valid lifetime (default to 4000 if not specified)
	if cfg.ValidLife > 0 {
		subnet4["valid-lifetime"] = cfg.ValidLife
	} else {
		subnet4["valid-lifetime"] = 4000
	}

	if cfg.RenewTimer > 0 {
		subnet4["renew-timer"] = cfg.RenewTimer
	}

	if cfg.RebindTimer > 0 {
		subnet4["rebind-timer"] = cfg.RebindTimer
	}

	// Build pools if start and end are specified
	if cfg.PoolStart != "" && cfg.PoolEnd != "" {
		pool := map[string]any{
			"pool": fmt.Sprintf("%s - %s", cfg.PoolStart, cfg.PoolEnd),
		}
		if len(cfg.RequireClientClasses) > 0 {
			pool["require-client-classes"] = cfg.RequireClientClasses
		}
		subnet4["pools"] = []map[string]any{pool}
	}

	// Build option-data for gateway and DNS
	var optionData []map[string]any

	if cfg.Gateway != "" {
		optionData = append(optionData, map[string]any{
			"name": "routers",
			"code": 3,
			"data": cfg.Gateway,
		})
	}

	if len(cfg.DNS) > 0 {
		optionData = append(optionData, map[string]any{
			"name": "domain-name-servers",
			"code": 6,
			"data": strings.Join(cfg.DNS, ", "),
		})
	}

	if len(optionData) > 0 {
		subnet4["option-data"] = optionData
	}

	req := keamodels.Request{
		Command: "subnet4-add",
		Args: map[string]any{
			"subnet4": []map[string]any{subnet4},
		},
	}

	resp, err := s.Client.Send(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("failed to send subnet4-add request: %w", err)
	}

	if resp.Result != 0 {
		return 0, fmt.Errorf("kea subnet4-add failed: %s", resp.Text)
	}

	// Return the subnet ID we used
	return subnetID, nil
}

// getNextSubnetID finds the next available subnet ID by listing existing subnets
func (s *Service) getNextSubnetID(ctx context.Context) int {
	req := keamodels.Request{Command: "subnet4-list", Args: map[string]any{}}
	resp, err := s.Client.Send(ctx, req)
	if err != nil {
		return 1 // If we can't list, start with ID 1
	}
	if resp.Result != 0 {
		// No subnets exist or command failed, start with ID 1
		return 1
	}

	subnets, ok := resp.Arguments["subnets"].([]any)
	if !ok || len(subnets) == 0 {
		return 1 // No subnets, start with ID 1
	}

	// Find the maximum existing ID
	maxID := 0
	for _, snet := range subnets {
		m, ok := snet.(map[string]any)
		if !ok {
			continue
		}
		switch idv := m["id"].(type) {
		case float64:
			if int(idv) > maxID {
				maxID = int(idv)
			}
		case int:
			if idv > maxID {
				maxID = idv
			}
		}
	}

	return maxID + 1
}

// GetOrCreateSubnet returns the subnet ID for the given prefix, creating the subnet if it doesn't exist.
func (s *Service) GetOrCreateSubnet(ctx context.Context, cfg keamodels.SubnetConfig) (int, bool, error) {
	// Serialize get-or-create for this prefix so a burst of NetworkConfigurations
	// sharing one subnet issues a single subnet4-add instead of one per reconcile.
	lock := s.subnetLock(cfg.Subnet)
	lock.Lock()
	defer lock.Unlock()

	// First, try to find an existing subnet
	subnetID, err := s.GetSubnetID(ctx, cfg.Subnet)
	if err == nil {
		return subnetID, false, nil // Subnet exists, not created
	}

	// Only treat "no matching subnet" as a signal to create. Any other error
	// (e.g. a transport/list failure) is surfaced so we don't create blindly.
	if !errors.Is(err, ErrSubnetNotFound) {
		return 0, false, err
	}

	// Not found — create it.
	newID, createErr := s.CreateSubnet(ctx, cfg)
	if createErr == nil {
		return newID, true, nil // Subnet created
	}

	// Create failed. A concurrent writer — another replica (leader election is
	// optional) or the HA peer — may have created the subnet between our list and
	// create, or Kea may have rejected our chosen ID. Re-resolve by CIDR before
	// surfacing the error, rather than depending on the exact wording of Kea's
	// error message.
	if existingID, getErr := s.GetSubnetID(ctx, cfg.Subnet); getErr == nil {
		return existingID, false, nil
	}
	return 0, false, fmt.Errorf("failed to create subnet: %w", createErr)
}

// GetSubnetID lists Kea subnets and returns the id of the subnet matching the
// given IPv4 CIDR prefix. It returns an error wrapping ErrSubnetNotFound when
// Kea has no such subnet.
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

		// Extract subnet string - handle both direct string and pointer to interface containing string
		var subnetStr string
		switch subVal := m["subnet"].(type) {
		case string:
			subnetStr = subVal
		case *any:
			if subVal != nil {
				if str, ok := (*subVal).(string); ok {
					subnetStr = str
				}
			}
		}

		if subnetStr == ipv4Prefix {
			// Extract ID - handle both numeric types and pointer to interface
			switch idv := m["id"].(type) {
			case float64:
				return int(idv), nil
			case int:
				return idv, nil
			case *any:
				if idv != nil {
					switch id := (*idv).(type) {
					case float64:
						return int(id), nil
					case int:
						return id, nil
					}
				}
			}
		}
	}
	return 0, fmt.Errorf("%w for prefix %s", ErrSubnetNotFound, ipv4Prefix)
}

// SubnetInfo contains details about a Kea subnet
type SubnetInfo struct {
	ID      int
	Subnet  string
	Gateway string
	DNS     []string
}

// GetSubnetInfo retrieves detailed subnet information including gateway and DNS servers
func (s *Service) GetSubnetInfo(ctx context.Context, subnetID int) (*SubnetInfo, error) {
	req := keamodels.Request{
		Command: "subnet4-get",
		Args:    map[string]any{"id": subnetID},
	}
	resp, err := s.Client.Send(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Result != 0 {
		return nil, fmt.Errorf("kea subnet4-get failed: %s", resp.Text)
	}

	// subnet4-get returns "subnet4" key, not "subnets"
	subnet4List, ok := resp.Arguments["subnet4"].([]any)
	if !ok || len(subnet4List) == 0 {
		return nil, fmt.Errorf("unexpected subnet4-get response shape")
	}

	subnetData, ok := subnet4List[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected subnet data format")
	}

	info := &SubnetInfo{ID: subnetID}

	// Extract subnet CIDR
	if subnet, ok := subnetData["subnet"].(string); ok {
		info.Subnet = subnet
	} else if subnetPtr, ok := subnetData["subnet"].(*any); ok && subnetPtr != nil {
		if str, ok := (*subnetPtr).(string); ok {
			info.Subnet = str
		}
	}

	// Extract option-data for gateway (router) and DNS
	if optionData, ok := subnetData["option-data"].([]any); ok {
		for _, opt := range optionData {
			optMap, ok := opt.(map[string]any)
			if !ok {
				continue
			}

			var code float64
			var data string

			// Extract code
			switch c := optMap["code"].(type) {
			case float64:
				code = c
			case int:
				code = float64(c)
			case *any:
				if c != nil {
					if f, ok := (*c).(float64); ok {
						code = f
					} else if i, ok := (*c).(int); ok {
						code = float64(i)
					}
				}
			}

			// Extract data
			switch d := optMap["data"].(type) {
			case string:
				data = d
			case *any:
				if d != nil {
					if str, ok := (*d).(string); ok {
						data = str
					}
				}
			}

			// Code 3 = router (gateway), Code 6 = DNS servers
			if code == 3 && data != "" {
				info.Gateway = data
			} else if code == 6 && data != "" {
				// DNS can be comma-separated
				for dns := range strings.SplitSeq(data, ",") {
					dns = strings.TrimSpace(dns)
					if dns != "" {
						info.DNS = append(info.DNS, dns)
					}
				}
			}
		}
	}

	return info, nil
}

// DeleteReservationForMAC removes a reservation for the given MAC and subnet.
// A reservation that is already gone counts as deleted.
func (s *Service) DeleteReservationForMAC(ctx context.Context, mac string, subnetID int) error {
	mac = strings.ToLower(strings.TrimSpace(mac))
	if mac == "" {
		return fmt.Errorf("missing mac")
	}
	delReq := keamodels.Request{
		Command: "reservation-del",
		Args: map[string]any{
			keaFieldSubnetID:       subnetID,
			keaFieldIdentifierType: keaFieldHWAddress,
			keaFieldIdentifier:     mac,
			"operation-target":     "all",
		},
	}
	resp, err := s.Client.Send(ctx, delReq)
	if err != nil {
		return err
	}
	if resp.Result != keaResultSuccess && !isEmptyResult(resp) {
		return fmt.Errorf("kea reservation-del failed: %s", resp.Text)
	}
	return nil
}

// EnsureReservationForMACIP ensures a reservation exists for mac in the given
// subnet. A new reservation holds ipv4 when one is given. An existing MAC-only
// reservation is upgraded to hold ipv4 according to s.PinMode. A reservation
// that already holds an IP, or carries settings of its own, is never changed.
func (s *Service) EnsureReservationForMACIP(ctx context.Context, mac string, subnetID int, ipv4 string) (ReservationResult, error) {
	mac = strings.ToLower(strings.TrimSpace(mac))
	if mac == "" {
		return ReservationResult{}, fmt.Errorf("missing mac")
	}
	ip := strings.TrimSpace(ipv4)

	host, found, err := s.getReservation(ctx, mac, subnetID)
	if err != nil {
		return ReservationResult{}, err
	}
	if !found {
		if err := s.addReservation(ctx, mac, subnetID, ip); err != nil {
			return ReservationResult{}, err
		}
		return ReservationResult{Created: true, Pinned: ip != ""}, nil
	}

	reserved := hostIPv4(host)
	switch {
	case reserved != "" && (ip == "" || reserved == ip):
		return ReservationResult{Pinned: true}, nil
	case reserved != "":
		return ReservationResult{Warning: fmt.Sprintf("reservation holds %s but the lease is %s; reservation left unchanged", reserved, ip)}, nil
	case ip == "":
		return ReservationResult{}, nil // MAC-only and no lease yet: nothing to pin
	}
	return s.pinMACOnlyReservation(ctx, mac, subnetID, ip, host)
}

// pinMACOnlyReservation upgrades mac's MAC-only reservation to hold ip, as
// allowed by s.PinMode.
func (s *Service) pinMACOnlyReservation(ctx context.Context, mac string, subnetID int, ip string, host map[string]any) (ReservationResult, error) {
	if s.PinMode != PinModeLog && s.PinMode != PinModeEnforce {
		return ReservationResult{}, nil
	}
	if !isBareReservation(host) {
		return ReservationResult{Warning: fmt.Sprintf("reservation has settings of its own; not pinning %s", ip)}, nil
	}
	owner, err := s.reservationOwnerOfIP(ctx, subnetID, ip)
	if err != nil {
		return ReservationResult{}, err
	}
	if owner != "" {
		return ReservationResult{Warning: fmt.Sprintf("%s is already reserved for %s; not pinned", ip, owner)}, nil
	}
	if s.PinMode == PinModeLog {
		return ReservationResult{WouldPin: true}, nil
	}

	// Kea allows one reservation per MAC and subnet, so the MAC-only one is
	// replaced. It carries no settings, so the brief gap changes nothing.
	if err := s.DeleteReservationForMAC(ctx, mac, subnetID); err != nil {
		return ReservationResult{}, fmt.Errorf("pinning %s: %w", ip, err)
	}
	if addErr := s.addReservation(ctx, mac, subnetID, ip); addErr != nil {
		if restoreErr := s.addReservation(ctx, mac, subnetID, ""); restoreErr != nil {
			return ReservationResult{}, fmt.Errorf("pinning %s failed: %w; restoring the MAC-only reservation also failed: %v", ip, addErr, restoreErr)
		}
		return ReservationResult{}, fmt.Errorf("pinning %s failed, MAC-only reservation restored: %w", ip, addErr)
	}
	return ReservationResult{Upgraded: true, Pinned: true}, nil
}

// addReservation adds a reservation for mac in subnetID, holding ip if given.
func (s *Service) addReservation(ctx context.Context, mac string, subnetID int, ip string) error {
	reservation := map[string]any{
		keaFieldSubnetID:  subnetID,
		keaFieldHWAddress: mac,
	}
	if ip != "" {
		reservation[keaFieldIPAddress] = ip
	}
	addReq := keamodels.Request{
		Command: "reservation-add",
		Args: map[string]any{
			"reservation":      reservation,
			"operation-target": "all",
		},
	}
	resp, err := s.Client.Send(ctx, addReq)
	if err != nil {
		return err
	}
	if resp.Result != keaResultSuccess {
		return fmt.Errorf("kea reservation-add failed: %s", resp.Text)
	}
	return nil
}

// getReservation returns mac's reservation in subnetID. found is false when
// Kea has none; an error means Kea couldn't be asked, so callers must not write.
func (s *Service) getReservation(ctx context.Context, mac string, subnetID int) (host map[string]any, found bool, err error) {
	byID := keamodels.Request{
		Command: "reservation-get-by-id",
		Args: map[string]any{
			keaFieldIdentifierType: keaFieldHWAddress,
			keaFieldIdentifier:     mac,
		},
	}
	if resp, sendErr := s.Client.Send(ctx, byID); sendErr == nil {
		if resp.Result == keaResultSuccess {
			host, found = findHost(resp.Arguments, mac, subnetID)
			return host, found, nil
		}
		if isEmptyResult(resp) {
			return nil, false, nil
		}
	}

	// Fallback (older Kea, or the first lookup failed): scan the subnet.
	all := keamodels.Request{Command: "reservation-get-all", Args: map[string]any{keaFieldSubnetID: subnetID}}
	resp, sendErr := s.Client.Send(ctx, all)
	if sendErr != nil {
		return nil, false, fmt.Errorf("reading reservations for %s: %w", mac, sendErr)
	}
	if isEmptyResult(resp) {
		return nil, false, nil
	}
	if resp.Result != keaResultSuccess {
		return nil, false, fmt.Errorf("kea reservation-get-all failed: %s", resp.Text)
	}
	host, found = findHost(resp.Arguments, mac, subnetID)
	return host, found, nil
}

// findHost returns the host for mac in subnetID from a host_cmds "hosts" list.
func findHost(args map[string]any, mac string, subnetID int) (map[string]any, bool) {
	hosts, _ := args["hosts"].([]any)
	for _, h := range hosts {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if hw, ok := hm[keaFieldHWAddress].(string); !ok || !strings.EqualFold(hw, mac) {
			continue
		}
		switch v := hm[keaFieldSubnetID].(type) {
		case float64:
			if int(v) != subnetID {
				continue
			}
		case int:
			if v != subnetID {
				continue
			}
		}
		return hm, true
	}
	return nil, false
}

// reservationOwnerOfIP returns who holds a reservation for ip in subnetID, or
// "" when the address isn't reserved.
func (s *Service) reservationOwnerOfIP(ctx context.Context, subnetID int, ip string) (string, error) {
	req := keamodels.Request{
		Command: "reservation-get",
		Args: map[string]any{
			keaFieldSubnetID:  subnetID,
			keaFieldIPAddress: ip,
		},
	}
	resp, err := s.Client.Send(ctx, req)
	if err != nil {
		return "", fmt.Errorf("checking reservations for %s: %w", ip, err)
	}
	if isEmptyResult(resp) {
		return "", nil
	}
	if resp.Result != keaResultSuccess {
		return "", fmt.Errorf("kea reservation-get failed: %s", resp.Text)
	}
	if hw, ok := resp.Arguments[keaFieldHWAddress].(string); ok && hw != "" {
		return strings.ToLower(hw), nil
	}
	return "another client", nil
}

// hostIPv4 returns the address a reservation holds, or "" for a MAC-only one.
func hostIPv4(host map[string]any) string {
	ip, _ := host[keaFieldIPAddress].(string)
	ip = strings.TrimSpace(ip)
	if ip == "0.0.0.0" {
		return ""
	}
	return ip
}

// isBareReservation reports whether a reservation carries nothing but its MAC
// and subnet — the shape this operator creates. Kea also lists default-valued
// fields (empty hostname, 0.0.0.0 next-server, ...), which don't count.
func isBareReservation(host map[string]any) bool {
	for k, v := range host {
		if k == keaFieldHWAddress || k == keaFieldSubnetID {
			continue
		}
		if !isEmptyValue(v) {
			return false
		}
	}
	return true
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == "" || x == "0.0.0.0"
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// isEmptyResult reports whether Kea answered "nothing found": result 3, or the
// not-found wording older versions used with other result codes.
func isEmptyResult(resp keamodels.Response) bool {
	if resp.Result == keaResultEmpty {
		return true
	}
	txt := strings.ToLower(resp.Text)
	return strings.Contains(txt, "not found") || strings.Contains(txt, "no host") || strings.Contains(txt, "0 ipv4 host")
}

// GetLeaseIPv4ForMAC resolves the IPv4 lease for the given MAC.
// Returns ip, subnet-id (if available), error. The error wraps ErrNoLease when
// Kea answered but holds no lease; any other error means Kea couldn't be asked.
func (s *Service) GetLeaseIPv4ForMAC(ctx context.Context, mac string) (string, int, error) {
	mac = strings.ToLower(strings.TrimSpace(mac))
	if mac == "" {
		return "", 0, fmt.Errorf("missing mac")
	}
	primary := keamodels.Request{
		Command: "lease4-get-by-hw-address",
		Args:    map[string]any{keaFieldHWAddress: mac},
	}
	resp, err := s.Client.Send(ctx, primary)
	if err != nil {
		return "", 0, fmt.Errorf("lease lookup for %s: %w", mac, err)
	}
	switch {
	case resp.Result == keaResultSuccess:
		if ip, sid := newestLease(resp.Arguments, mac); ip != "" {
			return ip, sid, nil
		}
	case resp.Result == keaResultUnsupported, isEmptyResult(resp):
		// No lease, or lease commands unavailable: try the reservation below.
	default:
		return "", 0, fmt.Errorf("kea lease4-get-by-hw-address failed: %s", resp.Text)
	}

	// Fallback: reservation-get-by-id for any stored address
	fb := keamodels.Request{
		Command: "reservation-get-by-id",
		Args: map[string]any{
			keaFieldIdentifierType: keaFieldHWAddress,
			keaFieldIdentifier:     mac,
		},
	}
	resp, err = s.Client.Send(ctx, fb)
	if err != nil {
		return "", 0, fmt.Errorf("reservation lookup for %s: %w", mac, err)
	}
	if resp.Result == keaResultSuccess {
		if hosts, ok := resp.Arguments["hosts"].([]any); ok {
			for _, h := range hosts {
				hm, ok := h.(map[string]any)
				if !ok {
					continue
				}
				if v, ok2 := hm[keaFieldIPAddress].(string); ok2 && v != "" {
					sid := 0
					if sv, ok3 := hm[keaFieldSubnetID].(float64); ok3 {
						sid = int(sv)
					} else if sv2, ok4 := hm[keaFieldSubnetID].(int); ok4 {
						sid = sv2
					}
					return v, sid, nil
				}
			}
		}
	}
	// Not finding a lease is not necessarily a problem - the machine might not
	// have booted yet or the lease may have expired. Callers check ErrNoLease.
	return "", 0, fmt.Errorf("%w for MAC %s", ErrNoLease, mac)
}

// newestLease picks the most recent (largest cltt) lease for mac from a
// lease4-get-by-hw-address answer. Returns "" when there is none.
func newestLease(args map[string]any, mac string) (string, int) {
	// Kea can return leases as an array; pick the newest (largest cltt) that matches the MAC.
	if arr, ok := args["leases"].([]any); ok {
		bestIP := ""
		bestSID := 0
		var bestCLTT float64
		for _, elem := range arr {
			m, ok := elem.(map[string]any)
			if !ok {
				continue
			}
			hw, _ := m[keaFieldHWAddress].(string)
			if !strings.EqualFold(strings.TrimSpace(hw), mac) {
				// Be defensive in case server returns extra entries
				continue
			}
			ip, _ := m[keaFieldIPAddress].(string)
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
			switch v := m[keaFieldSubnetID].(type) {
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
		return bestIP, bestSID
	}
	if l, ok := args["leases"].(map[string]any); ok {
		// Some deployments might return a single lease object; keep legacy support.
		ip, _ := l[keaFieldIPAddress].(string)
		sid := 0
		if v, ok2 := l[keaFieldSubnetID].(float64); ok2 {
			sid = int(v)
		} else if v2, ok3 := l[keaFieldSubnetID].(int); ok3 {
			sid = v2
		}
		return ip, sid
	}
	return "", 0
}
