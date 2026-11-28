package central

import "testing"

func TestViaHasNode(t *testing.T) {
	for _, c := range []struct {
		via, node string
		want      bool
	}{
		{"node:n1", "n1", true},
		{"node:n1", "n2", false},
		{"node:n10", "n1", false},
		{"nodes:n1,n2", "n2", true},
		{"nodes:n1,n2", "n3", false},
		{"central", "n1", false},
		{"", "", false},
		{"node:", "", false},
	} {
		if got := viaHasNode(c.via, c.node); got != c.want {
			t.Errorf("viaHasNode(%q, %q) = %v, want %v", c.via, c.node, got, c.want)
		}
	}
}
