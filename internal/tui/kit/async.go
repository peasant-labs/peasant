package kit

import "sync/atomic"

// asyncOwnerID identifies one concrete asynchronous kit component instance.
// Constructors allocate it once; ordinary value copies deliberately preserve
// it so Bubble Tea's value-model updates still describe the same owner.
type asyncOwnerID uint64

func (id asyncOwnerID) valid() bool { return id != 0 }

// asyncOwnerAllocator is the process-local monotonic owner allocator. It uses
// compare-and-swap rather than Add so reaching the final value leaves the
// counter pinned there: a recovered exhaustion panic cannot wrap and reuse an
// identity that an older component may still hold.
type asyncOwnerAllocator struct {
	last atomic.Uint64
}

func (a *asyncOwnerAllocator) next() asyncOwnerID {
	const maximum = ^uint64(0)
	for {
		current := a.last.Load()
		if current == maximum {
			panic("allocate kit async owner: the process exhausted every nonzero owner identity; no component was constructed, because reusing an identity could deliver a result to the wrong component; restart the process before mounting another asynchronous component")
		}
		next := current + 1
		if next == 0 {
			panic("allocate kit async owner: the owner identity wrapped to zero; no component was constructed, because zero is never a valid asynchronous owner; restart the process before mounting another asynchronous component")
		}
		if a.last.CompareAndSwap(current, next) {
			return asyncOwnerID(next)
		}
	}
}

var componentAsyncOwners asyncOwnerAllocator

func nextAsyncOwnerID() asyncOwnerID { return componentAsyncOwners.next() }
