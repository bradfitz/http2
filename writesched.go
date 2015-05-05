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
	"fmt"
	"log"
)

var VerboseSheduler = false

// frameWriteMsg is a request to write a frame.
type frameWriteMsg struct {
	// write is the interface value that does the writing, once the
	// writeScheduler (below) has decided to select this frame
	// to write. The write functions are all defined in write.go.
	write writeFramer

	stream *stream // used for prioritization. nil for non-stream frames.

	// done, if non-nil, must be a buffered channel with space for
	// 1 message and is sent the return value from write (or an
	// earlier error) when the frame has been written.
	done chan error
}

// for debugging only:
func (wm frameWriteMsg) String() string {
	var streamID, parentID uint32
	if wm.stream != nil {
		streamID = wm.stream.id
		parentID = wm.stream.parentID()
	}
	var des string
	if s, ok := wm.write.(fmt.Stringer); ok {
		des = s.String()
	} else {
		des = fmt.Sprintf("%T", wm.write)
	}
	return fmt.Sprintf("[frameWriteMsg stream=%d, parent=%d, ch=%v, type: %v]", streamID, parentID, wm.done != nil, des)
}

func (wm frameWriteMsg) len() int {
	if wd, ok := wm.write.(*writeData); ok {
		return len(wd.p)
	}
	panic("internal error: getting len on not writeData")

}

// writeScheduler tracks pending frames to write, priorities, and decides
// the next one to use. It is not thread-safe.
type writeScheduler struct {
	// zero are frames not associated with a specific stream.
	// They're sent before any stream-specific freams.
	zero writeQueue

	// maxFrameSize is the maximum size of a DATA frame
	// we'll write. Must be non-zero and between 16K-16M.
	maxFrameSize uint32

	// sq contains the stream-specific queues, keyed by stream ID.
	// when a stream is idle, it's deleted from the map.
	sq map[uint32]*writeQueue

	// canSend is a priority queue that's reused between frame
	// scheduling decisions to hold the list of writeQueues (from sq)
	// which are not blocked by other streams by dependeny tree.
	// canSend is updated on every write scheduler take() call.
	canSend *priorityQueue

	// pool of empty queues for reuse.
	queuePool []*writeQueue

	// double linked list of roots streams.
	roots *roots

	// last virtual finish for weighted fair queuing algorithm.
	lvf int32
}

func newWriteScheduler(roots *roots) writeScheduler {
	return writeScheduler{
		maxFrameSize: initialMaxFrameSize,
		roots:        roots,
		canSend:      newPriorityQueue(),
	}
}

func (ws *writeScheduler) vlogf(format string, args ...interface{}) {
	if VerboseSheduler {
		log.Printf(format, args...)
	}
}

func (ws *writeScheduler) putEmptyQueue(q *writeQueue) {
	if len(q.s) != 0 {
		panic("queue must be empty")
	}
	ws.queuePool = append(ws.queuePool, q)
}

func (ws *writeScheduler) getEmptyQueue() *writeQueue {
	ln := len(ws.queuePool)
	if ln == 0 {
		return new(writeQueue)
	}
	q := ws.queuePool[ln-1]
	ws.queuePool = ws.queuePool[:ln-1]
	return q
}

func (ws *writeScheduler) empty() bool { return ws.zero.empty() && len(ws.sq) == 0 }

func (ws *writeScheduler) add(wm frameWriteMsg) {
	st := wm.stream
	if st == nil {
		ws.zero.push(wm)
	} else {
		ws.streamQueue(st.id).push(wm)
		// Update dependency tree depState in case of adding DATA frames.
		if _, ok := wm.write.(*writeData); ok {
			st.setDepState(depStateReady)
			ws.vlogf("writeScheduler add DATA: %s state: %s\n", wm, st.depState)
		}
	}
}

func (ws *writeScheduler) streamQueue(streamID uint32) *writeQueue {
	if q, ok := ws.sq[streamID]; ok {
		return q
	}
	if ws.sq == nil {
		ws.sq = make(map[uint32]*writeQueue)
	}
	q := ws.getEmptyQueue()
	ws.sq[streamID] = q
	return q
}

// take returns the most important frame to write and removes it from the scheduler.
// It is illegal to call this if the scheduler is empty or if there are no connection-level
// flow control bytes available.
func (ws *writeScheduler) take() (wm frameWriteMsg, ok bool) {
	if ws.maxFrameSize == 0 {
		panic("internal error: ws.maxFrameSize not initialized or invalid")
	}

	// If there any frames not associated with streams, prefer those first.
	// These are usually SETTINGS, etc.
	if !ws.zero.empty() {
		return ws.zero.shift(), true
	}
	if len(ws.sq) == 0 {
		return
	}
	// Next, prioritize frames on streams that aren't DATA frames (no cost).
	for id, q := range ws.sq {
		if q.firstIsNoCost() {
			return ws.takeFrom(id, q)
		}
	}

	// Update canSend pq.
	for re := ws.roots.Front(); re != nil; re = re.Next() {
		root := re.Value.(*stream)
		root.schedule(ws)
	}

	for ws.canSend.len() > 0 {
		ws.vlogf("writeScheduler canSend: %s", ws.canSend)
		q, id := ws.canSend.pop()
		st := q.stream()

		// Checking for stale data in pq. There are at least two cases:
		//  * writeQueue's s is niled because of forgetStream() call (e.g. got RST);
		//  * writeQueue was reused by getEmptyQueue() with another stream id.
		if st == nil || q.streamID() != id {
			continue
		}

		// We have higher priority data to sent. This protects us from sending
		// old stale data after reprioritization and rebuilding dependency tree.
		if st.depState != depStateTop {
			continue
		}

		// Check flow control.
		n := ws.streamWritableBytes(q)
		switch {
		case n > 0:
			wm, ok = ws.takeFrom(id, q)
			if ok {
				// Update scheduler's last virt finish.
				ws.lvf += int32(wm.len()) * maxStreamWeight / st.weightEff
				// If we have more data then queue it.
				if !q.empty() {
					q.vf = ws.lvf + q.calcVirtFinish(st.weightEff)
					ws.canSend.push(q)
				} else {
					// No data to send and stream is alive so just unblock
					// dependency tree.
					st.setDepState(depStateIdle)
				}
			}
			return wm, ok
		default:
			if st.flow.availableConn() == 0 {
				// Don't have enough connection flow: so put stream queue back
				// in the pq without virtual last recalculation.
				ws.canSend.push(q)
				return
			} else {
				// Don't have enough stream flow: unblock dependency tree and
				// start waiting for WINDOW_UPDATE frame.
				st.setDepState(depStateFlowDefer)
				return
			}
		}
	}
	return
}

