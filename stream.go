package http2

import (
	"container/list"
	"fmt"
)

const (
	defaultWeight    = 15
	defaulMaxDepTree = 100
	maxStreamWeight  = 255
)

var defaultPriority = PriorityParam{
	Weight:    defaultWeight,
	StreamDep: 0,
}

type streamDepState int

const (
	depStateIdle     streamDepState = iota // init state without data
	depStateReady                          // ready to send but blocked by dependency tree
	depStateTop                            // can send data
	depStatFlowDefer                       // blocked by stream flow control
)

var depStateName = [...]string{
	depStateIdle:     "Idle",
	depStateReady:    "Ready",
	depStateTop:      "Top",
	depStatFlowDefer: "FlowDefer",
}

func (ds streamDepState) String() string {
	return depStateName[ds]
}

// roots is a list of connection's root streams.
type roots struct {
	*list.List       // doubly linked list of all root streams
	n          int32 // number of streams in the dependency tree
}

func newRoots() *roots {
	return &roots{list.New(), 0}
}

// add stream and increment n.
func (r *roots) add(st *stream) {
	st.rootElem = r.PushFront(st)
	r.n += 1
}

func (r *roots) move(st *stream) {
	st.rootElem = r.PushFront(st)
}

// remove removes stream but doesn't decrement n.
func (r *roots) remove(st *stream) {
	r.Remove(st.rootElem)
}

// removeAll removes all streams but doesn't flush n.
func (r *roots) removeAll() {
	r.Init()
}

// stream represents a stream. This is the minimal metadata needed by
// the serve goroutine. Most of the actual stream state is owned by
// the http.Handler's goroutine in the responseWriter. Because the
// responseWriter's responseWriterState is recycled at the end of a
// handler, this struct intentionally has no pointer to the
// *responseWriter{,State} itself, as the Handler ending nils out the
// responseWriter's state field.
type stream struct {
	// immutable:
	id   uint32
	body *pipe       // non-nil if expecting DATA frames
	cw   closeWaiter // closed wait stream transitions to closed state

	// owned by serverConn's serve loop:
	bodyBytes     int64   // body bytes seen so far
	declBodyBytes int64   // or -1 if undeclared
	flow          flow    // limits writing from Handler to client
	inflow        flow    // what the client is allowed to POST/etc to us
	parent        *stream // or nil
	weight        uint8
	state         streamState
	sentReset     bool // only true once detached from streams map
	gotReset      bool // only true once detacted from streams map

	/* Dependency tree stucture
	 *
	 *                              a(1)
	 *                        _______________
	 *                       | depPrev = nil |
	 *                       | depNext = b   |
	 *                       | sibPrev = nil |
	 *                       | sibNext = nil |
	 *                       |_______________|
	 *                      /        |        \
	 *                     /         |         \
	 *          b(3)      /     c(5) |          \       i(17)
	 *    _______________/     ______|________   \ _______________
	 *   | depPrev = a   |    | depPrev = nil |   | depPrev = nil |
	 *   | depNext = d   |    | depNext = nil |   | depNext = nil |
	 *   | sibPrev = nil |    | sibPrev = b   |   | sibPrev = c   |
	 *   | sibNext = c   |    | sibNext = i   |   | sibNext = nil |
	 *   |_______________|    |_______________|   |_______________|
	 *           |
	 *      d(7) |
	 *    _______|________
	 *   | depPrev = b   |
	 *   | depNext = nil |
	 *   | sibPrev = nil |
	 *   | sibNext = nil |
	 *   |_______________|
	 */

	// dependency tree
	// owned by serverConn's serve loop:
	streams      map[uint32]*stream // point to server connection streams
	depPrev      *stream
	depNext      *stream
	sibNext      *stream
	sibPrev      *stream
	depState     streamDepState // state in dependency tree
	roots        *roots
	rootElem     *list.Element // root element in the roots
	n            int32         // number of total descendants plus me
	weightSum    int32         // sum of weight of direct descendants
	weightSumTop int32         // sum of weight of direct descendants that have depStateTop in its dependency tree (at any level deeper)
	weightEff    int32         // effective weight used for calculating virtual finish for weighted fair queue
}

