// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package http2

import (
	"runtime"
	"testing"
)

type rootTest struct {
	roots               *roots  // roots struct
	rootFirst, rootLast *stream // first and last root streams
	l                   int     // number of elements in roots
	n                   int32   // number of streams in dependency tree
}

func testRoots(t *testing.T, r rootTest) {
	_, file, line, _ := runtime.Caller(1)
	if r.roots.Front().Value.(*stream) != r.rootFirst {
		t.Errorf("%s:%d", file, line)
		t.Errorf("Roots First element error. ")
		t.Errorf("Expect: %d get: %d", r.rootFirst.id, r.roots.Front().Value.(*stream).id)
	}
	if r.roots.Back().Value.(*stream) != r.rootLast {
		t.Errorf("%s:%d", file, line)
		t.Errorf("Roots Last element error. ")
		t.Errorf("Expect: %d get: %d", r.rootLast.id, r.roots.Back().Value.(*stream).id)
	}
	if r.roots.Len() != r.l {
		t.Errorf("%s:%d", file, line)
		t.Errorf("Roots Len error. Expect: %d get: %d", r.l, r.roots.Len())
	}
	if r.roots.n != r.n {
		t.Errorf("%s:%d", file, line)
		t.Errorf("Roots streams count error. Expect: %d get: %d", r.n, r.roots.n)
	}
}

type streamTest struct {
	st               *stream // stream for test
	depPrev, depNext *stream
	sibPrev, sibNext *stream
	n                int32 // number streams in subtree
	weight           int32
	weightSum        int32 // sum of weights of direct descendants (children)
}

func sumWeight(streams ...*stream) (sum int32) {
	for _, s := range streams {
		sum += s.weight
	}
	return
}

func testStreamNode(t *testing.T, streamTests []streamTest) {

	for _, s := range streamTests {
		_, file, line, _ := runtime.Caller(1)
		if s.st.depPrev != s.depPrev || s.st.depNext != s.depNext || s.st.sibPrev != s.sibPrev || s.st.sibNext != s.sibNext {
			t.Errorf("%s:%d", file, line)
			t.Errorf("Stream id: %d", s.st.id)
			t.Errorf("        Expect  Get")
			t.Errorf("depPrev: %5d %5d", s.depPrev.id, s.st.depPrev.id)
			t.Errorf("depNext: %5d %5d", s.depNext.id, s.st.depNext.id)
			t.Errorf("sibPrev: %5d %5d", s.sibPrev.id, s.st.sibPrev.id)
			t.Errorf("sibNext: %5d %5d", s.sibNext.id, s.st.sibNext.id)
		}
		if s.st.n != s.n {
			t.Errorf("%s:%d", file, line)
			t.Errorf("Stream id: %d", s.st.id)
			t.Errorf("   Expect  Get")
			t.Errorf("n: %5d %5d\n", s.n, s.st.n)
		}
		if s.st.weightSum != s.weightSum {
			t.Errorf("%s:%d", file, line)
			t.Errorf("Stream id: %d", s.st.id)
			t.Errorf("         Expect  Get")
			t.Errorf("weightSum: %5d %5d", s.weightSum, s.st.weightSum)
		}
		if s.st.weight-1 != s.weight {
			t.Errorf("%s:%d", file, line)
			t.Errorf("Stream id: %d", s.st.id)
			t.Errorf("       Expect  Get")
			t.Errorf("weight: %5d %5d", s.weight, s.st.weight-1)
		}
	}
}

/*
 * Tests Remove Stream from dependency tree
 * ========================================
 *
 * - update roots
 * - update weightSum
 * - update depPrev, depNext, sibPrev, sibNext
 * - num of streams in dependency tree
 */

func TestRemoveDependent(t *testing.T) {
	sc := &serverConn{
		roots:                 newRoots(),
		streams:               make(map[uint32]*stream),
		serveG:                newGoroutineLock(),
		maxDependencyTree:     defaultMaxDependencyTree,
		maxSavedClosedStreams: defaultMaxSavedClosedStreams,
		maxSavedIdleStreams:   defaultMaxSavedIdleStreams,
	}

	/* Initial dependency:
	 *
	 *  l(23)  e(9)   b(3)       _______a(1)_____
	 *          |               /        |       \
	 *         d(7)           h(15)     k(21)    c(5)
	 *          |            /   \
	 *         f(11)       i(17) g(13)
	 *                            |
	 *                           j(19)
	 */

	a, _ := newStream(1, sc, defaultPriority)
	b, _ := newStream(3, sc, PriorityParam{
		Weight: 120,
	})
	e, _ := newStream(9, sc, PriorityParam{
		StreamDep: 0,
		Weight:    190,
	})
	c, _ := newStream(5, sc, PriorityParam{
		StreamDep: 1,
		Weight:    220,
	})
	k, _ := newStream(21, sc, PriorityParam{
		StreamDep: 1,
		Weight:    19,
	})
	h, _ := newStream(15, sc, PriorityParam{
		StreamDep: 1,
		Weight:    76,
	})
	d, _ := newStream(7, sc, PriorityParam{
		StreamDep: 9,
		Weight:    12,
	})
	l, _ := newStream(23, sc, PriorityParam{
		Weight: 254,
	})
	g, _ := newStream(13, sc, PriorityParam{
		StreamDep: 15,
		Weight:    3,
	})
	i, _ := newStream(17, sc, PriorityParam{
		StreamDep: 15,
		Weight:    89,
	})
	f, _ := newStream(11, sc, PriorityParam{
		StreamDep: 7,
		Weight:    201,
	})
	j, _ := newStream(19, sc, PriorityParam{
		StreamDep: 13,
		Weight:    75,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: l,
		rootLast:  a,
		l:         4,
		n:         12,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   nil,
			depNext:   h,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(h, k, c),
			n:         7,
		},
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    120,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        c,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   k,
			sibNext:   nil,
			weight:    220,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        d,
			depPrev:   e,
			depNext:   f,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    12,
			weightSum: sumWeight(f),
			n:         2,
		},
		streamTest{
			st:        e,
			depPrev:   nil,
			depNext:   d,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    190,
			weightSum: sumWeight(d),
			n:         3,
		},
		streamTest{
			st:        f,
			depPrev:   d,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    201,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        g,
			depPrev:   nil,
			depNext:   j,
			sibPrev:   i,
			sibNext:   nil,
			weight:    3,
			weightSum: sumWeight(j),
			n:         2,
		},
		streamTest{
			st:        h,
			depPrev:   a,
			depNext:   i,
			sibPrev:   nil,
			sibNext:   k,
			weight:    76,
			weightSum: sumWeight(i, g),
			n:         4,
		},
		streamTest{
			st:        i,
			depPrev:   h,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   g,
			weight:    89,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   g,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    75,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        k,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   h,
			sibNext:   c,
			weight:    19,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        l,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    254,
			weightSum: 0,
			n:         1,
		},
	})

	/* Remove root b(3) without dependencies:
	 *
	 *  l(23)  e(9)              _______a(1)_____
	 *          |               /        |       \
	 *         d(7)           h(15)     k(21)    c(5)
	 *          |            /   \
	 *         f(11)       i(17) g(13)
	 *                            |
	 *                           j(19)
	 */

	b.removeDependent()

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: l,
		rootLast:  a,
		l:         3,
		n:         11,
	})

	/* Remove root e(9) with dep:
	 *
	 *         d(7)    l(23)     _______a(1)_____
	 *          |               /        |       \
	 *        f(11)           h(15)     k(21)    c(5)
	 *                        /   \
	 *                     i(17) g(13)
	 *                            |
	 *                           j(19)
	 *
	 */

	e.removeDependent()

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: d,
		rootLast:  a,
		l:         3,
		n:         10,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        d,
			depPrev:   nil,
			depNext:   f,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    190, // distribute weight
			weightSum: sumWeight(f),
			n:         2,
		},
		streamTest{
			st:        f,
			depPrev:   d,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    201,
			weightSum: 0,
			n:         1,
		},
	})

	/* Remove h(15):
	 *
	 *         d(7)    l(23)      ______a(1)______
	 *          |                /     |    |     \
	 *        f(11)            i(17) g(13) k(21) c(5)
	 *                                 |
	 *                               j(19)
	 */

	h.removeDependent()

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: d,
		rootLast:  a,
		l:         3,
		n:         9,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   nil,
			depNext:   i,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(i, g, k, c),
			n:         6,
		},
		streamTest{
			st:        c,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   k,
			sibNext:   nil,
			weight:    220,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        d,
			depPrev:   nil,
			depNext:   f,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    190, // distribute weight
			weightSum: sumWeight(f),
			n:         2,
		},
		streamTest{
			st:        f,
			depPrev:   d,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    201,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        g,
			depPrev:   nil,
			depNext:   j,
			sibPrev:   i,
			sibNext:   k,
			weight:    2,
			weightSum: sumWeight(j),
			n:         2,
		},
		streamTest{
			st:        i,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   g,
			weight:    72,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   g,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    75,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        k,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   g,
			sibNext:   c,
			weight:    19,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        l,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    254,
			weightSum: 0,
			n:         1,
		},
	})

	/* Remove i(17):
	 *
	 *         d(7)    l(23)       ___a(1)__
	 *          |                 /    |    \
	 *        f(11)             g(13) k(21) c(5)
	 *                           |
	 *                          j(19)
	 */

	i.removeDependent()

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: d,
		rootLast:  a,
		l:         3,
		n:         8,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   nil,
			depNext:   g,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(g, k, c),
			n:         5,
		},
		streamTest{
			st:        c,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   k,
			sibNext:   nil,
			weight:    220,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        d,
			depPrev:   nil,
			depNext:   f,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    190, // distribute weight
			weightSum: sumWeight(f),
			n:         2,
		},
		streamTest{
			st:        f,
			depPrev:   d,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    201,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        g,
			depPrev:   a,
			depNext:   j,
			sibPrev:   nil,
			sibNext:   k,
			weight:    2,
			weightSum: sumWeight(j),
			n:         2,
		},
		streamTest{
			st:        j,
			depPrev:   g,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    75,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        k,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   g,
			sibNext:   c,
			weight:    19,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        l,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    254,
			weightSum: 0,
			n:         1,
		},
	})

	/* Remove k(21):
	 *
	 *         d(7)    l(23)       a(1)
	 *          |                 /   \
	 *        f(11)             g(13) c(5)
	 *                           |
	 *                          j(19)
	 */

	k.removeDependent()

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: d,
		rootLast:  a,
		l:         3,
		n:         7,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   nil,
			depNext:   g,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(g, c),
			n:         4,
		},
		streamTest{
			st:        c,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   g,
			sibNext:   nil,
			weight:    220,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        d,
			depPrev:   nil,
			depNext:   f,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    190, // distribute weight
			weightSum: sumWeight(f),
			n:         2,
		},
		streamTest{
			st:        f,
			depPrev:   d,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    201,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        g,
			depPrev:   a,
			depNext:   j,
			sibPrev:   nil,
			sibNext:   c,
			weight:    2,
			weightSum: sumWeight(j),
			n:         2,
		},
		streamTest{
			st:        j,
			depPrev:   g,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    75,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        l,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    254,
			weightSum: 0,
			n:         1,
		},
	})

	/* Add m(25) dependent on a(1) and n(27) depend on g(13) :
	 *
	 *         d(7)    l(23)       __a(1)_
	 *          |                 /   |   \
	 *        f(11)            m(25) g(13) c(5)
	 *                               /  \
	 *                            n(27) j(19)
	 *
	 * and remove g(13):
	 *
	 *         d(7)    l(23)       ______a(1)____
	 *          |                 /   |     |    \
	 *        f(11)            m(25) n(27) j(19) c(5)
	 */

	m, _ := newStream(25, sc, PriorityParam{
		StreamDep: 1,
		Weight:    99,
	})
	n, _ := newStream(27, sc, PriorityParam{
		StreamDep: 13,
		Weight:    43,
	})

	g.removeDependent()

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: d,
		rootLast:  a,
		l:         3,
		n:         8,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   nil,
			depNext:   m,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(m, n, j, c),
			n:         5,
		},
		streamTest{
			st:        d,
			depPrev:   nil,
			depNext:   f,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    190, // distribute weight
			weightSum: sumWeight(f),
			n:         2,
		},
		streamTest{
			st:        f,
			depPrev:   d,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    201,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   n,
			sibNext:   c,
			weight:    0, // distributed weight
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        l,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    254,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        m,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   n,
			weight:    99,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        n,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   m,
			sibNext:   j,
			weight:    0, // distributed weight
			weightSum: 0,
			n:         1,
		},
	})

	/*
	 * Check all deleted stream
	 */

	testStreamNode(t, []streamTest{
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    120,
			weightSum: 0,
			n:         1,
		},

		streamTest{
			st:        e,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    190,
			weightSum: 0,
			n:         1,
		},

		streamTest{
			st:        h,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    76,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        i,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    72,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        k,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    19,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        g,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    2,
			weightSum: 0,
			n:         1,
		},
	})
}

