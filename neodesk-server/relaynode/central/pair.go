package central

import (
	"github.com/neodeskapp/neodesk/neodesk-server/signal"
	"github.com/neodeskapp/neodesk/neodesk-server/sites"
)

// pairRoute is the route between two sites (phase 2). Not yet routed:
// the session keeps today's relay.
func (g *Glue) pairRoute(sites.NodeRoute) (signal.NodeRelay, bool) {
	return signal.NodeRelay{}, false
}
