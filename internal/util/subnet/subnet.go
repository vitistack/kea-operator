package subnet

import (
	"fmt"
	"net"
)

// PoolConfig contains calculated pool configuration based on a CIDR
type PoolConfig struct {
	Gateway   string // First usable IP (e.g., 10.123.1.1)
	PoolStart string // Start of pool range (e.g., 10.123.1.4)
	PoolEnd   string // End of pool range (e.g., 10.123.1.254)
}

// CalculatePoolFromCIDR calculates gateway and pool range from a CIDR.
// Gateway is .1, pool starts at .4 and ends at .254 (for /24 networks).
// For other network sizes, pool ends at the last usable address before broadcast.
func CalculatePoolFromCIDR(cidr string) (*PoolConfig, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid CIDR %q: %w", cidr, err)
	}

	// Get network address as 4-byte slice
	ip := ipnet.IP.To4()
	if ip == nil {
		return nil, fmt.Errorf("only IPv4 CIDRs are supported: %s", cidr)
	}

	// Calculate broadcast address
	mask := ipnet.Mask
	broadcast := make(net.IP, 4)
	for i := range 4 {
		broadcast[i] = ip[i] | ^mask[i]
	}

	// Gateway is network + 1 (e.g., 10.123.1.1)
	gateway := make(net.IP, 4)
	copy(gateway, ip)
	gateway[3]++

	// Pool start is network + 4 (e.g., 10.123.1.4)
	poolStart := make(net.IP, 4)
	copy(poolStart, ip)
	poolStart[3] += 4

	// Pool end is broadcast - 1 (e.g., 10.123.1.254 for /24)
	poolEnd := make(net.IP, 4)
	copy(poolEnd, broadcast)
	poolEnd[3]--

	// Validate that pool range is valid (start < end)
	if !isIPLess(poolStart, poolEnd) {
		return nil, fmt.Errorf("network %s is too small for a valid pool", cidr)
	}

	return &PoolConfig{
		Gateway:   gateway.String(),
		PoolStart: poolStart.String(),
		PoolEnd:   poolEnd.String(),
	}, nil
}

// isIPLess returns true if a < b for IPv4 addresses
func isIPLess(a, b net.IP) bool {
	for i := range 4 {
		if a[i] < b[i] {
			return true
		}
		if a[i] > b[i] {
			return false
		}
	}
	return false
}
