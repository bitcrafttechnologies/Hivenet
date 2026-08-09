package reconcile

import "fmt"

// OpKind is one kind of change the reconciler can make. The set is closed:
// every difference between desired and actual state reduces to these five
// (spec §5.2).
type OpKind string

const (
	OpDestroyLink      OpKind = "DestroyLink"
	OpDestroyNode      OpKind = "DestroyNode"
	OpCreateNode       OpKind = "CreateNode"
	OpCreateLink       OpKind = "CreateLink"
	OpUpdateNodeConfig OpKind = "UpdateNodeConfig"
)

// phase is the position of this op kind in the fixed apply sequence: destroy
// links → destroy nodes → create nodes → create links → config updates last
// (spec §5.3).
//
// The order is not incidental. Links are torn down first so that node teardown
// never races a live veth, and config is applied last so it lands on
// interfaces that already exist. To change the ordering, change these numbers —
// never reorder at a call site.
func (k OpKind) phase() int {
	switch k {
	case OpDestroyLink:
		return 0
	case OpDestroyNode:
		return 1
	case OpCreateNode:
		return 2
	case OpCreateLink:
		return 3
	case OpUpdateNodeConfig:
		return 4
	default:
		return 99
	}
}

// Op is a single unit of work produced by Diff and handed to the driver.
// Exactly one of NodeID or LinkID is set, depending on Kind.
type Op struct {
	Kind   OpKind `json:"kind"`
	NodeID string `json:"nodeId,omitempty"`
	LinkID string `json:"linkId,omitempty"`
	// Reason explains what made this op necessary. It is carried into logs and
	// into the error message on a failed node, so it should read as an
	// explanation to a user, not to a compiler.
	Reason string `json:"reason,omitempty"`
}

// Target returns the ID of the entity this op acts on.
func (o Op) Target() string {
	if o.NodeID != "" {
		return o.NodeID
	}
	return o.LinkID
}

func (o Op) String() string {
	return fmt.Sprintf("%s(%s): %s", o.Kind, o.Target(), o.Reason)
}