// streamWritableBytes returns the number of DATA bytes we could write
// from the given queue's stream, if this stream/queue were
// selected. It is an error to call this if q's head isn't a
// *writeData.
func (ws *writeScheduler) streamWritableBytes(q *writeQueue) int32 {
	wm := q.head()
	ret := wm.stream.flow.available() // max we can write
	if ret == 0 {
		return 0
	}
	if int32(ws.maxFrameSize) < ret {
		ret = int32(ws.maxFrameSize)
	}
	if ret == 0 {
		panic("internal error: ws.maxFrameSize not initialized or invalid")
	}
	wd := wm.write.(*writeData)
	if len(wd.p) < int(ret) {
		ret = int32(len(wd.p))
	}
	return ret
}

func (ws *writeScheduler) takeFrom(id uint32, q *writeQueue) (wm frameWriteMsg, ok bool) {
	wm = q.head()
	// If the first item in this queue costs flow control tokens
	// and we don't have enough, write as much as we can.
	if wd, ok := wm.write.(*writeData); ok && len(wd.p) > 0 {
		allowed := wm.stream.flow.available() // max we can write
		if allowed == 0 {
			// No quota available. Caller can try the next stream.
			return frameWriteMsg{}, false
		}
		if int32(ws.maxFrameSize) < allowed {
			allowed = int32(ws.maxFrameSize)
		}
		// TODO: further restrict the allowed size, because even if
		// the peer says it's okay to write 16MB data frames, we might
		// want to write smaller ones to properly weight competing
		// streams' priorities.

		if len(wd.p) > int(allowed) {
			wm.stream.flow.take(allowed)
			chunk := wd.p[:allowed]
			wd.p = wd.p[allowed:]
			// Make up a new write message of a valid size, rather
			// than shifting one off the queue.
			return frameWriteMsg{
				stream: wm.stream,
				write: &writeData{
					streamID: wd.streamID,
					p:        chunk,
					// even if the original had endStream set, there
					// arebytes remaining because len(wd.p) > allowed,
					// so we know endStream is false:
					endStream: false,
				},
				// our caller is blocking on the final DATA frame, not
				// these intermediates, so no need to wait:
				done: nil,
			}, true
		}
		wm.stream.flow.take(int32(len(wd.p)))
	}

	q.shift()
	if q.empty() {
		ws.putEmptyQueue(q)
		delete(ws.sq, id)
	}
	return wm, true
}

func (ws *writeScheduler) forgetStream(id uint32) {
	ws.vlogf("writeScheduler forgetStream: %d\n", id)
	q, ok := ws.sq[id]
	if !ok {
		return
	}
	delete(ws.sq, id)

	// But keep it for others later.
	for i := range q.s {
		q.s[i] = frameWriteMsg{}
	}
	// drop to default
	q.s = q.s[:0]
	q.vf = 0
	q.queued = false
	ws.putEmptyQueue(q)
}

type writeQueue struct {
	s      []frameWriteMsg
	vf     int32 // virtual finish for weighted fair queuing algorithm
	queued bool  // is this writeQueue in pq?
}

// for debugging only:
func (q writeQueue) String() string {
	return fmt.Sprintf("[writeQueue stream=%d, vf=%v]", q.streamID(), q.vf)
}

func (q writeQueue) calcVirtFinish(w int32) int32 {
	return int32(q.head().len()) * maxStreamWeight / w
}

// streamID returns the stream ID for a stream-specific queue.
func (q *writeQueue) streamID() uint32 {
	if st := q.stream(); st != nil {
		return st.id
	}
	return 0
}

// stream returns the stream for a stream-specific queue. It can be nil.
func (q *writeQueue) stream() *stream {
	if len(q.s) > 0 {
		return q.s[0].stream
	}
	return nil
}

func (q *writeQueue) empty() bool { return len(q.s) == 0 }

func (q *writeQueue) push(wm frameWriteMsg) {
	q.s = append(q.s, wm)
}

// head returns the next item that would be removed by shift.
func (q *writeQueue) head() frameWriteMsg {
	if len(q.s) == 0 {
		panic("invalid use of queue")
	}
	return q.s[0]
}

func (q *writeQueue) shift() frameWriteMsg {
	if len(q.s) == 0 {
		panic("invalid use of queue")
	}
	wm := q.s[0]
	// TODO: less copy-happy queue.
	copy(q.s, q.s[1:])
	q.s[len(q.s)-1] = frameWriteMsg{}
	q.s = q.s[:len(q.s)-1]
	return wm
}

func (q *writeQueue) firstIsNoCost() bool {
	if df, ok := q.s[0].write.(*writeData); ok {
		return len(df.p) == 0
	}
	return true
}
