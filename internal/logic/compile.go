package logic

import "sort"

// circuit 是通过全部校验后的编译结果。
type circuit struct {
	req *Request

	// 线网索引：主输入与门输出共用一个 id 命名空间。
	idIndex map[string]int
	names   []string
	isInput []bool // 该索引是否为主输入

	// 门按拓扑序排列（输入在前、输出在后）。
	gates []*compiledGate

	// 反向扇出：线网 -> 以它为输入的门（拓扑序排列）。
	fanout [][]*compiledGate
}

type compiledGate struct {
	index  int // 输出线网索引
	typ    GateType
	delay  int
	inputs []int // 输入线网索引（已去重校验）
	topo   int   // 在拓扑序中的位置
}

// compile 执行整份请求的静态校验并编译电路。
// 任何结构性错误都以 *ErrReject 整份拒绝。
func compile(req *Request) (*circuit, error) {
	if len(req.Gates) > 40 {
		return nil, rejectf("门数量 %d 超过上限 40", len(req.Gates))
	}
	if req.GlitchWidth < 0 {
		return nil, rejectf("glitch_width 不能为负")
	}

	c := &circuit{
		req:     req,
		idIndex: make(map[string]int),
	}

	// 先登记主输入。
	for i := range req.Inputs {
		in := &req.Inputs[i]
		if in.ID == "" {
			return nil, rejectf("存在缺少 id 的主输入")
		}
		if _, dup := c.idIndex[in.ID]; dup {
			return nil, rejectf("线网 id 重复定义: %q", in.ID)
		}
		c.idIndex[in.ID] = len(c.names)
		c.names = append(c.names, in.ID)
		c.isInput = append(c.isInput, true)
	}

	// 登记门输出（单驱动：门 id 不得与主输入或其它门重复）。
	c.gates = make([]*compiledGate, len(req.Gates))
	for i := range req.Gates {
		g := &req.Gates[i]
		if g.ID == "" {
			return nil, rejectf("存在缺少 id 的门")
		}
		if _, dup := c.idIndex[g.ID]; dup {
			return nil, rejectf("重复驱动: 线网 %q 被多次定义", g.ID)
		}
		switch g.Type {
		case AND, OR, XOR, NOT:
		default:
			return nil, rejectf("门 %q 的类型 %q 非法", g.ID, g.Type)
		}
		if g.Delay < 1 || g.Delay > 8 {
			return nil, rejectf("门 %q 的延迟 %d 超出 1..8", g.ID, g.Delay)
		}
		// NOT 恰好一个输入；其余至少两个。
		if g.Type == NOT {
			if len(g.Inputs) != 1 {
				return nil, rejectf("NOT 门 %q 必须恰好有 1 个输入，实际 %d 个", g.ID, len(g.Inputs))
			}
		} else if len(g.Inputs) < 2 {
			return nil, rejectf("%s 门 %q 至少需要 2 个输入", g.Type, g.ID)
		}

		cg := &compiledGate{
			typ:   g.Type,
			delay: g.Delay,
		}
		seenInput := make(map[string]bool, len(g.Inputs))
		for _, inID := range g.Inputs {
			if inID == g.ID {
				return nil, rejectf("门 %q 的输入直接引用自身输出", g.ID)
			}
			if seenInput[inID] {
				return nil, rejectf("门 %q 的输入线网 %q 重复", g.ID, inID)
			}
			seenInput[inID] = true
		}
		c.gates[i] = cg
		c.idIndex[g.ID] = len(c.names)
		c.names = append(c.names, g.ID)
		c.isInput = append(c.isInput, false)
	}

	n := len(c.names)

	// 解析输入引用。
	for i, cg := range c.gates {
		g := &req.Gates[i]
		cg.inputs = make([]int, 0, len(g.Inputs))
		for _, inID := range g.Inputs {
			idx, ok := c.idIndex[inID]
			if !ok {
				return nil, rejectf("门 %q 引用了不存在的线网 %q", g.ID, inID)
			}
			cg.inputs = append(cg.inputs, idx)
		}
		cg.index = c.idIndex[g.ID]
	}

	// 校验主输入的翻转事件：时间为正整数、严格升序。
	// 同一主输入同一时刻出现两次翻转即“非法同时翻转”，整份拒绝。
	for i := range req.Inputs {
		in := &req.Inputs[i]
		prev := 0
		for _, ev := range in.Events {
			if ev.At < 1 {
				return nil, rejectf("主输入 %q 的翻转时刻 %d 必须为正整数", in.ID, ev.At)
			}
			if ev.At <= prev {
				return nil, rejectf("主输入 %q 在时刻 %d 存在非法同时/乱序翻转", in.ID, ev.At)
			}
			prev = ev.At
		}
	}

	// 观察点必须全部存在；空观察列表合法（只返回空结果）。
	for _, ob := range req.Observe {
		if _, ok := c.idIndex[ob]; !ok {
			return nil, rejectf("观察点 %q 不存在", ob)
		}
	}

	// 环检测 + 拓扑排序（Kahn 算法）。
	indeg := make([]int, n)        // 按门计数（主输入入度视为 0 不参与）
	dependents := make([][]int, n) // 线网 -> 门输出索引
	for _, cg := range c.gates {
		indeg[cg.index] = len(cg.inputs)
		for _, in := range cg.inputs {
			dependents[in] = append(dependents[in], cg.index)
		}
	}
	// 初始队列：所有主输入。为保证结果确定，按 id 排序处理。
	var ready []int
	for idx, isIn := range c.isInput {
		if isIn {
			ready = append(ready, idx)
		}
	}
	sort.Ints(ready)

	topoGates := make([]*compiledGate, 0, len(c.gates))
	gateByNet := make([]*compiledGate, n)
	for _, cg := range c.gates {
		gateByNet[cg.index] = cg
	}
	for len(ready) > 0 {
		net := ready[0]
		ready = ready[1:]
		if g := gateByNet[net]; g != nil {
			g.topo = len(topoGates)
			topoGates = append(topoGates, g)
		}
		// 后继按 id 确定顺序
		nets := dependents[net]
		sort.Ints(nets)
		for _, outNet := range nets {
			indeg[outNet]--
			if indeg[outNet] == 0 {
				ready = append(ready, outNet)
			}
		}
	}
	if len(topoGates) != len(c.gates) {
		return nil, rejectf("电路中存在组合环")
	}
	c.gates = topoGates

	// 建立反向扇出（按拓扑序，保证后续重算确定）。
	c.fanout = make([][]*compiledGate, n)
	for _, cg := range c.gates {
		for _, in := range cg.inputs {
			c.fanout[in] = append(c.fanout[in], cg)
		}
	}
	for _, fo := range c.fanout {
		sort.Slice(fo, func(a, b int) bool { return fo[a].topo < fo[b].topo })
	}

	return c, nil
}

// evalGate 依据当前线网电平计算门输出。
func (c *circuit) evalGate(g *compiledGate, v []bool) bool {
	switch g.typ {
	case NOT:
		return !v[g.inputs[0]]
	case AND:
		for _, in := range g.inputs {
			if !v[in] {
				return false
			}
		}
		return true
	case OR:
		for _, in := range g.inputs {
			if v[in] {
				return true
			}
		}
		return false
	case XOR:
		r := false
		for _, in := range g.inputs {
			r = r != v[in]
		}
		return r
	}
	return false
}

// initialState 求初始稳态：主输入用初值，门按拓扑序求值。
// 无环组合电路一次拓扑遍历即稳定。
func (c *circuit) initialState() []bool {
	n := len(c.names)
	v := make([]bool, n)
	for i := range c.req.Inputs {
		in := &c.req.Inputs[i]
		v[c.idIndex[in.ID]] = in.Init
	}
	for _, g := range c.gates {
		v[g.index] = c.evalGate(g, v)
	}
	return v
}
