package logic

import (
	"container/heap"
	"sort"
)

// eventKey 唯一标识一个待发门事件：同一线网在同一时刻至多有一个到期值。
type eventKey struct {
	at  int
	net int
}

// pendingItem 是事件堆元素。
type pendingItem struct {
	at    int
	net   int
	value bool
}

// timeHeap 按时刻排序；同刻按线网索引排序以保证回放确定。
type timeHeap []pendingItem

func (h timeHeap) Len() int { return len(h) }
func (h timeHeap) Less(i, j int) bool {
	if h[i].at != h[j].at {
		return h[i].at < h[j].at
	}
	return h[i].net < h[j].net
}
func (h timeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *timeHeap) Push(x any)   { *h = append(*h, x.(pendingItem)) }
func (h *timeHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// simResult 保存全部线网的跳变边（测试与输出层按需筛选）。
type simResult struct {
	initV []bool
	// edges[net] 为该线网实际发生的跳变（值已改变），按时间升序。
	edges [][]Jump
}

// simulate 在编译好的电路上执行事件驱动回放。
//
// 每一拍 t：
//  1. 把 t 时刻到期的所有门事件、以及主输入在 t 的翻转批量生效；
//     到期值与当前值相同的同值事件直接丢弃（不产生跳变、不传播）。
//  2. 收集“本拍有输入真正变化”的门（每门每拍至多重算一次），
//     按拓扑序依据批量生效后的稳态输入重算，并在 t+delay 排队新事件。
//  3. 排队只追加，从不取消先前已排队的事件（纯传输延迟），
//     因此再快的连续变化也不会抹掉前一个待发跳变，窄脉冲可穿过门传播。
func (c *circuit) simulate() *simResult {
	n := len(c.names)
	v := c.initialState()

	// initV 必须独立保存：v 会在整个回放过程中被持续改写，
	// 直接别名会让时间线的“初值”变成终态。
	res := &simResult{
		initV: append([]bool(nil), v...),
		edges: make([][]Jump, n),
	}

	// 外部翻转分组：at -> 主输入索引。同一输入同刻翻转在编译期已拒绝。
	external := make(map[int][]int)
	extTimes := make(map[int]struct{})
	for i := range c.req.Inputs {
		in := &c.req.Inputs[i]
		idx := c.idIndex[in.ID]
		for _, ev := range in.Events {
			external[ev.At] = append(external[ev.At], idx)
			extTimes[ev.At] = struct{}{}
		}
	}

	// 待发事件堆 + 去重集合。
	pending := &timeHeap{}
	heap.Init(pending)
	pendingSet := make(map[eventKey]bool)

	schedule := func(at, net int, value bool) {
		key := eventKey{at, net}
		if pendingSet[key] {
			// 同一门每拍只重算一次，不可能排两个不同值；
			//出现同键即同值重复事件，忽略即可。
			return
		}
		pendingSet[key] = true
		heap.Push(pending, pendingItem{at: at, net: net, value: value})
	}

	// 下一拍时刻：外部事件与事件堆中较小者。
	nextExternal := func() (int, bool) {
		if len(extTimes) == 0 {
			return 0, false
		}
		m := 0
		for t := range extTimes {
			if m == 0 || t < m {
				m = t
			}
		}
		return m, true
	}

	for {
		tExt, hasExt := nextExternal()
		if !hasExt && pending.Len() == 0 {
			break
		}
		t := 0
		switch {
		case !hasExt:
			t = (*pending)[0].at
		case pending.Len() == 0:
			t = tExt
		case tExt < (*pending)[0].at:
			t = tExt
		default:
			t = (*pending)[0].at
		}

		// changed：本拍批量生效后值真正发生变化的线网。
		changed := make(map[int]bool)

		// 1a. 外部翻转批量生效（不同主输入同刻翻转合法）。
		if inputs, ok := external[t]; ok {
			delete(extTimes, t)
			for _, idx := range inputs {
				nv := !v[idx] // 翻转语义
				if nv != v[idx] {
					v[idx] = nv
					changed[idx] = true
					res.edges[idx] = append(res.edges[idx], Jump{At: t, Value: nv})
				}
			}
		}

		// 1b. 到期门事件批量生效；同值重复事件在此被忽略。
		for pending.Len() > 0 && (*pending)[0].at == t {
			it := heap.Pop(pending).(pendingItem)
			delete(pendingSet, eventKey{it.at, it.net})
			if v[it.net] == it.value {
				continue // 忽略同值重复事件：无跳变、不传播
			}
			v[it.net] = it.value
			changed[it.net] = true
			res.edges[it.net] = append(res.edges[it.net], Jump{At: t, Value: it.value})
		}

		if len(changed) == 0 {
			continue
		}

		// 2. 收集受影响门（输入中有线网本拍真正变化）。
		affected := make(map[*compiledGate]bool)
		changedNets := make([]int, 0, len(changed))
		for net := range changed {
			changedNets = append(changedNets, net)
		}
		sort.Ints(changedNets)
		for _, net := range changedNets {
			for _, g := range c.fanout[net] {
				affected[g] = true
			}
		}

		// 每门每拍只重算一次；按拓扑序，依据批量生效后的输入值。
		gates := make([]*compiledGate, 0, len(affected))
		for g := range affected {
			gates = append(gates, g)
		}
		sort.Slice(gates, func(a, b int) bool { return gates[a].topo < gates[b].topo })

		// 3. 只追加、不取消：即使算出的值与当前输出相同也照样排队，
		// 到期时若已相同则作为同值事件忽略；这保证中途其它路径
		// 把输出拉走时，本事件仍能在到期时把它拉回，形成窄脉冲。
		for _, g := range gates {
			nv := c.evalGate(g, v)
			schedule(t+g.delay, g.index, nv)
		}
	}

	return res
}

// buildResponse 从全量跳变中抽取观察点时间线，并识别窄脉冲。
func (c *circuit) buildResponse(res *simResult) *Response {
	observed := make(map[int]bool, len(c.req.Observe))
	for _, id := range c.req.Observe {
		observed[c.idIndex[id]] = true
	}

	resp := &Response{Timelines: []Timeline{}, Pulses: []Pulse{}}
	threshold := c.req.GlitchWidth

	// 以请求中观察点的顺序输出时间线。
	for _, id := range c.req.Observe {
		idx := c.idIndex[id]
		tl := Timeline{Net: id, Init: res.initV[idx], Jumps: []Jump{}}
		tl.Jumps = append(tl.Jumps, res.edges[idx]...)
		resp.Timelines = append(resp.Timelines, tl)
	}

	// 窄脉冲：相邻两条跳变边之间的电平区间，宽度严格小于阈值。
	// 首个跳变之前的电平是初始稳态，没有起始沿，不算脉冲。
	for idx := range c.names {
		if !observed[idx] || len(res.edges[idx]) < 2 {
			continue
		}
		for i := 0; i+1 < len(res.edges[idx]); i++ {
			from := res.edges[idx][i]
			to := res.edges[idx][i+1]
			width := to.At - from.At
			if threshold > 0 && width < threshold {
				resp.Pulses = append(resp.Pulses, Pulse{
					Net:   c.names[idx],
					From:  from.At,
					To:    to.At,
					Width: width,
					Value: from.Value,
				})
			}
		}
	}

	// 确定性输出：先按起始时刻、再按线网、再按脉冲值。
	sort.Slice(resp.Pulses, func(i, j int) bool {
		a, b := resp.Pulses[i], resp.Pulses[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.Net != b.Net {
			return a.Net < b.Net
		}
		return !a.Value && b.Value
	})

	return resp
}

// Run 校验并模拟一份请求，返回拒绝原因或回放结果。
func Run(req *Request) (*Response, error) {
	c, err := compile(req)
	if err != nil {
		return nil, err
	}
	res := c.simulate()
	return c.buildResponse(res), nil
}