/*
 * Tests Create Stream with Priority
 * =================================
 *
 * - update roots
 * - update weightSum
 * - update depPrev, depNext, sibPrev, sibNext
 * - num of streams in dependency tree
 */

func TestCreateStreamPriority(t *testing.T) {
	sc := &serverConn{
		roots:                 newRoots(),
		streams:               make(map[uint32]*stream),
		serveG:                newGoroutineLock(),
		maxDependencyTree:     defaultMaxDependencyTree,
		maxSavedClosedStreams: defaultMaxSavedClosedStreams,
		maxSavedIdleStreams:   defaultMaxSavedIdleStreams,
	}

	/*  Initial dependency:
	 *
	 *  a(1)
	 *  |
	 *  b(3)
	 */

	a, _ := newStream(1, sc, defaultPriority)
	b, _ := newStream(3, sc, PriorityParam{
		Weight:    20,
		StreamDep: 1,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: a,
		rootLast:  a,
		l:         1,
		n:         2,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   nil,
			depNext:   b,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(b),
			n:         2,
		},
		streamTest{
			st:        b,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    20,
			weightSum: 0,
			n:         1,
		},
	})

	/* Add stream c(5) as dependent on a(1):
	 *
	 *     a(1)
	 *    /   \
	 *  c(5)  b(3)
	 */

	c, _ := newStream(5, sc, PriorityParam{
		Weight:    24,
		StreamDep: 1,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: a,
		rootLast:  a,
		l:         1,
		n:         3,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   nil,
			depNext:   c,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(c, b),
			n:         3,
		},
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   c,
			sibNext:   nil,
			weight:    20,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        c,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   b,
			weight:    24,
			weightSum: 0,
			n:         1,
		},
	})

	/* Add streams d(7), e(9), f(11) and g(13):
	 *
	 *        __a(1)__
	 *       /  |     \
	 *   g(13) c(5)   b(3)
	 *          |    /    \
	 *         e(9) f(11) d(7)
	 */

	d, _ := newStream(7, sc, PriorityParam{
		Weight:    defaultWeight - 1,
		StreamDep: 3,
	})
	e, _ := newStream(9, sc, PriorityParam{
		StreamDep: 5,
		Weight:    60,
	})
	f, _ := newStream(11, sc, PriorityParam{
		StreamDep: 3,
		Weight:    defaultWeight - 1,
	})
	g, _ := newStream(13, sc, PriorityParam{
		StreamDep: 1,
		Weight:    defaultWeight - 1,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: a,
		rootLast:  a,
		l:         1,
		n:         7,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   nil,
			depNext:   g,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(g, c, b),
			n:         7,
		},
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   f,
			sibPrev:   c,
			sibNext:   nil,
			weight:    20,
			weightSum: sumWeight(f, d),
			n:         3,
		},
		streamTest{
			st:        c,
			depPrev:   nil,
			depNext:   e,
			sibPrev:   g,
			sibNext:   b,
			weight:    24,
			weightSum: sumWeight(e),
			n:         2,
		},
		streamTest{
			st:        d,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   f,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        e,
			depPrev:   c,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    60,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        f,
			depPrev:   b,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   d,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        g,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   c,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
	})

	/* Add stream j(15) with exclusive dependency on b(3):
	 *
	 *        __a(1)__
	 *       /   |    \
	 *   g(13) c(5)   b(3)
	 *           |     |
	 *         e(9)  j(15)
	 *              /    \
	 *            f(11) d(7)
	 */

	j, _ := newStream(15, sc, PriorityParam{
		StreamDep: 3,
		Weight:    43,
		Exclusive: true,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: a,
		rootLast:  a,
		l:         1,
		n:         8,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   nil,
			depNext:   g,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(g, c, b),
			n:         8,
		},
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   j,
			sibPrev:   c,
			sibNext:   nil,
			weight:    20,
			weightSum: sumWeight(j),
			n:         4,
		},
		streamTest{
			st:        f,
			depPrev:   j,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   d,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   b,
			depNext:   f,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    43,
			weightSum: sumWeight(f, d),
			n:         3,
		},
	})

	/* Create a root stream h(17) and then create a stream i(19) with exclusive
	 * dependency on 0
	 *
	 *       __i(19)__
	 *      /         \
	 *   h(17)     __a(1)__
	 *            /   |    \
	 *        g(13) c(5)   b(3)
	 *                |     |
	 *              e(9)  j(15)
	 *                   /    \
	 *                 f(11) d(7)
	 */

	h, _ := newStream(17, sc, defaultPriority)

	// intermediate state for roots before Exclusive on 0
	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: h,
		rootLast:  a,
		l:         2,
		n:         9,
	})

	i, _ := newStream(19, sc, PriorityParam{
		StreamDep: 0,
		Exclusive: true,
		Weight:    244,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: i,
		rootLast:  i,
		l:         1,
		n:         10,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   nil,
			depNext:   g,
			sibPrev:   h,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(g, c, b),
			n:         8,
		},
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   j,
			sibPrev:   c,
			sibNext:   nil,
			weight:    20,
			weightSum: sumWeight(j),
			n:         4,
		},
		streamTest{
			st:        c,
			depPrev:   nil,
			depNext:   e,
			sibPrev:   g,
			sibNext:   b,
			weight:    24,
			weightSum: sumWeight(e),
			n:         2,
		},
		streamTest{
			st:        d,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   f,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        e,
			depPrev:   c,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    60,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        f,
			depPrev:   j,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   d,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   b,
			depNext:   f,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    43,
			weightSum: sumWeight(f, d),
			n:         3,
		},
		streamTest{
			st:        g,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   c,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        h,
			depPrev:   i,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   a,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        i,
			depPrev:   nil,
			depNext:   h,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    244,
			weightSum: sumWeight(h, a),
			n:         10,
		},
	})

	/* Create streams z(21), k(23), l(25), m(27):
	 *
	 *  k(23)    z(21)    i(19)
	 *   |        |        |
	 *  l(25)    m(27)    hudge tree (vide supra)
	 *
	 *  and move to following state by creating stream n(29) with exclusive dependency on 0:
	 *
	 *     _____ n(29)_____
	 *    /       |        \
	 *  k(23)    z(21)    i(19)
	 *   |        |        |
	 *  l(25)    m(27)    hudge tree (vide supra)
	 *
	 */

	z, _ := newStream(21, sc, defaultPriority)

	k, _ := newStream(23, sc, defaultPriority)

	l, _ := newStream(25, sc, PriorityParam{
		StreamDep: 23,
		Weight:    defaultWeight - 1,
	})

	m, _ := newStream(27, sc, PriorityParam{
		StreamDep: 21,
		Weight:    defaultWeight - 1,
	})

	// intermediate state for roots before Exclusive on 0
	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: k,
		rootLast:  i,
		l:         3,
		n:         14,
	})

	n, _ := newStream(29, sc, PriorityParam{
		StreamDep: 0,
		Exclusive: true,
		Weight:    defaultWeight - 1,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: n,
		rootLast:  n,
		l:         1,
		n:         15,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        i,
			depPrev:   nil,
			depNext:   h,
			sibPrev:   z,
			sibNext:   nil,
			weight:    244,
			weightSum: sumWeight(h, a),
			n:         10,
		},
		streamTest{
			st:        z,
			depPrev:   nil,
			depNext:   m,
			sibPrev:   k,
			sibNext:   i,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(m),
			n:         2,
		},
		streamTest{
			st:        k,
			depPrev:   n,
			depNext:   l,
			sibPrev:   nil,
			sibNext:   z,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(l),
			n:         2,
		},
		streamTest{
			st:        l,
			depPrev:   k,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        m,
			depPrev:   z,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        n,
			depPrev:   nil,
			depNext:   k,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(i, z, k),
			n:         15,
		},
	})

}

