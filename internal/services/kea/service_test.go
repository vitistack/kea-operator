package kea

import (
	"context"
	"testing"

	"github.com/vitistack/kea-operator/pkg/interfaces/keainterface"
	"github.com/vitistack/kea-operator/pkg/models/keamodels"
)

type fakeKeaClient struct {
	resp keamodels.Response
	err  error
}

func (f fakeKeaClient) Send(ctx context.Context, cmd keamodels.Request) (keamodels.Response, error) {
	return f.resp, f.err
}

func TestGetLeaseIPv4ForMAC_ArrayResponse_PicksLatest(t *testing.T) {
	mac := "00:02:12:34:56:78"
	client := fakeKeaClient{resp: keamodels.Response{
		Result: 0,
		Arguments: map[string]any{
			"leases": []any{
				map[string]any{
					"hw-address": mac,
					"ip-address": "100.64.0.50",
					"subnet-id":  1,
					"cltt":       1000,
				},
				map[string]any{
					"hw-address": mac,
					"ip-address": "100.64.0.123",
					"subnet-id":  1,
					"cltt":       2000,
				},
			},
		},
	}}
	service := &Service{Client: keainterface.KeaClient(client)}
	ip, sid, err := service.GetLeaseIPv4ForMAC(context.Background(), mac)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip != "100.64.0.123" || sid != 1 {
		t.Fatalf("unexpected ip/sid: %s/%d", ip, sid)
	}
}

func TestGetLeaseIPv4ForMAC_SingleObject(t *testing.T) {
	mac := "aa:bb:cc:dd:ee:ff"
	client := fakeKeaClient{resp: keamodels.Response{
		Result: 0,
		Arguments: map[string]any{
			"leases": map[string]any{
				"hw-address": mac,
				"ip-address": "100.64.0.200",
				"subnet-id":  2,
			},
		},
	}}
	service := &Service{Client: keainterface.KeaClient(client)}
	ip, sid, err := service.GetLeaseIPv4ForMAC(context.Background(), mac)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip != "100.64.0.200" || sid != 2 {
		t.Fatalf("unexpected ip/sid: %s/%d", ip, sid)
	}
}
