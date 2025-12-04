package keamodels

type Request struct {
	Command string         `json:"command"`
	Service string         `json:"service,omitempty"` // e.g. "dhcp4"
	Args    map[string]any `json:"arguments,omitempty"`
}

type Response struct {
	Result    int            `json:"result"`
	Text      string         `json:"text,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// SubnetConfig contains configuration options for creating a new subnet
type SubnetConfig struct {
	Subnet      string   // Required: CIDR notation (e.g., "192.168.1.0/24")
	ID          int      // Optional: specific subnet ID (0 = auto-assign)
	Gateway     string   // Optional: default gateway (router option)
	DNS         []string // Optional: DNS servers
	PoolStart   string   // Optional: start of IP pool range
	PoolEnd     string   // Optional: end of IP pool range
	ValidLife   int      // Optional: valid lifetime in seconds (default: 4000)
	RenewTimer  int      // Optional: renew timer in seconds
	RebindTimer int      // Optional: rebind timer in seconds
}