/*
 * Tests Adjust Stream Priority
 * =============================
 *
 * - update roots
 * - update weightSum
 * - update depPrev, depNext, sibPrev, sibNext
 * - num of streams in dependency tree
 */

func TestAdjustStreamPriority(t *testing.T) {
	sc := &serverConn{
		roots:                 newRoots(),
		streams:               make(map[uint32]*stream),
		serveG:                newGoroutineLock(),
		maxDependencyTree:     defaultMaxDependencyTree,
		maxSavedClosedStreams: defaultMaxSavedClosedStreams,
		maxSavedIdleStreams:   defaultMaxSavedIdleStreams,
	}

	/*  Initial dependency
	 *
	 *  e(9) d(7) c(5) b(3) a(1)
	 *
	 */

	a, _ := newStream(1, sc, defaultPriority)
	b, _ := newStream(3, sc, defaultPriority)
	c, _ := newStream(5, sc, PriorityParam{
		Weight:    144,
		StreamDep: 0,
	})
	d, _ := newStream(7, sc, PriorityParam{
		Weight: 254,
	})
	e, _ := newStream(9, sc, defaultPriority)

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: e,
		rootLast:  a,
		l:         5,
		n:         5,
	})

	/* Reprioritize:
	 *
	 *  c(5) b(3) a(1)
	 *   |         |
	 *  e(9)      d(7)
	 *
	 */

	d.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 1,
		Weight:    defaultWeight - 1,
	})
	e.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 5,
		Weight:    255,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: c,
		rootLast:  a,
		l:         3,
		n:         5,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   nil,
			depNext:   d,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(d),
			n:         2,
		},
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        c,
			depPrev:   nil,
			depNext:   e,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    144,
			weightSum: sumWeight(e),
			n:         2,
		},
		streamTest{
			st:        d,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        e,
			depPrev:   c,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    255,
			weightSum: 0,
			n:         1,
		},
	})

	/* Add more streams:
	 *
	 *  k(21) j(19) i(17) h(15) g(13) f(11) c(5) b(3) a(1)
	 *                                       |         |
	 *                                      e(9)      d(7)
	 *
	 */

	f, _ := newStream(11, sc, PriorityParam{
		Weight: 1,
	})
	g, _ := newStream(13, sc, PriorityParam{
		Weight:    32,
		StreamDep: 0,
	})
	h, _ := newStream(15, sc, defaultPriority)
	i, _ := newStream(17, sc, PriorityParam{
		Weight: 141,
	})
	j, _ := newStream(19, sc, defaultPriority)
	k, _ := newStream(21, sc, defaultPriority)

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: k,
		rootLast:  a,
		l:         9,
		n:         11,
	})

	/* Reprioritize dependencies:
	 *
	 *               g(13)           c(5)       a(1)
	 *              /  |  \          /   \       |
	 *         k(21) j(19) i(17)  f(11)  e(9)   d(7)
	 *         / \
	 *      b(3) h(15)
	 */

	f.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 5,
		Weight:    21,
	})
	i.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 13,
		Weight:    defaultWeight - 1,
	})
	j.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 13,
		Weight:    123,
	})
	k.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 13,
		Weight:    72,
	})
	h.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 21,
		Weight:    defaultWeight - 1,
	})
	b.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 21,
		Weight:    defaultWeight - 1,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: g,
		rootLast:  a,
		l:         3,
		n:         11,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   nil,
			depNext:   d,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(d),
			n:         2,
		},
		streamTest{
			st:        b,
			depPrev:   k,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   h,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        c,
			depPrev:   nil,
			depNext:   f,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    144,
			weightSum: sumWeight(f, e),
			n:         3,
		},
		streamTest{
			st:        d,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        e,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   f,
			sibNext:   nil,
			weight:    255,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        f,
			depPrev:   c,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   e,
			weight:    21,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        g,
			depPrev:   nil,
			depNext:   k,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    32,
			weightSum: sumWeight(k, j, i),
			n:         6,
		},
		streamTest{
			st:        h,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   b,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        i,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   j,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   k,
			sibNext:   i,
			weight:    123,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        k,
			depPrev:   g,
			depNext:   b,
			sibPrev:   nil,
			sibNext:   j,
			weight:    72,
			weightSum: sumWeight(b, h),
			n:         3,
		},
	})

	/* Reprioritize dependency: move k(21) and a(1) subtrees to j(19):
	 *
	 *           g(13)           c(5)
	 *           /  \           /   \
	 *        j(19) i(17)    f(11)  e(9)
	 *        /   \
	 *     a(1)  k(21)
	 *       |    /  \
	 *     d(7) b(3) h(15)
	 */

	k.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 19,
		Weight:    defaultWeight - 1,
	})
	a.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 19,
		Weight:    12,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: g,
		rootLast:  c,
		l:         2,
		n:         11,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   j,
			depNext:   d,
			sibPrev:   nil,
			sibNext:   k,
			weight:    12,
			weightSum: sumWeight(d),
			n:         2,
		},
		streamTest{
			st:        b,
			depPrev:   k,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   h,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        c,
			depPrev:   nil,
			depNext:   f,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    144,
			weightSum: sumWeight(f, e),
			n:         3,
		},
		streamTest{
			st:        d,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        e,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   f,
			sibNext:   nil,
			weight:    255,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        f,
			depPrev:   c,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   e,
			weight:    21,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        g,
			depPrev:   nil,
			depNext:   j,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    32,
			weightSum: sumWeight(j, i),
			n:         8,
		},
		streamTest{
			st:        h,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   b,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        i,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   j,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   g,
			depNext:   a,
			sibPrev:   nil,
			sibNext:   i,
			weight:    123,
			weightSum: sumWeight(a, k),
			n:         6,
		},
		streamTest{
			st:        k,
			depPrev:   nil,
			depNext:   b,
			sibPrev:   a,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(b, h),
			n:         3,
		},
	})

	/* Reprioritize dependency: move i(17) under k(21) and c(5) subtree under h(15):
	 *
	 *              g(13)
	 *               |
	 *             j(19)
	 *            /     \
	 *         a(1)      k(21)
	 *           |     /  |   \
	 *         d(7) i(17) b(3) h(15)
	 *                          |
	 *                         c(5)
	 *                         /   \
	 *                      f(11)  e(9)
	 */

	i.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 21,
		Weight:    1,
	})
	c.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 15,
		Weight:    20,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: g,
		rootLast:  g,
		l:         1,
		n:         11,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   j,
			depNext:   d,
			sibPrev:   nil,
			sibNext:   k,
			weight:    12,
			weightSum: sumWeight(d),
			n:         2,
		},
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   i,
			sibNext:   h,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        c,
			depPrev:   h,
			depNext:   f,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    20,
			weightSum: sumWeight(f, e),
			n:         3,
		},
		streamTest{
			st:        d,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        e,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   f,
			sibNext:   nil,
			weight:    255,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        f,
			depPrev:   c,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   e,
			weight:    21,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        g,
			depPrev:   nil,
			depNext:   j,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    32,
			weightSum: sumWeight(j),
			n:         11,
		},
		streamTest{
			st:        h,
			depPrev:   nil,
			depNext:   c,
			sibPrev:   b,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(c),
			n:         4,
		},
		streamTest{
			st:        i,
			depPrev:   k,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   b,
			weight:    1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   g,
			depNext:   a,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    123,
			weightSum: sumWeight(a, k),
			n:         10,
		},
		streamTest{
			st:        k,
			depPrev:   nil,
			depNext:   i,
			sibPrev:   a,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(i, b, h),
			n:         7,
		},
	})

	/* Add new streams:
	 *
	 *  p(31)  o(29)  n(27)  m(25)  l(23)    g(13)
	 *                                         |
	 *                                       j(19)
	 *                                      /     \
	 *                                   a(1)      k(21)
	 *                                     |     /  |   \
	 *                                   d(7) i(17) b(3) h(15)
	 *                                                    |
	 *                                                   c(5)
	 *                                                   /   \
	 *                                                f(11)  e(9)
	 */

	l, _ := newStream(23, sc, defaultPriority)
	m, _ := newStream(25, sc, defaultPriority)
	n, _ := newStream(27, sc, defaultPriority)
	o, _ := newStream(29, sc, defaultPriority)
	p, _ := newStream(31, sc, defaultPriority)

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: p,
		rootLast:  g,
		l:         6,
		n:         16,
	})

	/* Reprioritize dependency: add stream m(25) with exlusive flag to j(19):
	 *
	 *  p(31)  o(29)  n(27)  l(23)  g(13)
	 *                               |
	 *                              j(19)
	 *                               |
	 *                              m(25)
	 *                            /     \
	 *                        a(1)      k(21)
	 *                          |     /  |   \
	 *                        d(7) i(17) b(3) h(15)
	 *                                         |
	 *                                        c(5)
	 *                                        /   \
	 *                                     f(11)  e(9)
	 */

	m.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 19,
		Exclusive: true,
		Weight:    183,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: p,
		rootLast:  g,
		l:         5,
		n:         16,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   m,
			depNext:   d,
			sibPrev:   nil,
			sibNext:   k,
			weight:    12,
			weightSum: sumWeight(d),
			n:         2,
		},
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   i,
			sibNext:   h,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        c,
			depPrev:   h,
			depNext:   f,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    20,
			weightSum: sumWeight(f, e),
			n:         3,
		},
		streamTest{
			st:        d,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        e,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   f,
			sibNext:   nil,
			weight:    255,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        f,
			depPrev:   c,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   e,
			weight:    21,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        g,
			depPrev:   nil,
			depNext:   j,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    32,
			weightSum: sumWeight(j),
			n:         12,
		},
		streamTest{
			st:        h,
			depPrev:   nil,
			depNext:   c,
			sibPrev:   b,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(c),
			n:         4,
		},
		streamTest{
			st:        i,
			depPrev:   k,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   b,
			weight:    1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   g,
			depNext:   m,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    123,
			weightSum: sumWeight(m),
			n:         11,
		},
		streamTest{
			st:        k,
			depPrev:   nil,
			depNext:   i,
			sibPrev:   a,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(i, b, h),
			n:         7,
		},
		streamTest{
			st:        m,
			depPrev:   j,
			depNext:   a,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    183,
			weightSum: sumWeight(a, k),
			n:         10,
		},
	})

	/* Reprioritize dependency: create tree under p(31) with l(23), n(27) and o(29)
	 * then add stream p(31) with exlusive flag to k(21):
	 *
	 *           g(13)
	 *            |
	 *           j(19)
	 *            |
	 *     ______m(25)____
	 *    /               \
	 *  a(1)             k(21)
	 *   |                |
	 *  d(7)   __________p(31)_________
	 *        /  |     |     |    |    \
	 *    o(29) n(27) l(23) i(17) b(3) h(15)
	 *                                  |
	 *                                 c(5)
	 *                                /   \
	 *                             f(11)  e(9)
	 */

	l.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 31,
		Weight:    20,
	})
	n.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 31,
		Weight:    defaultWeight - 1,
	})
	o.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 31,
		Weight:    42,
	})
	p.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 21,
		Exclusive: true,
		Weight:    200,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: g,
		rootLast:  g,
		l:         1,
		n:         16,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   m,
			depNext:   d,
			sibPrev:   nil,
			sibNext:   k,
			weight:    12,
			weightSum: sumWeight(d),
			n:         2,
		},
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   i,
			sibNext:   h,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        c,
			depPrev:   h,
			depNext:   f,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    20,
			weightSum: sumWeight(f, e),
			n:         3,
		},
		streamTest{
			st:        d,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        e,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   f,
			sibNext:   nil,
			weight:    255,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        f,
			depPrev:   c,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   e,
			weight:    21,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        g,
			depPrev:   nil,
			depNext:   j,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    32,
			weightSum: sumWeight(j),
			n:         16,
		},
		streamTest{
			st:        h,
			depPrev:   nil,
			depNext:   c,
			sibPrev:   b,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(c),
			n:         4,
		},
		streamTest{
			st:        i,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   l,
			sibNext:   b,
			weight:    1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   g,
			depNext:   m,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    123,
			weightSum: sumWeight(m),
			n:         15,
		},
		streamTest{
			st:        k,
			depPrev:   nil,
			depNext:   p,
			sibPrev:   a,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(p),
			n:         11,
		},
		streamTest{
			st:        l,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   n,
			sibNext:   i,
			weight:    20,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        m,
			depPrev:   j,
			depNext:   a,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    183,
			weightSum: sumWeight(a, k),
			n:         14,
		},
		streamTest{
			st:        n,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   o,
			sibNext:   l,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        o,
			depPrev:   p,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   n,
			weight:    42,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        p,
			depPrev:   k,
			depNext:   o,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    200,
			weightSum: sumWeight(o, n, l, i, b, h),
			n:         10,
		},
	})

	/* Reprioritize dependency add a1(101) with exclusive flag to f1(111)
	 *
	 * From:
	 *
	 *   b1(103)          __a1(101)__
	 *     |             /     |     \
	 *   f1(111)     e1(109) d1(107) c1(105)
	 *
	 * To:
	 *
	 *         b1(103)
	 *           |
	 *         f1(111)
	 *           |
	 *       __a1(101)__
	 *      /     |     \
	 *  e1(109) d1(107) c1(105)
	 */

	a1, _ := newStream(101, sc, defaultPriority)
	b1, _ := newStream(103, sc, defaultPriority)
	c1, _ := newStream(105, sc, PriorityParam{
		StreamDep: 101,
		Weight:    148,
	})
	d1, _ := newStream(107, sc, PriorityParam{
		StreamDep: 101,
		Weight:    3,
	})
	e1, _ := newStream(109, sc, PriorityParam{
		StreamDep: 101,
		Weight:    81,
	})
	f1, _ := newStream(111, sc, PriorityParam{
		StreamDep: 103,
		Weight:    52,
	})

	// intermediate state for roots before Exclusive on root
	// don't forget about early roots
	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: b1,
		rootLast:  g,
		l:         3,
		n:         22,
	})

	a1.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 111,
		Exclusive: true,
		Weight:    200,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: b1,
		rootLast:  g,
		l:         2,
		n:         22,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a1,
			depPrev:   f1,
			depNext:   e1,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    200,
			weightSum: sumWeight(e1, d1, c1),
			n:         4,
		},
		streamTest{
			st:        b1,
			depPrev:   nil,
			depNext:   f1,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(f1),
			n:         6,
		},
		streamTest{
			st:        c1,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   d1,
			sibNext:   nil,
			weight:    148,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        d1,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   e1,
			sibNext:   c1,
			weight:    3,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        e1,
			depPrev:   a1,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   d1,
			weight:    81,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        f1,
			depPrev:   b1,
			depNext:   a1,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    52,
			weightSum: sumWeight(a1),
			n:         5,
		},
	})

	/* Reprioritize to dependent on own dependency: h(15) to c(5)
	 *
	 * From:
	 *
	 *           g(13)
	 *            |
	 *           j(19)
	 *            |
	 *     ______m(25)____
	 *    /               \
	 *  a(1)             k(21)
	 *   |                |
	 *  d(7)   __________p(31)_________
	 *        /  |     |     |    |    \
	 *    o(29) n(27) l(23) i(17) b(3) h(15)
	 *                                  |
	 *                                 c(5)
	 *                                /   \
	 *                             f(11)  e(9)
	 * Intermediate:
	 *
	 *              g(13)
	 *               |
	 *              j(19)
	 *               |
	 *      _________m(25)______
	 *     /                    \
	 *   a(1)                  k(21)
	 *    |                     |
	 *   d(7)    ______________p(31)_____________
	 *          /    |     |     |     |    |    \
	 *       _c(5)_  o(29) n(27) l(23) i(17) b(3) h(15)
	 *      /      \
	 *    f(11)    e(9)
	 *
	 *
	 * Non-exclusive final:
	 *
	 *           g(13)
	 *            |
	 *           j(19)
	 *            |
	 *     _______m(25)_______
	 *    /                   \
	 *  a(1)                  k(21)
	 *   |                     |
	 *  d(7)      ____________p(31)__________
	 *           /     |     |     |    |    \
	 *      __c(5)__  o(29) n(27) l(23) i(17) b(3)
	 *     /   |    \
	 *  h(15) f(11) e(9)
	 *
	 */

	h.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 5,
		Weight:    254,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: b1,
		rootLast:  g,
		l:         2,
		n:         22,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   m,
			depNext:   d,
			sibPrev:   nil,
			sibNext:   k,
			weight:    12,
			weightSum: sumWeight(d),
			n:         2,
		},
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   i,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        c,
			depPrev:   p,
			depNext:   h,
			sibPrev:   nil,
			sibNext:   o,
			weight:    20,
			weightSum: sumWeight(h, f, e),
			n:         4,
		},
		streamTest{
			st:        d,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        e,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   f,
			sibNext:   nil,
			weight:    255,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        f,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   h,
			sibNext:   e,
			weight:    21,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        g,
			depPrev:   nil,
			depNext:   j,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    32,
			weightSum: sumWeight(j),
			n:         16,
		},
		streamTest{
			st:        h,
			depPrev:   c,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   f,
			weight:    254,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        i,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   l,
			sibNext:   b,
			weight:    1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   g,
			depNext:   m,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    123,
			weightSum: sumWeight(m),
			n:         15,
		},
		streamTest{
			st:        k,
			depPrev:   nil,
			depNext:   p,
			sibPrev:   a,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(p),
			n:         11,
		},
		streamTest{
			st:        l,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   n,
			sibNext:   i,
			weight:    20,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        m,
			depPrev:   j,
			depNext:   a,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    183,
			weightSum: sumWeight(a, k),
			n:         14,
		},
		streamTest{
			st:        n,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   o,
			sibNext:   l,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        o,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   c,
			sibNext:   n,
			weight:    42,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        p,
			depPrev:   k,
			depNext:   c,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    200,
			weightSum: sumWeight(c, o, n, l, i, b),
			n:         10,
		},
	})

	/* Reprioritize to dependent on own dependency: m(25) to p(31)
	 *
	 * From:
	 *
	 *           g(13)
	 *            |
	 *           j(19)
	 *            |
	 *     _______m(25)_______
	 *    /                   \
	 *  a(1)                  k(21)
	 *   |                     |
	 *  d(7)      ____________p(31)__________
	 *           /     |     |     |    |    \
	 *      __c(5)__  o(29) n(27) l(23) i(17) b(3)
	 *     /   |    \
	 *  h(15) f(11) e(9)
	 *
	 *
	 * Intermediate:
	 *
	 *                                     g(13)
	 *                                      |
	 *                           __________j(19)_______
	 *                          /                      \
	 *            ____________p(31)__________         _m(25)_
	 *           /     |     |     |    |    \       /       \
	 *      __c(5)__  o(29) n(27) l(23) i(17) b(3) a(1)     k(21)
	 *     /   |    \                               |
	 *  h(15) f(11) e(9)                           d(7)
	 *
	 *
	 * Non-exclusive final:
	 *
	 *                                    g(13)
	 *                                     |
	 *                                    j(19)
	 *                                     |
	 *          __________________________p(31)_________________
	 *         /                 |      |     |     |      |    \
	 *      _m(25)_           __c(5)__  o(29) n(27) l(23) i(17) b(3)
	 *     /       \         /   |    \
	 *   a(1)      k(21)  h(15) f(11) e(9)
	 *    |
	 *   d(7)
	 *
	 */

	m.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 31,
		Weight:    111,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   m,
			depNext:   d,
			sibPrev:   nil,
			sibNext:   k,
			weight:    12,
			weightSum: sumWeight(d),
			n:         2,
		},
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   i,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        c,
			depPrev:   nil,
			depNext:   h,
			sibPrev:   m,
			sibNext:   o,
			weight:    20,
			weightSum: sumWeight(h, f, e),
			n:         4,
		},
		streamTest{
			st:        d,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        e,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   f,
			sibNext:   nil,
			weight:    255,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        f,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   h,
			sibNext:   e,
			weight:    21,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        g,
			depPrev:   nil,
			depNext:   j,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    32,
			weightSum: sumWeight(j),
			n:         16,
		},
		streamTest{
			st:        h,
			depPrev:   c,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   f,
			weight:    254,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        i,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   l,
			sibNext:   b,
			weight:    1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   g,
			depNext:   p,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    123,
			weightSum: sumWeight(p),
			n:         15,
		},
		streamTest{
			st:        k,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   a,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        l,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   n,
			sibNext:   i,
			weight:    20,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        m,
			depPrev:   p,
			depNext:   a,
			sibPrev:   nil,
			sibNext:   c,
			weight:    111,
			weightSum: sumWeight(a, k),
			n:         4,
		},
		streamTest{
			st:        n,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   o,
			sibNext:   l,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        o,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   c,
			sibNext:   n,
			weight:    42,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        p,
			depPrev:   j,
			depNext:   m,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    200,
			weightSum: sumWeight(m, c, o, n, l, i, b),
			n:         14,
		},
	})

	/* Reprioritize dependency to dependent on own dependency with exclusive
	 * flag: j(19) to c(5)
	 *
	 * From:
	 *                                    g(13)
	 *                                     |
	 *                                    j(19)
	 *                                     |
	 *          __________________________p(31)_________________
	 *         /                 |      |     |     |      |    \
	 *      _m(25)_           __c(5)__  o(29) n(27) l(23) i(17) b(3)
	 *     /       \         /   |    \
	 *   a(1)      k(21)  h(15) f(11) e(9)
	 *    |
	 *   d(7)
	 *
	 *
	 * Intermediate:
	 *
	 *          ___________ g(13)_________
	 *         /                          \
	 *     __c(5)__                       j(19)
	 *    /   |    \                       |
	 * h(15) f(11) e(9)    _______________p(31)____________
	 *                    /         |      |     |     |   \
	 *                 _m(25)_     o(29) n(27) l(23) i(17) b(3)
	 *                /       \
	 *              a(1)      k(21)
	 *               |
	 *              d(7)
	 *
	 * Exclusive final:
	 *
	 *                                        g(13)
	 *                                         |
	 *                                        c(5)
	 *                                         |
	 *                           _____________j(19)___________
	 *                          /                  |     |    \
	 *         _______________p(31)____________   h(15) f(11) e(9)
	 *        /         |      |     |     |   \
	 *     _m(25)_     o(29) n(27) l(23) i(17) b(3)
	 *    /       \
	 *  a(1)     k(21)
	 *   |
	 *  d(7)
	 */

	j.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 5,
		Exclusive: true,
		Weight:    defaultWeight - 1,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   m,
			depNext:   d,
			sibPrev:   nil,
			sibNext:   k,
			weight:    12,
			weightSum: sumWeight(d),
			n:         2,
		},
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   i,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        c,
			depPrev:   g,
			depNext:   j,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    20,
			weightSum: sumWeight(j),
			n:         15,
		},
		streamTest{
			st:        d,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        e,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   f,
			sibNext:   nil,
			weight:    255,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        f,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   h,
			sibNext:   e,
			weight:    21,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        g,
			depPrev:   nil,
			depNext:   c,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    32,
			weightSum: sumWeight(c),
			n:         16,
		},
		streamTest{
			st:        h,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   p,
			sibNext:   f,
			weight:    254,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        i,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   l,
			sibNext:   b,
			weight:    1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   c,
			depNext:   p,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(p, h, f, e),
			n:         14,
		},
		streamTest{
			st:        k,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   a,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        l,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   n,
			sibNext:   i,
			weight:    20,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        m,
			depPrev:   p,
			depNext:   a,
			sibPrev:   nil,
			sibNext:   o,
			weight:    111,
			weightSum: sumWeight(a, k),
			n:         4,
		},
		streamTest{
			st:        n,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   o,
			sibNext:   l,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        o,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   m,
			sibNext:   n,
			weight:    42,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        p,
			depPrev:   j,
			depNext:   m,
			sibPrev:   nil,
			sibNext:   h,
			weight:    200,
			weightSum: sumWeight(m, o, n, l, i, b),
			n:         10,
		},
	})

	/* Reprioritize dependency to dependent on own dependency: g(13) to f(11)
	 *
	 * From:
	 *                                        g(13)
	 *                                         |
	 *                                        c(5)
	 *                                         |
	 *                           _____________j(19)___________
	 *                          /                  |     |    \
	 *         _______________p(31)____________   h(15) f(11) e(9)
	 *        /         |      |     |     |   \
	 *     _m(25)_     o(29) n(27) l(23) i(17) b(3)
	 *    /       \
	 *  a(1)     k(21)
	 *   |
	 *  d(7)
	 *
	 * Intermediate:
	 *
	 *                                f(11)   g(13)
	 *                                         |
	 *                                        c(5)
	 *                                         |
	 *                           _____________j(19)______
	 *                          /                  |     \
	 *         _______________p(31)____________   h(15) e(9)
	 *        /         |      |     |     |   \
	 *     _m(25)_     o(29) n(27) l(23) i(17) b(3)
	 *    /       \
	 *  a(1)     k(21)
	 *   |
	 *  d(7)
	 *
	 * Non-exclusive final:
	 *
	 *                                       f(11)
	 *                                         |
	 *                                       g(13)
	 *                                         |
	 *                                        c(5)
	 *                                         |
	 *                           _____________j(19)______
	 *                          /                  |     \
	 *         _______________p(31)____________   h(15) e(9)
	 *        /         |      |     |     |   \
	 *     _m(25)_     o(29) n(27) l(23) i(17) b(3)
	 *    /       \
	 *  a(1)     k(21)
	 *   |
	 *  d(7)
	 */

	g.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 11,
		Weight:    212,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: f,
		rootLast:  b1,
		l:         2,
		n:         22,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   m,
			depNext:   d,
			sibPrev:   nil,
			sibNext:   k,
			weight:    12,
			weightSum: sumWeight(d),
			n:         2,
		},
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   i,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        c,
			depPrev:   g,
			depNext:   j,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    20,
			weightSum: sumWeight(j),
			n:         14,
		},
		streamTest{
			st:        d,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        e,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   h,
			sibNext:   nil,
			weight:    255,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        f,
			depPrev:   nil,
			depNext:   g,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    21,
			weightSum: sumWeight(g),
			n:         16,
		},
		streamTest{
			st:        g,
			depPrev:   f,
			depNext:   c,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    212,
			weightSum: sumWeight(c),
			n:         15,
		},
		streamTest{
			st:        h,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   p,
			sibNext:   e,
			weight:    254,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        i,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   l,
			sibNext:   b,
			weight:    1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   c,
			depNext:   p,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(p, h, e),
			n:         13,
		},
		streamTest{
			st:        k,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   a,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        l,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   n,
			sibNext:   i,
			weight:    20,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        m,
			depPrev:   p,
			depNext:   a,
			sibPrev:   nil,
			sibNext:   o,
			weight:    111,
			weightSum: sumWeight(a, k),
			n:         4,
		},
		streamTest{
			st:        n,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   o,
			sibNext:   l,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        o,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   m,
			sibNext:   n,
			weight:    42,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        p,
			depPrev:   j,
			depNext:   m,
			sibPrev:   nil,
			sibNext:   h,
			weight:    200,
			weightSum: sumWeight(m, o, n, l, i, b),
			n:         10,
		},
	})

	/* Reprioritize dependency to dependent on own dependency: f(11) to m(25)
	 *
	 * From:
	 *
	 *                                        f(11)
	 *                                         |
	 *                                        g(13)
	 *                                         |
	 *                                        c(5)
	 *                                         |
	 *                           _____________j(19)______
	 *                          /                  |     \
	 *         _______________p(31)____________   h(15) e(9)
	 *        /         |      |     |     |   \
	 *     _m(25)_     o(29) n(27) l(23) i(17) b(3)
	 *    /       \
	 *  a(1)     k(21)
	 *   |
	 *  d(7)
	 *
	 * Intermediate:
	 *
	 *                        _m(25)_         f(11)
	 *                       /       \         |
	 *                     a(1)     k(21)     g(13)
	 *                      |                  |
	 *                     d(7)               c(5)
	 *                                         |
	 *                           _____________j(19)___
	 *                          /               |     \
	 *               _________p(31)_______    h(15) e(9)
	 *              /   |     |     |     \
	 *           o(29) n(27) l(23) i(17) b(3)
	 *
	 *
	 * Non-exclusive final:
	 *
	 *                                            ___m(25)__
	 *                                           /    |     \
	 *                                        f(11)  a(1)  k(21)
	 *                                         |      |
	 *                                        g(13)  d(7)
	 *                                         |
	 *                                        c(5)
	 *                                         |
	 *                           _____________j(19)___
	 *                          /               |     \
	 *        		 _________p(31)_______    h(15) e(9)
	 *        		/   |     |     |     \
	 *           o(29) n(27) l(23) i(17) b(3)
	 */

	f.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 25,
		Weight:    99,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: m,
		rootLast:  b1,
		l:         2,
		n:         22,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   nil,
			depNext:   d,
			sibPrev:   f,
			sibNext:   k,
			weight:    12,
			weightSum: sumWeight(d),
			n:         2,
		},
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   i,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        c,
			depPrev:   g,
			depNext:   j,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    20,
			weightSum: sumWeight(j),
			n:         10,
		},
		streamTest{
			st:        d,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        e,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   h,
			sibNext:   nil,
			weight:    255,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        f,
			depPrev:   m,
			depNext:   g,
			sibPrev:   nil,
			sibNext:   a,
			weight:    99,
			weightSum: sumWeight(g),
			n:         12,
		},
		streamTest{
			st:        g,
			depPrev:   f,
			depNext:   c,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    212,
			weightSum: sumWeight(c),
			n:         11,
		},
		streamTest{
			st:        h,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   p,
			sibNext:   e,
			weight:    254,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        i,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   l,
			sibNext:   b,
			weight:    1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   c,
			depNext:   p,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(p, h, e),
			n:         9,
		},
		streamTest{
			st:        k,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   a,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        l,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   n,
			sibNext:   i,
			weight:    20,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        m,
			depPrev:   nil,
			depNext:   f,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    111,
			weightSum: sumWeight(f, a, k),
			n:         16,
		},
		streamTest{
			st:        n,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   o,
			sibNext:   l,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        o,
			depPrev:   p,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   n,
			weight:    42,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        p,
			depPrev:   j,
			depNext:   o,
			sibPrev:   nil,
			sibNext:   h,
			weight:    200,
			weightSum: sumWeight(o, n, l, i, b),
			n:         6,
		},
	})

	/* Reprioritize dependency to dependent on own dependency with exclusive
	 * flag: m(25) to h(15)
	 *
	 * From:
	 *
	 *                                   ___m(25)__
	 *                                  /    |     \
	 *                               f(11)  a(1)  k(21)
	 *                                |      |
	 *                               g(13)  d(7)
	 *                                |
	 *                               c(5)
	 *                                |
	 *                  _____________j(19)___
	 *                 /               |     \
	 *     _________p(31)_______     h(15)   e(9)
	 *    /   |     |     |     \
	 *  o(29) n(27) l(23) i(17) b(3)
	 *
	 * Intermediate:
	 *
	 *                      h(15)         ___m(25)__
	 *                                   /    |     \
	 *                                f(11)  a(1)  k(21)
	 *                                 |      |
	 *                                g(13)  d(7)
	 *                                 |
	 *                                c(5)
	 *                                 |
	 *                         _______j(19)___
	 *                        /               \
	 *             _________p(31)_______     e(9)
	 *            /   |     |     |     \
	 *         o(29) n(27) l(23) i(17)  b(3)
	 *
	 * Exclusive final:
	 *
	 *                                       h(15)
	 *                                        |
	 *                                    ___m(25)__
	 *                                   /    |     \
	 *                                f(11)  a(1)  k(21)
	 *                                 |      |
	 *                                g(13)  d(7)
	 *                                 |
	 *                                c(5)
	 *                                 |
	 *                         _______j(19)___
	 *                        /               \
	 *             _________p(31)_______     e(9)
	 *            /   |     |     |     \
	 *         o(29) n(27) l(23) i(17)  b(3)
	 */

	m.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 15,
		Weight:    33,
		Exclusive: true,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: h,
		rootLast:  b1,
		l:         2,
		n:         22,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   nil,
			depNext:   d,
			sibPrev:   f,
			sibNext:   k,
			weight:    12,
			weightSum: sumWeight(d),
			n:         2,
		},
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   i,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        c,
			depPrev:   g,
			depNext:   j,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    20,
			weightSum: sumWeight(j),
			n:         9,
		},
		streamTest{
			st:        d,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        e,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   p,
			sibNext:   nil,
			weight:    255,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        f,
			depPrev:   m,
			depNext:   g,
			sibPrev:   nil,
			sibNext:   a,
			weight:    99,
			weightSum: sumWeight(g),
			n:         11,
		},
		streamTest{
			st:        g,
			depPrev:   f,
			depNext:   c,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    212,
			weightSum: sumWeight(c),
			n:         10,
		},
		streamTest{
			st:        h,
			depPrev:   nil,
			depNext:   m,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    254,
			weightSum: sumWeight(m),
			n:         16,
		},
		streamTest{
			st:        i,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   l,
			sibNext:   b,
			weight:    1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   c,
			depNext:   p,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(p, e),
			n:         8,
		},
		streamTest{
			st:        k,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   a,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        l,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   n,
			sibNext:   i,
			weight:    20,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        m,
			depPrev:   h,
			depNext:   f,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    33,
			weightSum: sumWeight(f, a, k),
			n:         15,
		},
		streamTest{
			st:        n,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   o,
			sibNext:   l,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        o,
			depPrev:   p,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   n,
			weight:    42,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        p,
			depPrev:   j,
			depNext:   o,
			sibPrev:   nil,
			sibNext:   e,
			weight:    200,
			weightSum: sumWeight(o, n, l, i, b),
			n:         6,
		},
	})

	/* Reprioritize dependency to dependent on own dependency with exclusive
	 * flag: h(15) to j(19)
	 *
	 * From:
	 *
	 *                               h(15)
	 *                                |
	 *                            ___m(25)__
	 *                           /    |     \
	 *                        f(11)  a(1)  k(21)
	 *                         |      |
	 *                        g(13)  d(7)
	 *                         |
	 *                        c(5)
	 *                         |
	 *                  ______j(19)___
	 *                 /               \
	 *       _________p(31)_____      e(9)
	 *      /   |     |   |     \
	 * o(29) n(27) l(23) i(17) b(3)
	 *
	 * Intermediate:
	 *
	 *                   ______j(19)___          h(15)
	 *                  /              \          |
	 *       _________p(31)_____      e(9)    ___m(25)__
	 *      /   |     |   |     \            /    |     \
	 * o(29) n(27) l(23) i(17) b(3)       f(11)  a(1)  k(21)
	 *                                      |     |
	 *                                    g(13)  d(7)
	 *                                      |
	 *                                    c(5)
	 *
	 * Exclusive final:
	 *
	 *                         j(19)
	 *                           |
	 *           ______________h(15)__________________
	 *          /                     |               \
	 *     ___m(25)__       _________p(31)_______     e(9)
	 *    /    |     \     /     |     |     |   \
	 *  f(11) a(1) k(21) o(29) n(27) l(23) i(17) b(3)
	 *   |     |
	 *  g(13) d(7)
	 *   |
	 *  c(5)
	 */

	h.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 19,
		Weight:    31,
		Exclusive: true,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: j,
		rootLast:  b1,
		l:         2,
		n:         22,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   nil,
			depNext:   d,
			sibPrev:   f,
			sibNext:   k,
			weight:    12,
			weightSum: sumWeight(d),
			n:         2,
		},
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   i,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        c,
			depPrev:   g,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    20,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        d,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        e,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   p,
			sibNext:   nil,
			weight:    255,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        f,
			depPrev:   m,
			depNext:   g,
			sibPrev:   nil,
			sibNext:   a,
			weight:    99,
			weightSum: sumWeight(g),
			n:         3,
		},
		streamTest{
			st:        g,
			depPrev:   f,
			depNext:   c,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    212,
			weightSum: sumWeight(c),
			n:         2,
		},
		streamTest{
			st:        h,
			depPrev:   j,
			depNext:   m,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    31,
			weightSum: sumWeight(m, p, e),
			n:         15,
		},
		streamTest{
			st:        i,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   l,
			sibNext:   b,
			weight:    1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   nil,
			depNext:   h,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(h),
			n:         16,
		},
		streamTest{
			st:        k,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   a,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        l,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   n,
			sibNext:   i,
			weight:    20,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        m,
			depPrev:   h,
			depNext:   f,
			sibPrev:   nil,
			sibNext:   p,
			weight:    33,
			weightSum: sumWeight(f, a, k),
			n:         7,
		},
		streamTest{
			st:        n,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   o,
			sibNext:   l,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        o,
			depPrev:   p,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   n,
			weight:    42,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        p,
			depPrev:   nil,
			depNext:   o,
			sibPrev:   m,
			sibNext:   e,
			weight:    200,
			weightSum: sumWeight(o, n, l, i, b),
			n:         6,
		},
	})

	/* Reprioritize dependency: move c(5) to 0
	 *
	 * From:
	 *
	 *                         j(19)                        b1(113)
	 *                           |                             |
	 *           ______________h(15)__________________        ...
	 *          /                     |               \
	 *     ___m(25)__       _________p(31)_______     e(9)
	 *    /    |     \     /     |     |     |   \
	 *  f(11) a(1) k(21) o(29) n(27) l(23) i(17) b(3)
	 *   |     |
	 *  g(13) d(7)
	 *   |
	 *  c(5)
	 *
	 * To:
	 *
	 *  c(5)                   j(19)                        b1(113)
	 *                           |                             |
	 *           ______________h(15)__________________        ...
	 *          /                     |               \
	 *     ___m(25)__       _________p(31)_______     e(9)
	 *    /    |     \     /     |     |     |   \
	 *  f(11) a(1) k(21) o(29) n(27) l(23) i(17) b(3)
	 *   |     |
	 *  g(13) d(7)
	 *
	 */

	c.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 0,
		Weight:    231,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: c,
		rootLast:  b1,
		l:         3,
		n:         22,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   nil,
			depNext:   d,
			sibPrev:   f,
			sibNext:   k,
			weight:    12,
			weightSum: sumWeight(d),
			n:         2,
		},
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   i,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        c,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    231,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        d,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        e,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   p,
			sibNext:   nil,
			weight:    255,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        f,
			depPrev:   m,
			depNext:   g,
			sibPrev:   nil,
			sibNext:   a,
			weight:    99,
			weightSum: sumWeight(g),
			n:         2,
		},
		streamTest{
			st:        g,
			depPrev:   f,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    212,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        h,
			depPrev:   j,
			depNext:   m,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    31,
			weightSum: sumWeight(m, p, e),
			n:         14,
		},
		streamTest{
			st:        i,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   l,
			sibNext:   b,
			weight:    1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   nil,
			depNext:   h,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(h),
			n:         15,
		},
		streamTest{
			st:        k,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   a,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        l,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   n,
			sibNext:   i,
			weight:    20,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        m,
			depPrev:   h,
			depNext:   f,
			sibPrev:   nil,
			sibNext:   p,
			weight:    33,
			weightSum: sumWeight(f, a, k),
			n:         6,
		},
		streamTest{
			st:        n,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   o,
			sibNext:   l,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        o,
			depPrev:   p,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   n,
			weight:    42,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        p,
			depPrev:   nil,
			depNext:   o,
			sibPrev:   m,
			sibNext:   e,
			weight:    200,
			weightSum: sumWeight(o, n, l, i, b),
			n:         6,
		},
	})

	/* Reprioritize dependency: move stream m(25) to 0 with exclusive flag
	 *
	 * From:
	 *
	 *  c(5)                   j(19)                        b1(103)
	 *                           |                             |
	 *           ______________h(15)__________________        ...
	 *          /                     |               \
	 *     ___m(25)__       _________p(31)_______     e(9)
	 *    /    |     \     /     |     |     |   \
	 *  f(11) a(1) k(21) o(29) n(27) l(23) i(17) b(3)
	 *   |     |
	 *  g(13) d(7)
	 *
	 * To:
	 *                 _______________m(25)___________
	 *                /     |        |      |    |    \
	 *              c(5)  j(19)   b1(103) f(11) a(1) k(21)
	 *                      |        |      |    |
	 *                 ___h(15)___  ...   g(13) d(7)
	 *                /           \
	 *     _________p(31)_______  e(9)
	 *    /    |     |     |    \
	 *  o(29) n(27) l(23) i(17) b(3)
	 *
	 */

	m.adjustStreamPriority(sc, PriorityParam{
		StreamDep: 0,
		Exclusive: true,
		Weight:    67,
	})

	testRoots(t, rootTest{
		roots:     sc.roots,
		rootFirst: m,
		rootLast:  m,
		l:         1,
		n:         22,
	})

	testStreamNode(t, []streamTest{
		streamTest{
			st:        a,
			depPrev:   nil,
			depNext:   d,
			sibPrev:   f,
			sibNext:   k,
			weight:    12,
			weightSum: sumWeight(d),
			n:         2,
		},
		streamTest{
			st:        b,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   i,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        c,
			depPrev:   m,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   j,
			weight:    231,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        d,
			depPrev:   a,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        e,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   p,
			sibNext:   nil,
			weight:    255,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        f,
			depPrev:   nil,
			depNext:   g,
			sibPrev:   b1,
			sibNext:   a,
			weight:    99,
			weightSum: sumWeight(g),
			n:         2,
		},
		streamTest{
			st:        g,
			depPrev:   f,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    212,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        h,
			depPrev:   j,
			depNext:   p,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    31,
			weightSum: sumWeight(p, e),
			n:         8,
		},
		streamTest{
			st:        i,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   l,
			sibNext:   b,
			weight:    1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        j,
			depPrev:   nil,
			depNext:   h,
			sibPrev:   c,
			sibNext:   b1,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(h),
			n:         9,
		},
		streamTest{
			st:        k,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   a,
			sibNext:   nil,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        l,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   n,
			sibNext:   i,
			weight:    20,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        m,
			depPrev:   nil,
			depNext:   c,
			sibPrev:   nil,
			sibNext:   nil,
			weight:    67,
			weightSum: sumWeight(f, a, k, c, j, b1),
			n:         16 + 6,
		},
		streamTest{
			st:        n,
			depPrev:   nil,
			depNext:   nil,
			sibPrev:   o,
			sibNext:   l,
			weight:    defaultWeight - 1,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        o,
			depPrev:   p,
			depNext:   nil,
			sibPrev:   nil,
			sibNext:   n,
			weight:    42,
			weightSum: 0,
			n:         1,
		},
		streamTest{
			st:        p,
			depPrev:   h,
			depNext:   o,
			sibPrev:   nil,
			sibNext:   e,
			weight:    200,
			weightSum: sumWeight(o, n, l, i, b),
			n:         6,
		},
		streamTest{
			st:        b1,
			depPrev:   nil,
			depNext:   f1,
			sibPrev:   j,
			sibNext:   f,
			weight:    defaultWeight - 1,
			weightSum: sumWeight(f1),
			n:         6,
		},
	})
}

