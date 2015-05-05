// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

// Package http2 implements the HTTP/2 protocol.
//
// This is a work in progress. This package is low-level and intended
// to be used directly by very few people. Most users will use it
// indirectly through integration with the net/http package. See
// ConfigureServer. That ConfigureServer call will likely be automatic
// or available via an empty import in the future.
//
// See http://http2.github.io/

package http2

import (
	"container/list"
	"fmt"
)

const (
	defaultWeight                = 16
	maxStreamWeight              = 256
	defaultMaxDependencyTree     = 100
	defaultMaxSavedIdleStreams   = 10
	defaultMaxSavedClosedStreams = 10
)

var defaultPriority = PriorityParam{
	Weight:    defaultWeight - 1, // uint8 and incrementing while prioritizing
	StreamDep: 0,
}

type streamState int

const (
	stateIdle streamState = iota
	stateOpen
	stateHalfClosedLocal
	stateHalfClosedRemote
	stateResvLocal
	stateResvRemote
	stateClosed
)

var stateName = [...]string{
	stateIdle:             "Idle",
	stateOpen:             "Open",
	stateHalfClosedLocal:  "HalfClosedLocal",
	stateHalfClosedRemote: "HalfClosedRemote",
	stateResvLocal:        "ResvLocal",
	stateResvRemote:       "ResvRemote",
	stateClosed:           "Closed",
}

func (st streamState) String() string {
	return stateName[st]
}

type streamDepState int

const (
	depStateIdle      streamDepState = iota // init state without data
	depStateReady                           // ready to send, but blocked by dependency tree
	depStateTop                             // write scheduler can send data
	depStateFlowDefer                       // blocked by stream flow control and waiting for WINDOW_UPDATE
)

var depStateName = [...]string{
	depStateIdle:      "Idle",
	depStateReady:     "Ready",
	depStateTop:       "Top",
	depStateFlowDefer: "FlowDefer",
}

func (ds streamDepState) String() string {
	if 0 <= ds && ds < streamDepState(len(depStateName)) {
		return depStateName[ds]
	}
	return fmt.Sprintf("http2: unknown streamDepState %d", ds)
}

func clientInitiated(id uint32) bool {
	if id%2 == 1 {
		// client initiated
		return true
	}
	// server initiated (push stream)
	return false
}

func serverInitiated(id uint32) bool {
	return !clientInitiated(id)
}

// roots is a list of root streams.
type roots struct {
	*list.List       // doubly linked list of all root streams
	n          int32 // number of streams in the whole dependency tree of this serverConn
}

func newRoots() *roots {
	return &roots{list.New(), 0}
}

// add adds stream and increments n.
func (r *roots) add(st *stream) {
	st.rootElem = r.PushFront(st)
	r.n += 1
}

// move moves stream to roots w/o changing n.
func (r *roots) move(st *stream) {
	st.rootElem = r.PushFront(st)
}

// remove removes stream from roots but doesn't decrement n.
func (r *roots) remove(st *stream) {
	r.Remove(st.rootElem)
	st.rootElem = nil
}

// removeAll removes all streams but doesn't change n.
func (r *roots) removeAll() {
	for r.Len() != 0 {
		rootStream := r.Back().Value.(*stream)
		r.remove(rootStream)
	}
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
	bodyBytes     int64 // body bytes seen so far
	declBodyBytes int64 // or -1 if undeclared
	flow          flow  // limits writing from Handler to client
	inflow        flow  // what the client is allowed to POST/etc to us
	weight        int32
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
	depPrev      *stream
	depNext      *stream
	sibNext      *stream
	sibPrev      *stream
	depState     streamDepState // state in dependency tree
	roots        *roots
	rootElem     *list.Element // root element in the roots
	n            int32         // number of total descendants plus me
	weightSum    int32         // sum of weight of direct descendants
	weightSumTop int32         // sum of weight of direct descendants that have depStateTop in theirs dependency tree (at any level deeper)
	weightEff    int32         // effective weight used for calculating virtual finish for weighted fair queue
	idleElem     *list.Element // idle stream element in serverConn idles
	closedElem   *list.Element // closed stream element in serverConn closeds

}

func newStream(id uint32, sc *serverConn, priority PriorityParam) (*stream, error) {
	sc.serveG.check()

	if state, st := sc.state(id); state == stateIdle && st != nil {
		// Creating stream that has been already created by priority frame
		// see "5.3.4 Prioritization State Management" for details.
		sc.removeIdle(st.idleElem)
	}

	switch {
	case clientInitiated(id) && id > sc.maxStreamID:
		sc.maxStreamID = id
	case serverInitiated(id) && id > sc.maxPushID:
		sc.maxPushID = id
	}

	st := &stream{
		id:       id,
		roots:    sc.roots,
		n:        1,
		depState: depStateIdle,
	}

	err := st.createStreamPriority(sc, priority)
	if err != nil {
		return nil, err
	}

	st.cw.Init()
	st.flow.conn = &sc.flow // link to conn-level counter
	st.flow.add(sc.initialWindowSize)
	st.inflow.conn = &sc.inflow      // link to conn-level counter
	st.inflow.add(initialWindowSize) // TODO: update this when we send a higher initial window size in the initial settings

	sc.streams[id] = st
	return st, nil
}