func newStream(id uint32, sc *serverConn, priority PriorityParam) (*stream, error) {
	st := &stream{
		id:        id,
		weight:    priority.Weight,
		weightEff: int32(priority.Weight),
		roots:     sc.roots,
		streams:   sc.streams,
		n:         1,
		depState:  depStateIdle,
	}

	err := st.createStreamPriority(priority)
	if err != nil {
		return nil, err
	}

	st.cw.Init()
	st.flow.conn = &sc.flow // link to conn-level counter
	st.flow.add(sc.initialWindowSize)
	st.inflow.conn = &sc.inflow      // link to conn-level counter
	st.inflow.add(initialWindowSize) // TODO: update this when we send a higher initial window size in the initial settings

	st.streams[id] = st
	return st, nil
}

func (st *stream) String() string {
	return fmt.Sprintf("%d", st.id)
}

func (st *stream) addRoots() {
	st.roots.add(st)
}

func (st *stream) moveRoots() {
	st.roots.move(st)
}

func (st *stream) removeRoots() {
	st.roots.remove(st)
}

func (st *stream) setState(state streamState) {
	st.state = state
}

func (st *stream) adjustStreamPriority(priority PriorityParam) error {
	weight := priority.Weight

	if priority.StreamDep == 0 {
		st.removeDependentSubTree()
		st.weight = weight
		if priority.Exclusive {
			if st.roots.n < defaulMaxDepTree {
				st.becomeSingleRoot()
				return nil
			}
			// TODO (brk0v): test this
			// drop to default weight
			st.weight = defaultWeight
		}
		st.moveRoots()
		return nil
	} else {
		parent := st.streams[priority.StreamDep] // might be nil
		if parent == st {
			// TODO (brk0v): test this
			// A stream cannot depend on itself. An endpoint MUST treat this as
			// a stream error (Section 5.4.2) of type PROTOCOL_ERROR.
			return StreamError{st.id, ErrCodeProtocol}
		}
		if parent == nil {
			// TODO (brk0v): rewrite this with support of retention info and idle groups
			st.weight = defaultWeight
			st.moveRoots()
			return nil
		}
		// 5.3.3 Reprioritization
		// ...
		// If a stream is made dependent on one of its own dependencies, the
		// formerly dependent stream is first moved to be dependent on the
		// reprioritized stream's previous parent. The moved dependency retains
		// its weight.
		if st.isInSubTree(parent, true) {
			// TODO (brk0v): defaulMaxDepTree checks?
			parent.removeDependentSubTree()
			first := st.firstSib()
			if first.depPrev != nil {
				first.depPrev.addDependentSubTree(parent)
			} else {
				parent.moveRoots()
			}
		}
		st.removeDependentSubTree()
		st.weight = weight
		root := st.getRoot()
		if (root.n + st.n) < defaulMaxDepTree {
			if priority.Exclusive {
				parent.insertDependentSubTree(st)
			} else {
				parent.addDependentSubTree(st)
			}
		} else {
			// TODO (brk0v): test this
			st.weight = defaultWeight
			st.moveRoots()
		}
	}
	return nil
}

func (st *stream) createStreamPriority(priority PriorityParam) error {
	if priority.StreamDep == 0 {
		if priority.Exclusive {
			if st.roots.n < defaulMaxDepTree {
				st.becomeSingleRoot()
				return nil
			}
			// TODO (brk0v): test this
			// drop to default weight
			st.weight = defaultWeight
			st.weightEff = int32(st.weight)
		}
		st.addRoots()
		return nil
	} else {
		parent := st.streams[priority.StreamDep] // might be nil
		if parent == st {
			// TODO (brk0v): test this
			// A stream cannot depend on itself. An endpoint MUST treat this as
			// a stream error (Section 5.4.2) of type PROTOCOL_ERROR.
			return StreamError{st.id, ErrCodeProtocol}
		}
		if parent == nil {
			// TODO (brk0v): rewrite this with support of retention info and idle groups
			st.weight = defaultWeight
			st.weightEff = int32(st.weight)
			st.addRoots()
			return nil
		}
		// TODO (brk0v) test this
		root := st.getRoot()
		if root.n < defaulMaxDepTree {
			if priority.Exclusive {
				parent.insertDependent(st)
			} else {
				parent.addDependent(st)
			}
		} else {
			// TODO (brk0v): test this
			st.weight = defaultWeight
			st.weightEff = int32(st.weight)
			st.addRoots()
		}
	}
	return nil
}

