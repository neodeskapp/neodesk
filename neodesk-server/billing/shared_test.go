package billing

import (
	"testing"

	"github.com/neodeskapp/neodesk/neodesk-server/cluster"
)

// TestARelayPreparedOnOnePodIsTakenOnAnother: signal prepares the relay
// on pod A, the legs pair on pod B. B finds the billing data once.
func TestARelayPreparedOnOnePodIsTakenOnAnother(t *testing.T) {
	kv := cluster.NewMemKV()
	a, b := NewService(nil, nil, 1, false), NewService(nil, nil, 1, false)
	a.SetShared(kv)
	b.SetShared(kv)
	a.sharePending("uuid-1", &pendingRelay{OrgID: "org1", DeviceID: "123456789", OperatorID: "987654321", OperatorName: "alice", ContractID: "c1", Currency: "EUR"})

	meta, ok := b.takeShared("uuid-1")
	if !ok {
		t.Fatal("pod b does not find the relay pod a prepared")
	}
	if meta.ContractID != "c1" || meta.OperatorName != "alice" || meta.DeviceID != "123456789" {
		t.Fatalf("pod b got %+v", meta)
	}
	if _, ok := b.takeShared("uuid-1"); ok {
		t.Fatal("the relay was taken twice")
	}
}
