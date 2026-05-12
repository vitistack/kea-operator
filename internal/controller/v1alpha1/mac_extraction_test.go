package v1alpha1

import (
	"testing"

	"github.com/vitistack/kea-operator/internal/util/unstructuredconv"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Field names + sentinel MAC reused across this test file. Extracted as
// constants so goconst stays quiet and so a typo in one fixture doesn't
// silently misalign the test data with the production extractor's
// expectations.
const (
	fieldNetworkInterfaces = "networkInterfaces"
	fieldMACAddress        = "macAddress"
	macDuplicateFixture    = "aa:bb:cc:dd:ee:0c"
)

// setEquals compares slice of strings against a wanted set.
func setEquals(got []string, want map[string]struct{}) (bool, string) {
	if len(got) != len(want) {
		return false, "length mismatch"
	}
	for _, g := range got {
		if _, ok := want[g]; !ok {
			return false, g
		}
	}
	return true, ""
}

func TestExtractMACs_Scenarios(t *testing.T) {
	mk := func(spec, status map[string]any) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.Object = map[string]any{}
		if spec != nil {
			u.Object["spec"] = spec
		}
		if status != nil {
			u.Object["status"] = status
		}
		return u
	}

	tests := []struct {
		name   string
		spec   map[string]any
		status map[string]any
		want   map[string]struct{}
	}{
		{
			name: "reads only spec.networkInterfaces[].macAddress and ignores others",
			spec: map[string]any{
				fieldNetworkInterfaces: []any{
					map[string]any{fieldMACAddress: "AA-BB-CC-DD-EE-FF"}, // normalized -> aa:bb:cc:dd:ee:ff
					map[string]any{fieldMACAddress: "aa:bb:cc:dd:ee:01"},
					map[string]any{"name": "eth0"}, // no macAddress => ignored
				},
				// should be ignored
				"mac":           "aa:bb:cc:dd:ee:02",
				fieldMACAddress: "aa:bb:cc:dd:ee:03",
				"macs":          []any{"aa:bb:cc:dd:ee:04"},
			},
			status: map[string]any{
				// should be ignored
				fieldNetworkInterfaces: []any{map[string]any{fieldMACAddress: "aa:bb:cc:dd:ee:05"}},
				"mac":                  "aa:bb:cc:dd:ee:06",
				fieldMACAddress:        "aa:bb:cc:dd:ee:07",
			},
			want: map[string]struct{}{
				"aa:bb:cc:dd:ee:ff": {},
				"aa:bb:cc:dd:ee:01": {},
			},
		},
		{
			name: "empty when no spec.networkInterfaces",
			spec: map[string]any{},
			want: map[string]struct{}{},
		},
		{
			name: "normalizes and validates (lowercase, '-' -> ':')",
			spec: map[string]any{
				fieldNetworkInterfaces: []any{
					map[string]any{fieldMACAddress: "AA-BB-CC-DD-EE-0A"},
					map[string]any{fieldMACAddress: "aa:bb:cc:dd:ee:0b"},
				},
			},
			want: map[string]struct{}{
				"aa:bb:cc:dd:ee:0a": {},
				"aa:bb:cc:dd:ee:0b": {},
			},
		},
		{
			name: "deduplicates and ignores invalid values",
			spec: map[string]any{
				fieldNetworkInterfaces: []any{
					map[string]any{fieldMACAddress: macDuplicateFixture},
					map[string]any{fieldMACAddress: macDuplicateFixture}, // duplicate
					map[string]any{fieldMACAddress: "not-a-mac"},         // invalid
					map[string]any{fieldMACAddress: "aa:bb:cc:dd:ee"},    // invalid (short)
					map[string]any{"name": "eth1"},                       // missing macAddress
				},
			},
			want: map[string]struct{}{
				macDuplicateFixture: {},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(tt *testing.T) {
			u := mk(tc.spec, tc.status)
			// Convert to typed NetworkConfiguration and then extract
			nc, err := unstructuredconv.ToNetworkConfiguration(u)
			if err != nil {
				tt.Fatalf("conversion failed: %v", err)
			}
			got := extractMACsFromTypedNetworkConfiguration(nc)
			ok, extra := setEquals(got, tc.want)
			if !ok {
				tt.Fatalf("unexpected result: got=%v unexpected=%s want-set=%v", got, extra, tc.want)
			}
		})
	}
}