// for debugging only:
func (st *stream) String() string {
	return fmt.Sprintf("[stream id=%d parent=%d state=%s weight=%d "+
		"weightEff=%d]", st.id, st.parentID(), st.state, st.weight, st.weightEff)
}

// addRoots adds stream to roots.
func (st *stream) addRoots() {
	st.roots.add(st)
}

// moveRoots move stream to roots and update dependency tree.
func (st *stream) moveRoots() {
	st.roots.move(st)
	st.resetDepStateReady()
	st.setDepState(depStateTop)
}

// removeRoots removes stream from roots.
func (st *stream) removeRoots() {
	st.roots.remove(st)
}

// setState sets state of the stream st to streamState and in charge of keeping
// idle and closed stream in theirs limits.
func (st *stream) setState(sc *serverConn, state streamState) {
	sc.serveG.check()
	st.state = state
	switch state {
	case stateIdle:
		// Idle stream as a grouping node for priority purposes.
		st.idleElem = sc.idles.PushFront(st)
		// recycle old ones
		for sc.idles.Len() > sc.maxSavedIdleStreams {
			sc.removeIdle(sc.idles.Back())
		}
	case stateClosed:
		// If stream is push don't retain.
		if serverInitiated(st.id) {
			st.destroyStream(sc)
			return
		}
		// 5.3.4 Prioritization State Management
		//... an endpoint SHOULD retain stream prioritization state for a period
		// after streams become closed. The longer state is retained, the lower
		// the chance that streams are assigned incorrect or default priority
		// values.
		st.closedElem = sc.closeds.PushFront(st)
		// recycle old ones
		for sc.closeds.Len() > sc.maxSavedClosedStreams {
			sc.removeClosed(sc.closeds.Back())
		}
	}
}

// destroyStream finally destroys stream.
func (st *stream) destroyStream(sc *serverConn) {
	st.removeDependent()
	delete(sc.streams, st.id)
}

func (st *stream) createStreamPriority(sc *serverConn, priority PriorityParam) error {
	sc.serveG.check()
	st.weight = int32(priority.Weight) + 1 // calc real weight
	st.weightEff = st.weight
	parentID := priority.StreamDep
	switch parentID {
	case 0:
		if priority.Exclusive {
			if st.roots.n < sc.maxDependencyTree {
				st.becomeSingleRoot()
				return nil
			}
			// TODO (brk0v): test this
			// drop to default weight
			st.weight = defaultWeight - 1 // uint8
			st.weightEff = st.weight
		}
		st.addRoots()
		return nil
	default:
		state, parent := sc.state(parentID)
		switch parent {
		case st:
			// TODO (brk0v): test this
			// A stream cannot depend on itself. An endpoint MUST treat this as
			// a stream error (Section 5.4.2) of type PROTOCOL_ERROR.
			return StreamError{st.id, ErrCodeProtocol}
		case nil:
			switch state {
			case stateIdle:
				// Priority on a idle parent stream
				// 5.3.4 Prioritization State Management
				// ... streams that are in the "idle" state can be assigned priority
				// or become a parent of other streams. This allows for the creation
				// of a grouping node in the dependency tree, which enables more
				// flexible expressions of priority. Idle streams begin with a
				// default priority (Section 5.3.5)
				if serverInitiated(parentID) {
					// Trying to create grouping node with pushed id.
					return nil
				}
				var err error
				parent, err = newStream(parentID, sc, defaultPriority)
				if err != nil {
					return err
				}
				// Set stateIdle and don't increment concurrent counter.
				parent.setState(sc, stateIdle)
			case stateClosed:
				// We've alredy deleted this stream because of maxSavedClosedStreams,
				// so drop stream's priority to default.
				st.weight = defaultWeight - 1 // uint8
				st.weightEff = st.weight
				st.addRoots()
				return nil
			}
		}
		root := st.getRoot()
		if root.n < sc.maxDependencyTree {
			if priority.Exclusive {
				parent.insertDependent(st)
			} else {
				parent.addDependent(st)
			}
		} else {
			// TODO (brk0v): test this
			st.weight = defaultWeight - 1 // uint8
			st.weightEff = st.weight
			st.addRoots()
		}
	}
	return nil
}

