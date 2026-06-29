package kea

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vitistack/kea-operator/pkg/models/keamodels"
)

const (
	cmdSubnet4Add  = "subnet4-add"
	cmdSubnet4List = "subnet4-list"
	testCIDR       = "10.0.0.0/24"
)

// subnetEntry is a minimal in-memory Kea subnet record for the stateful fakes below.
type subnetEntry struct {
	id   int
	cidr string
}

// parseSubnet4Add extracts the CIDR and id from a subnet4-add command payload.
func parseSubnet4Add(cmd keamodels.Request) (string, int) {
	arr, ok := cmd.Args["subnet4"].([]map[string]any)
	if !ok || len(arr) == 0 {
		return "", 0
	}
	cidr, _ := arr[0]["subnet"].(string)
	id := 0
	switch v := arr[0]["id"].(type) {
	case int:
		id = v
	case float64:
		id = int(v)
	}
	return cidr, id
}

func subnetListResponse(entries []subnetEntry) keamodels.Response {
	subs := make([]any, 0, len(entries))
	for _, e := range entries {
		subs = append(subs, map[string]any{"id": e.id, "subnet": e.cidr})
	}
	return keamodels.Response{Result: 0, Arguments: map[string]any{"subnets": subs}}
}

// rejectingKea reports the subnet as absent until subnet4-add is attempted, then
// reports it as present — and always rejects the add with a configurable error
// text. This models a concurrent writer (another replica or the HA peer) that
// created the subnet between our list and our create.
type rejectingKea struct {
	mu           sync.Mutex
	addAttempted bool
	existing     subnetEntry
	rejectText   string
	addCalls     int
}

func (f *rejectingKea) Send(_ context.Context, cmd keamodels.Request) (keamodels.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch cmd.Command {
	case cmdSubnet4List:
		if !f.addAttempted {
			return subnetListResponse(nil), nil
		}
		return subnetListResponse([]subnetEntry{f.existing}), nil
	case cmdSubnet4Add:
		f.addCalls++
		f.addAttempted = true
		return keamodels.Response{Result: 1, Text: f.rejectText}, nil
	}
	return keamodels.Response{Result: 0}, nil
}

// TestGetOrCreateSubnet_ReResolvesWhenCreateRejected verifies that when our
// subnet4-add is rejected (e.g. a concurrent writer won the race) GetOrCreateSubnet
// re-resolves the subnet by CIDR and succeeds, regardless of the exact error wording.
func TestGetOrCreateSubnet_ReResolvesWhenCreateRejected(t *testing.T) {
	client := &rejectingKea{
		existing: subnetEntry{id: 5, cidr: testCIDR},
		// Deliberately NOT the substring "already exists" — Kea reports ID
		// collisions as "is already in use".
		rejectText: "ID '5' is already in use",
	}
	svc := New(client)

	id, created, err := svc.GetOrCreateSubnet(context.Background(), keamodels.SubnetConfig{Subnet: testCIDR})
	if err != nil {
		t.Fatalf("expected no error after re-resolving by CIDR, got: %v", err)
	}
	if id != 5 {
		t.Fatalf("expected re-resolved subnet id 5, got %d", id)
	}
	if created {
		t.Fatalf("expected created=false when the subnet already existed")
	}
}

// statefulKea is a minimal stateful Kea fake: subnet4-add appends to its subnet
// list (after an optional delay to widen the race window), and subnet4-list
// reflects what has been added so far.
type statefulKea struct {
	mu       sync.Mutex
	subnets  []subnetEntry
	addCalls int
	addDelay time.Duration
}

func (f *statefulKea) Send(_ context.Context, cmd keamodels.Request) (keamodels.Response, error) {
	switch cmd.Command {
	case cmdSubnet4List:
		f.mu.Lock()
		resp := subnetListResponse(f.subnets)
		f.mu.Unlock()
		return resp, nil
	case cmdSubnet4Add:
		f.mu.Lock()
		f.addCalls++
		delay := f.addDelay
		f.mu.Unlock()
		// Append only after the delay so that, absent serialization, every
		// concurrent caller observes an empty list and issues its own add.
		if delay > 0 {
			time.Sleep(delay)
		}
		cidr, id := parseSubnet4Add(cmd)
		f.mu.Lock()
		f.subnets = append(f.subnets, subnetEntry{id: id, cidr: cidr})
		f.mu.Unlock()
		return keamodels.Response{Result: 0}, nil
	}
	return keamodels.Response{Result: 0}, nil
}

// TestGetOrCreateSubnet_ConcurrentSamePrefixCreatesOnce verifies that a burst of
// concurrent reconciles for NetworkConfigurations sharing one subnet prefix
// results in exactly one subnet4-add.
func TestGetOrCreateSubnet_ConcurrentSamePrefixCreatesOnce(t *testing.T) {
	client := &statefulKea{addDelay: 50 * time.Millisecond}
	svc := New(client)
	cfg := keamodels.SubnetConfig{Subnet: testCIDR}

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			<-start
			_, _, _ = svc.GetOrCreateSubnet(context.Background(), cfg)
		}()
	}
	close(start)
	wg.Wait()

	client.mu.Lock()
	got := client.addCalls
	client.mu.Unlock()
	if got != 1 {
		t.Fatalf("expected exactly 1 subnet4-add for concurrent reconciles of the same prefix, got %d", got)
	}
}
