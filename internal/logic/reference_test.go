package logic

import (
	"sort"
	"testing"
)

// refCircuit 是与主引擎完全独立实现的“逐时刻（tick-by-tick）参考模拟器”：
// 采用稠密时间桶而不是事件堆，重算流程单独写一份，用于对拍验证
// 事件驱动引擎在传输延迟、同刻批量生效等语义上的正确性。
type refGate struct {
	out    int
	typ    GateType
	delay  int
	inputs []int
	topo   int
}

type refSim struct {
	t      *testing.T
	req    *Request
	idx    map[string]int
	names  []string
	gates  []*refGate // 拓扑序
	fanout [][]int    // 线网 -> 门序号（拓扑序）
	v      []bool
	initV  []bool // 初始稳态快照（不随后续模拟改写）
	edges  [][]Jump

	// ext[t] = 本拍翻转的主输入索引
	ext map[int][]int
	// bucket[t] = 本拍到期的门事件：输出线网 -> 到期值
	bucket map[int]map[int]bool
}

func refBuild(t *testing.T, req *Request) *refSim {
	t.Helper()
	r := &refSim{t: t, req: req, idx: map[string]int{}}

	addNet := func(id string) int {
		if x, ok := r.idx[id]; ok {
			return x
		}
		x := len(r.names)
		r.idx[id] = x
		r.names = append(r.names, id)
		return x
	}

	for i := range req.Inputs {
		addNet(req.Inputs[i].ID)
	}
	raw := make([]*refGate, len(req.Gates))
	for i := range req.Gates {
		g := &req.Gates[i]
		rg := &refGate{
			out:   addNet(g.ID),
			typ:   g.Type,
			delay: g.Delay,
			topo:  -1,
		}
		for _, in := range g.Inputs {
			if _, ok := r.idx[in]; !ok {
				addNet(in)
			}
			rg.inputs = append(rg.inputs, r.idx[in])
		}
		raw[i] = rg
	}

	// 独立做一次拓扑排序：按“输入线网最大深度”做分层 + 稳定排序。
	gateAtNet := make([]*refGate, len(r.names))
	for _, rg := range raw {
		gateAtNet[rg.out] = rg
	}
	depth := make([]int, len(r.names)) // 线网深度
	for {
		progress := false
		for _, rg := range raw {
			d := 0
			for _, in := range rg.inputs {
				if depth[in]+1 > d {
					d = depth[in] + 1
				}
			}
			if d != depth[rg.out] {
				depth[rg.out] = d
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	// 有环时深度会持续增大；本测试只喂合法电路。
	ordered := append([]*refGate(nil), raw...)
	sort.Slice(ordered, func(i, j int) bool {
		if depth[ordered[i].out] != depth[ordered[j].out] {
			return depth[ordered[i].out] < depth[ordered[j].out]
		}
		return r.names[ordered[i].out] < r.names[ordered[j].out]
	})
	for k, rg := range ordered {
		rg.topo = k
	}
	r.gates = ordered
	r.fanout = make([][]int, len(r.names))
	for k, rg := range ordered {
		for _, in := range rg.inputs {
			r.fanout[in] = append(r.fanout[in], k)
		}
	}
	return r
}

func (r *refSim) eval(rg *refGate) bool {
	switch rg.typ {
	case NOT:
		return !r.v[rg.inputs[0]]
	case AND:
		for _, in := range rg.inputs {
			if !r.v[in] {
				return false
			}
		}
		return true
	case OR:
		for _, in := range rg.inputs {
			if r.v[in] {
				return true
			}
		}
		return false
	case XOR:
		x := false
		for _, in := range rg.inputs {
			if r.v[in] {
				x = !x
			}
		}
		return x
	}
	return false
}

func (r *refSim) initState() {
	r.v = make([]bool, len(r.names))
	for i := range r.req.Inputs {
		in := &r.req.Inputs[i]
		r.v[r.idx[in.ID]] = in.Init
	}
	for _, rg := range r.gates {
		r.v[rg.out] = r.eval(rg)
	}
}

func (r *refSim) horizon() int {
	last := 0
	for i := range r.req.Inputs {
		for _, ev := range r.req.Inputs[i].Events {
			if ev.At > last {
				last = ev.At
			}
		}
	}
	// 最大门路径深度（以门数计）。
	d := 0
	for _, rg := range r.gates {
		// 拓扑序即深度递增序；用拓扑位置近似上界足够保守。
		_ = rg
		if rg.topo+1 > d {
			d = rg.topo + 1
		}
	}
	return last + 8*d + 8
}

func (r *refSim) run() {
	r.initState()
	r.initV = append([]bool(nil), r.v...)
	r.edges = make([][]Jump, len(r.names))
	r.ext = map[int][]int{}
	for i := range r.req.Inputs {
		in := &r.req.Inputs[i]
		for _, ev := range in.Events {
			r.ext[ev.At] = append(r.ext[ev.At], r.idx[in.ID])
		}
	}
	r.bucket = map[int]map[int]bool{}

	H := r.horizon()
	for t := 1; t <= H; t++ {
		changed := map[int]bool{}

		// 1a. 外部翻转批量生效。
		for _, net := range r.ext[t] {
			nv := !r.v[net]
			r.v[net] = nv
			changed[net] = true
			r.edges[net] = append(r.edges[net], Jump{At: t, Value: nv})
		}
		// 1b. 到期门事件批量生效；同值重复事件忽略。
		for net, nv := range r.bucket[t] {
			if r.v[net] == nv {
				continue
			}
			r.v[net] = nv
			changed[net] = true
			r.edges[net] = append(r.edges[net], Jump{At: t, Value: nv})
		}
		delete(r.bucket, t)

		if len(changed) == 0 {
			continue
		}
		// 2. 每门每拍至多一次重算，按拓扑序，结果放入未来时间桶。
		affected := map[int]bool{}
		for net := range changed {
			for _, gk := range r.fanout[net] {
				affected[gk] = true
			}
		}
		gks := make([]int, 0, len(affected))
		for k := range affected {
			gks = append(gks, k)
		}
		sort.Ints(gks)
		for _, k := range gks {
			rg := r.gates[k]
			nv := r.eval(rg)
			if r.bucket[t+rg.delay] == nil {
				r.bucket[t+rg.delay] = map[int]bool{}
			}
			r.bucket[t+rg.delay][rg.out] = nv // 只追加、不取消
		}
	}
}