func (st *stream) adjustStreamPriority(sc *serverConn, priority PriorityParam) error {
	sc.serveG.check()
	weight := int32(priority.Weight) + 1 // calc real weight
	parentID := priority.StreamDep
	switch parentID {
	case 0:
		st.removeDependentSubTree()
		st.weight = weight
		st.weightEff = st.weight
		if priority.Exclusive {
			if st.roots.n < sc.maxDependencyTree {
				st.becomeSingleRoot()
				return nil
			}
			// TODO (brk0v): test this
			// drop to default weight
			st.weight = defaultWeight - 1 // uint8
			st.weightEff = st.weight
		}
		st.moveRoots()
		return nil
	default:
		state, parent := sc.state(parentID)
		switch parent {
		case st:
			// TODO (brk0v): test this
			// A stream cannot depend on itself. An endpoint MUST treat this as
			// a stream error (Section 5.4.2) of type PROTOCOL_ERROR.
			return StreamError{st.id, ErrCodeProtocol}
		case nil:
			switch state {
			case stateIdle:
				// Priority on a idle parent stream
				// 5.3.4 Prioritization State Management
				// ... streams that are in the "idle" state can be assigned priority
				// or become a parent of other streams. This allows for the creation
				// of a grouping node in the dependency tree, which enables more
				// flexible expressions of priority. Idle streams begin with a
				// default priority (Section 5.3.5)
				if serverInitiated(parentID) {
					// Trying to create grouping node with pushed id.
					return nil
				}
				var err error
				parent, err = newStream(parentID, sc, defaultPriority)
				if err != nil {
					return err
				}
				// Set stateIdle and don't increment concurrent counter.
				parent.setState(sc, stateIdle)
			case stateClosed:
				// We've alredy deleted this stream because of maxSavedClosedStreams,
				// so drop stream's priority to default.
				st.removeDependentSubTree()
				st.weight = defaultWeight - 1 // uint8
				st.weightEff = st.weight
				st.moveRoots()
				return nil
			}
		}
		if st.isInSubTree(parent, true) {
			// 5.3.3 Reprioritization
			// ...
			// If a stream is made dependent on one of its own dependencies, the
			// formerly dependent stream is first moved to be dependent on the
			// reprioritized stream's previous parent. The moved dependency
			// retains its weight.
			// TODO (brk0v): sc.maxDependencyTree checks?
			parent.removeDependentSubTree()
			first := st.firstSib()
			if first.depPrev != nil {
				first.depPrev.addDependentSubTree(parent)
			} else {
				st.weightEff = st.weight
				parent.moveRoots()
			}
		}
		st.removeDependentSubTree()
		st.weight = weight
		root := st.getRoot()
		if (root.n + st.n) < sc.maxDependencyTree {
			if priority.Exclusive {
				parent.insertDependentSubTree(st)
			} else {
				parent.addDependentSubTree(st)
			}
		} else {
			// TODO (brk0v): test this
			st.weight = defaultWeight - 1 // uint8
			st.weightEff = st.weight
			st.moveRoots()
		}
	}
	return nil
}

// addDependent adds dependentcy to parent stream st.
func (st *stream) addDependent(chst *stream) {
	st.weightSum += chst.weight

	if st.depNext == nil {
		st.linkAddDependent(chst)
	} else {
		st.linkInsertDependent(chst)
	}

	root := st.updateNum(1)
	root.updateScheduleWeight()
	chst.roots.n += 1
}

// insertDependent inserts exclusive dependent stream chst to parent st.
func (st *stream) insertDependent(chst *stream) {
	chst.weightSum = st.weightSum
	st.weightSum = chst.weight

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
	root.updateScheduleWeight()
	chst.roots.n += 1
}

// add sub tree
func (st *stream) addDependentSubTree(chst *stream) {
	chst.resetDepStateReady()

	if st.depNext == nil {
		st.weightSum = chst.weight
		st.linkAddDependent(chst)
	} else {
		st.weightSum += chst.weight
		st.linkInsertDependent(chst)
	}

	root := st.updateNum(chst.n)
	root.setDepState(depStateTop)
}

func (st *stream) insertDependentSubTree(chst *stream) {
	d := chst.n
	chst.resetDepStateReady()

	if st.depNext != nil {
		chst.n += st.n - 1
		chst.weightSum += st.weightSum
		st.weightSum = chst.weight

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
		st.weightSum = chst.weight
	}

	root := st.updateNum(d)
	root.setDepState(depStateTop)
}

