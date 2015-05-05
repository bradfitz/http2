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
	"container/heap"
	"fmt"
)

type Item struct {
	v  *writeQueue
	id uint32 // see writeScheduler's take()
}
type priorityQueue []*Item

func newPriorityQueue() *priorityQueue {
	pq := new(priorityQueue)
	heap.Init(pq)
	return pq
}

// for debug: listing all items in pq with details.
func (pq priorityQueue) String() string {
	var vf int32
	var itemID, streamID, parentID uint32
	ret := "[priorityQueue Items="
	for i, item := range pq {
		st := item.v.stream()
		itemID = item.id
		if st != nil {
			streamID = st.id
			parentID = st.parentID()
			vf = item.v.vf
		}
		ret += fmt.Sprintf("\n\t[Item i=%d, id=%d, stream=%d, parent=%d, vf=%v]", i, itemID, streamID, parentID, vf)
	}
	return fmt.Sprintf("%s]", ret)
}

func (pq priorityQueue) Len() int { return len(pq) }

func (pq priorityQueue) Less(i, j int) bool {
	if pq.Len() == 0 {
		return true
	}
	return pq[i].v.vf < pq[j].v.vf
}

func (pq priorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
}

func (pq *priorityQueue) Push(x interface{}) {
	item := x.(*Item)
	*pq = append(*pq, item)
}

func (pq *priorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	*pq = old[0 : n-1]
	return item
}

func (pq *priorityQueue) push(q *writeQueue) {
	heap.Push(pq, &Item{v: q, id: q.streamID()})
	q.queued = true
}

func (pq *priorityQueue) pop() (*writeQueue, uint32) {
	item := heap.Pop(pq).(*Item)
	q := item.v
	q.queued = false
	return q, item.id
}

func (pq *priorityQueue) len() int {
	return pq.Len()
}
