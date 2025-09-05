package keamodels

type Request struct {
	Command string         `json:"command"`
	Service string         `json:"service,omitempty"` // e.g. "dhcp4"
	Args    map[string]any `json:"arguments,omitempty"`
}

type Response struct {
	Result    int            `json:"result"`
	Text      string         `json:"text"`
	Arguments map[string]any `json:"arguments,omitempty"`
}