func (st *stream) removeDependent() {
	var root *stream
	delta := -st.weight

	// Distibute weight.
	for s := st.depNext; s != nil; s = s.sibNext {
		s.weight = st.weight * s.weight / st.weightSum
		if s.weight == 0 {
			s.weight = 1
		}
		delta += s.weight
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
		// Stream is a root.
		st.removeRoots()

		// sibs become roots.
		for s := st.depNext; s != nil; s = s.sibNext {
			s.depPrev = nil
			s.sibPrev = nil
			s.sibNext = nil

			s.weightEff = s.weight
			s.moveRoots()
		}
	}

	if root != nil {
		root.updateScheduleWeight()
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
		depPrev.weightSum -= st.weight
		root := depPrev.updateNum(-1 * st.n)
		root.updateScheduleWeight()
	}

	st.sibPrev = nil
	st.sibNext = nil
	st.depPrev = nil
}

func (st *stream) becomeSingleRoot() {
	if st.roots.Len() > 0 {
		last := st.roots.Front()
		lastStream := last.Value.(*stream)

		st.weightSum += lastStream.weight
		st.n += lastStream.n

		prevStream := lastStream
		for s := last.Next(); s != nil; s = s.Next() {
			sib := s.Value.(*stream)
			st.weightSum += sib.weight
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
			st.weightEff = st.weight
			st.moveRoots()
			return
		} else {
			st.linkAddDependent(lastStream)
		}
	}
	st.roots.removeAll()
	st.addRoots()

	// Update dependency tree state and weight.
	st.resetDepStateReady()
	st.weightEff = st.weight
	st.setDepState(depStateTop)
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

func (st *stream) parent() *stream {
	s := st.firstSib()
	return s.depPrev
}

func (st *stream) parentID() uint32 {
	if parent := st.parent(); parent != nil {
		return parent.id
	}
	return 0
}

// updateNum updates number of total descendants on d size from the st stream
// to its root and returns this root stream.
func (st *stream) updateNum(d int32) *stream {
	st.n += d
	s := st.firstSib()
	if s.depPrev != nil {
		return s.depPrev.updateNum(d)
	}
	return s
}

// isInSubTree answers on a question: does chst stream locate in the subtree of
// the st stream.
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

// setDepState sets depState and update scheduler weights.
func (st *stream) setDepState(depState streamDepState) {

	switch depState {
	case depStateIdle, depStateFlowDefer:
		// In idle and blocked by stream flow.
		st.depState = depState
	case depStateReady:
		// Ready to send data.
		st.depState = depState
		if st.depNext != nil {
			st.depNext.resetDepStateReady()
		}
	case depStateTop:
		// Find and set top for scheduling.
		switch st.depState {
		case depStateTop:
			return
		case depStateReady:
			st.depState = depStateTop
			return
		}
		// Going deeper using dfs.
		for s := st.depNext; s != nil; s = s.sibNext {
			s.setDepState(depStateTop)
		}
	}

	// Update dependency tree.
	switch depState {
	case depStateIdle, depStateFlowDefer, depStateReady:
		root := st.getRoot()
		root.setDepState(depStateTop)
	case depStateTop:
		if st.rootElem != nil {
			// Updates sheduling weight starting from root.
			st.updateScheduleWeight()
		}
	}
}

// resetDepStateReady recursively resets depState to depStateReady for stream's
// dependecy tree.
func (st *stream) resetDepStateReady() {
	switch st.depState {
	case depStateReady:
		return
	case depStateTop:
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
	switch st.depState {
	case depStateReady:
		return
	case depStateTop:
		if q, ok := ws.sq[st.id]; ok && !q.queued {
			q.vf = ws.lvf + q.calcVirtFinish(st.weightEff)
			ws.canSend.push(q)
		}
		return
	}
	// Go deeper.
	for s := st.depNext; s != nil; s = s.sibNext {
		s.schedule(ws)
	}
}

// updateScheduleWeight updates weights (weightSumTop and weightEff) needed for
// scheduling streams.
func (st *stream) updateScheduleWeight() {
	st.updateWeightSumTop()
	st.updateWeightEff()
}

// updateWeightSumTop updates stream WeightSumTop.
func (st *stream) updateWeightSumTop() bool {
	st.weightSumTop = 0

	switch st.depState {
	case depStateTop:
		return true
	case depStateReady:
		return false
	}

	rv := false
	for s := st.depNext; s != nil; s = s.sibNext {
		if s.updateWeightSumTop() {
			rv = true
			st.weightSumTop += s.weight
		}
	}
	return rv
}

// updateWeightEff updates stream updateWeightEff for pq scheduling.
func (st *stream) updateWeightEff() {
	// weightSumTop = 0 means that there is no top streams below.
	if st.weightSumTop == 0 ||
		(st.depState != depStateIdle && st.depState != depStateFlowDefer) {
		return
	}
	for s := st.depNext; s != nil; s = s.sibNext {
		if s.depState != depStateReady {
			s.weightEff = st.weightEff * s.weight / st.weightSumTop
			// min weight 1
			if s.weightEff == 0 {
				s.weightEff = 1
			}
		}
		s.updateWeightEff()
	}
}