// addDependent adds dependentcy to parent stream st.
func (st *stream) addDependent(chst *stream) {
	st.weightSum += int32(chst.weight)

	if st.depNext == nil {
		st.linkAddDependent(chst)
	} else {
		st.linkInsertDependent(chst)
	}

	root := st.updateNum(1)
	root.updateWeightSumTop()
	root.updateWeightEff()
	chst.roots.n += 1
}

// insertDependent inserts exclusive dependent stream chst to parent st.
func (st *stream) insertDependent(chst *stream) {
	chst.weightSum = st.weightSum
	st.weightSum = int32(chst.weight)

	if st.depNext != nil {
		for s := st.depNext; s != nil; s = s.sibNext {
			chst.n += s.n
		}
		chst.depNext = st.depNext
		chst.depNext.depPrev = chst
	}
	st.depNext = chst
	chst.depPrev = st

	root := st.updateNum(1)
	root.updateWeightSumTop()
	root.updateWeightEff()
	chst.roots.n += 1
}

// add sub tree
func (st *stream) addDependentSubTree(chst *stream) {
	chst.resetDepStateReady()

	if st.depNext == nil {
		st.weightSum = int32(chst.weight)
		st.linkAddDependent(chst)
	} else {
		st.weightSum += int32(chst.weight)
		st.linkInsertDependent(chst)
	}

	root := st.updateNum(chst.n)
	root.setDepStateTop()
	root.updateWeightSumTop()
	root.updateWeightEff()
}

func (st *stream) insertDependentSubTree(chst *stream) {
	d := chst.n
	chst.resetDepStateReady()

	if st.depNext != nil {
		chst.n += st.n - 1
		chst.weightSum += st.weightSum
		st.weightSum = int32(chst.weight)

		depNext := st.depNext
		depNext.resetDepStateReady()

		st.linkAddDependent(chst)

		if chst.depNext != nil {
			lastSib := chst.depNext.lastSib()
			lastSib.linkSib(depNext)
			depNext.depPrev = nil
		} else {
			chst.linkAddDependent(depNext)
		}
	} else {
		st.linkAddDependent(chst)

		if st.weightSum != 0 {
			panic("wrong weightSum for stream!")
		}
		st.weightSum = int32(chst.weight)
	}

	root := st.updateNum(d)
	root.setDepStateTop()
	root.updateWeightSumTop()
	root.updateWeightEff()
}

func (st *stream) closeStream() {
	st.setDepStateIdle()
	st.removeDependent()
}

func (st *stream) removeDependent() {
	var root *stream
	delta := -int32(st.weight)

	// distibute weight
	for s := st.depNext; s != nil; s = s.sibNext {
		s.weight = uint8(int32(st.weight) * int32(s.weight) / st.weightSum)
		if s.weight == 0 {
			s.weight = 1
		}
		delta += int32(s.weight)
	}

	firstSib := st.firstSib()
	depPrev := firstSib.depPrev

	if depPrev != nil {
		root = depPrev.updateNum(-1)
		depPrev.weightSum += delta
	}

	if st.sibPrev != nil {
		st.unlinkSib()
	} else if st.depPrev != nil {
		st.unlinkDependent()
	} else {
		// stream is a root
		st.removeRoots()

		// sibs become roots
		for s := st.depNext; s != nil; s = s.sibNext {
			s.depPrev = nil
			s.sibPrev = nil
			s.sibNext = nil

			s.weightEff = int32(s.weight)

			s.moveRoots()
		}
	}

	if root != nil {
		root.updateWeightSumTop()
		root.updateWeightEff()
	}

	st.n = 1
	st.weightSum = 0
	st.depPrev = nil
	st.depNext = nil
	st.sibPrev = nil
	st.sibNext = nil
	st.roots.n -= 1
}

