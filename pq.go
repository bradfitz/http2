// This example demonstrates a priority queue built using the heap interface.
package http2

import (
	"container/heap"
)

type Item struct {
	v *writeQueue
}

type priorityQueue []*Item

func newPriorityQueue() *priorityQueue {
	pq := new(priorityQueue)
	heap.Init(pq)
	return pq
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
	heap.Push(pq, &Item{v: q})
	q.queued = true
}

func (pq *priorityQueue) pop() *writeQueue {
	q := heap.Pop(pq).(*Item).v
	q.queued = false
	return q
}

func (pq *priorityQueue) len() int {
	return pq.Len()
}