/*
 * Tests dependcy tree on add DATA to scheduler
 * ============================================
 *
 * - update dependency tree state depState
 * - update weightSumTop
 * - update weightEff
 */

type streamDataTest struct {
	st           *stream
	depState     streamDepState
	weightSumTop int32
	weightEff    int32
}

func testSetDepStateReady(t *testing.T, streamDataTests []streamDataTest) {
	for _, s := range streamDataTests {
		_, file, line, _ := runtime.Caller(1)
		if s.st.depState != s.depState {
			t.Errorf("%s:%d", file, line)
			t.Errorf("Stream id: %d", s.st.id)
			t.Errorf("          Expect  Get")
			t.Errorf("depState: %5s %5s", s.depState, s.st.depState)
		}
		if s.st.weightSumTop != s.weightSumTop {
			t.Errorf("%s:%d", file, line)
			t.Errorf("Stream id: %d", s.st.id)
			t.Errorf("             Expect  Get")
			t.Errorf("weightSumTop: %5d %5d", s.weightSumTop, s.st.weightSumTop)
		}
		if s.st.weightEff != s.weightEff {
			t.Errorf("%s:%d", file, line)
			t.Errorf("Stream id: %d", s.st.id)
			t.Errorf("           Expect  Get")
			t.Errorf("weightEff: %5d %5d", s.weightEff, s.st.weightEff)
		}
	}
}