// removeDependentSubTree removes subtree from dependency tree.
func (st *stream) removeDependentSubTree() {
	var depPrev *stream
	if st.sibPrev != nil {
		prev := st.sibPrev
		prev.sibNext = st.sibNext
		if prev.sibNext != nil {
			prev.sibNext.sibPrev = prev
		}

		prev = prev.firstSib()
		depPrev = prev.depPrev
	} else if st.depPrev != nil {
		depPrev = st.depPrev
		next := st.sibNext
		depPrev.depNext = next

		if next != nil {
			next.depPrev = depPrev
			next.sibPrev = nil
		}
	} else {
		st.removeRoots()
		depPrev = nil
	}

	if depPrev != nil {
		depPrev.weightSum -= int32(st.weight)
		root := depPrev.updateNum(-1 * st.n)
		root.updateWeightSumTop()
		root.updateWeightEff()
	}

	st.sibPrev = nil
	st.sibNext = nil
	st.depPrev = nil
}

func (st *stream) becomeSingleRoot() {
	if st.roots.Len() > 0 {
		last := st.roots.Front()
		lastStream := last.Value.(*stream)

		st.weightSum += int32(lastStream.weight)
		st.n += lastStream.n

		prevStream := lastStream
		for s := last.Next(); s != nil; s = s.Next() {
			sib := s.Value.(*stream)
			st.weightSum += int32(sib.weight)
			st.n += sib.n
			prevStream.linkSib(sib)
			prevStream = sib
		}

		if st.depNext != nil {
			sibNext := st.depNext
			sibNext.depPrev = nil
			prevStream.linkSib(sibNext)
			st.linkAddDependent(lastStream)

			// TODO (brk0v): a little ugly
			st.roots.removeAll()
			st.moveRoots()
			return
		} else {
			st.linkAddDependent(lastStream)
		}
	}
	st.roots.removeAll()
	st.addRoots()

	// update dep tree state and weight
	st.resetDepStateReady()
	st.weightEff = int32(st.weight)
	st.setDepStateTop()
	st.updateWeightSumTop()
	st.updateWeightEff()
}

func (st *stream) linkAddDependent(chst *stream) {
	st.depNext = chst
	chst.depPrev = st
}

func (st *stream) linkInsertDependent(chst *stream) {
	sibNext := st.depNext
	chst.linkSib(sibNext)
	sibNext.depPrev = nil
	st.linkAddDependent(chst)
}

func (st *stream) unlinkDependent() {
	depPrev := st.depPrev
	depNext := st.depNext

	if depNext != nil {
		depPrev.linkAddDependent(depNext)

		if st.sibNext != nil {
			lastSib := depNext.lastSib()
			lastSib.linkSib(st.sibNext)
		}
	} else if st.sibNext != nil {
		next := st.sibNext
		next.sibPrev = nil
		depPrev.linkAddDependent(next)
	} else {
		depPrev.depNext = nil
	}
}

func (st *stream) linkSib(next *stream) {
	st.sibNext = next
	next.sibPrev = st
}

func (st *stream) unlinkSib() {
	sibPrev := st.sibPrev
	depNext := st.depNext

	if depNext != nil {
		depNext.depPrev = nil
		sibPrev.linkSib(depNext)

		if st.sibNext != nil {
			lastSib := depNext.lastSib()
			lastSib.linkSib(st.sibNext)
		}
	} else {
		next := st.sibNext
		sibPrev.sibNext = next

		if next != nil {
			next.sibPrev = sibPrev
		}
	}
}

func (st *stream) firstSib() (fsib *stream) {
	for s := st; s != nil; s = s.sibPrev {
		fsib = s
	}
	return fsib
}

func (st *stream) lastSib() (lsib *stream) {
	for s := st; s != nil; s = s.sibNext {
		lsib = s
	}
	return lsib
}

// updateNum updates number of total descendants on d size from stream to its
// root and return root stream.
func (st *stream) updateNum(d int32) *stream {
	st.n += d
	s := st.firstSib()
	if s.depPrev != nil {
		return s.depPrev.updateNum(d)
	}
	return s
}

//findSubTree answers on a question: does chst stream in the subtree of st?.
func (st *stream) isInSubTree(chst *stream, top bool) bool {
	if chst == st {
		return true
	}

	if !top && st.sibNext != nil {
		if st.sibNext.isInSubTree(chst, false) {
			return true
		}
	}

	if st.depNext != nil {
		return st.depNext.isInSubTree(chst, false)
	}

	return false
}

// getRoot gets stream's root.
func (st *stream) getRoot() *stream {
	root := st
	for {
		if root.sibPrev != nil {
			root = root.sibPrev
			continue
		}
		if root.depPrev != nil {
			root = root.depPrev
			continue
		}
		break
	}
	return root
}

// setDepStateIdle sets steam's state to depStateIdle and updates dependency tree.
func (st *stream) setDepStateIdle() {
	st.depState = depStateIdle
	root := st.getRoot()
	root.setDepStateTop()
	root.updateWeightSumTop()
	root.updateWeightEff()
}

// setDepStateTop sets depState to depStateTop starting from stream st.
// Note: we don't update sibs of this stream.
func (st *stream) setDepStateTop() {
	if st.depState == depStateTop {
		return
	}
	if st.depState == depStateReady {
		st.depState = depStateTop
		return
	}
	// going deeper using dfs:
	for s := st.depNext; s != nil; s = s.sibNext {
		s.setDepStateTop()
	}
}

// setDepStateReady sets depState to setDepStateReady and updates dependency tree.
func (st *stream) setDepStateReady() {
	st.depState = depStateReady
	if st.depNext != nil {
		st.depNext.resetDepStateReady()
	}
	root := st.getRoot()
	root.setDepStateTop()
	root.updateWeightSumTop()
	root.updateWeightEff()
}

// setDepStateFlowDefer sets steam's state to depStatFlowDefer and updates dep tree.
func (st *stream) setDepStateFlowDefer() {
	st.depState = depStatFlowDefer
	root := st.getRoot()
	root.setDepStateTop()
	root.updateWeightSumTop()
	root.updateWeightEff()
}

// resetDepStateReady reset depState to depStateReady.
func (st *stream) resetDepStateReady() {
	if st.depState == depStateReady {
		return
	}
	if st.depState == depStateTop {
		st.depState = depStateReady
		if st.sibNext != nil {
			st.sibNext.resetDepStateReady()
		}
		return
	}
	if st.sibNext != nil {
		st.sibNext.resetDepStateReady()
	}
	if st.depNext != nil {
		st.depNext.resetDepStateReady()
	}
}

// schedule add stream to pq in case of depStateTop recursively.
func (st *stream) schedule(ws *writeScheduler) {
	if st.depState == depStateReady {
		return
	}
	if st.depState == depStateTop {
		if q, ok := ws.sq[st.id]; ok && !q.queued {
			q.vf = ws.lvf + q.calcVirtFinish(st.weightEff)
			ws.canSend.push(q)
		}
		return
	}
	// go deeper
	for s := st.depNext; s != nil; s = s.sibNext {
		s.schedule(ws)
	}
}

// updateWeightSumTop updates stream WeightSumTop.
func (st *stream) updateWeightSumTop() bool {
	st.weightSumTop = 0

	if st.depState == depStateTop {
		return true
	}
	if st.depState == depStateReady {
		return false
	}

	rv := false
	for s := st.depNext; s != nil; s = s.sibNext {
		if s.updateWeightSumTop() {
			rv = true
			st.weightSumTop += int32(s.weight)
		}
	}

	return rv
}

// updateWeightEff updates stream updateWeightEff for pq scheduling.
func (st *stream) updateWeightEff() {
	// weightSumTop = 0 means that there is no top stream under
	if st.weightSumTop == 0 || (st.depState != depStateIdle && st.depState != depStatFlowDefer) {
		return
	}

	for s := st.depNext; s != nil; s = s.sibNext {
		if s.depState != depStateReady {
			s.weightEff = st.weightEff * int32(s.weight) / st.weightSumTop
			// min weight 1
			if s.weightEff == 0 {
				s.weightEff = 1
			}
		}
		s.updateWeightEff()
	}
}