func TestSetDepStateReady(t *testing.T) {
	sc := &serverConn{
		roots:                 newRoots(),
		streams:               make(map[uint32]*stream),
		serveG:                newGoroutineLock(),
		maxDependencyTree:     defaultMaxDependencyTree,
		maxSavedClosedStreams: defaultMaxSavedClosedStreams,
		maxSavedIdleStreams:   defaultMaxSavedIdleStreams,
		initialWindowSize:     initialWindowSize,
	}
	sc.flow.add(initialWindowSize)
	sc.inflow.add(initialWindowSize)
	ws := newWriteScheduler(sc.roots)

	/* Dependency tree:
	 *
	 *       _______a(1)_____
	 *      /        |       \
	 *    h(15)     k(21)    c(5)
	 *   /   \                |
	 * i(17) g(13)           m(23)
	 *        |
	 *       j(19)
	 */

	a, _ := newStream(1, sc, defaultPriority)
	c, _ := newStream(5, sc, PriorityParam{
		StreamDep: 1,
		Weight:    220,
	})
	k, _ := newStream(21, sc, PriorityParam{
		StreamDep: 1,
		Weight:    19,
	})
	h, _ := newStream(15, sc, PriorityParam{
		StreamDep: 1,
		Weight:    76,
	})
	g, _ := newStream(13, sc, PriorityParam{
		StreamDep: 15,
		Weight:    3,
	})
	i, _ := newStream(17, sc, PriorityParam{
		StreamDep: 15,
		Weight:    89,
	})
	j, _ := newStream(19, sc, PriorityParam{
		StreamDep: 13,
		Weight:    75,
	})
	m, _ := newStream(23, sc, PriorityParam{
		StreamDep: 5,
		Weight:    166,
	})

	ws.add(frameWriteMsg{
		write:  &writeData{p: []byte{0, 0, 0}},
		stream: c,
	})
	ws.add(frameWriteMsg{
		write:  &writeData{p: []byte{0, 0, 0}},
		stream: i,
	})
	ws.add(frameWriteMsg{
		write:  &writeData{p: []byte{0, 0, 0}},
		stream: j,
	})
	ws.add(frameWriteMsg{
		write:  &writeData{p: []byte{0, 0, 0}},
		stream: m,
	})

	testSetDepStateReady(t, []streamDataTest{
		streamDataTest{
			st:           a,
			depState:     depStateIdle,
			weightSumTop: sumWeight(h, c),
			weightEff:    sumWeight(a),
		},
		streamDataTest{
			st:           c,
			depState:     depStateTop,
			weightSumTop: 0, // because we are top
			weightEff:    11,
		},
		streamDataTest{
			st:           g,
			depState:     depStateIdle,
			weightSumTop: sumWeight(j),
			weightEff:    1, //h.weightEff * g.weight / h.weightSumTop,
		},
		streamDataTest{
			st:           h,
			depState:     depStateIdle,
			weightSumTop: sumWeight(i, g),
			weightEff:    4,
		},
		streamDataTest{
			st:           i,
			depState:     depStateTop,
			weightSumTop: 0,
			weightEff:    3,
		},
		streamDataTest{
			st:           j,
			depState:     depStateTop,
			weightSumTop: 0,
			weightEff:    1,
		},
		streamDataTest{
			st:           k,
			depState:     depStateIdle,
			weightSumTop: 0,
			weightEff:    16 * 19 / (76 + 220), // this data not used for scheduling
		},
		streamDataTest{
			st:           m,
			depState:     depStateReady,
			weightSumTop: 0,
			weightEff:    sumWeight(m),
		},
	})
}

/*
 * Tests dependcy tree scheduler
 * =============================
 */

/*
 * Tests dependcy tree on creating idle priority streams
 * =====================================================
 */

/*
 * Tests dependcy tree on retain closed streams and repriotize closed
 * ==================================================================
 */
