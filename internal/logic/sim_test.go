package logic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func gate(t GateType, id string, delay int, ins ...string) Gate {
	return Gate{Type: t, ID: id, Delay: delay, Inputs: ins}
}

// assertEdges 检查某根线网的完整跳变时间线。
func assertEdges(t *testing.T, c *circuit, res *simResult, net string, want []Jump) {
	t.Helper()
	idx, ok := c.idIndex[net]
	if !ok {
		t.Fatalf("线网 %s 不存在", net)
	}
	got := res.edges[idx]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s 时间线不符:\n got  %v\n want %v", net, got, want)
	}
}

// TestReconvergentGlitch 重汇路径产生短脉冲：
// A 走一条带延迟的路径 g1，同时直接接到 XOR 另一输入。
// hold 恒为 1（OR(B, NOT(B))），所以 g1 = AND(A, hold) 稳态等于 A，但晚一拍。
// A 在 t=1 翻转：z 在 t=2 变 1、t=3 变回 0，形成宽 1 的正脉冲。
func TestReconvergentGlitch(t *testing.T) {
	req := &Request{
		Inputs: []Input{
			{ID: "A", Init: false, Events: []Event{{At: 1}}},
			{ID: "B", Init: false},
		},
		Gates: []Gate{
			gate(NOT, "nb", 1, "B"),
			gate(OR, "hold", 1, "B", "nb"), // 恒 1，初始稳态下无上电毛刺
			gate(AND, "g1", 1, "A", "hold"),
			gate(XOR, "z", 1, "g1", "A"),
		},
		Observe:     []string{"z"},
		GlitchWidth: 2,
	}

	c, err := compile(req)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res, errSim := c.simulate()
	if errSim != nil {
		t.Fatalf("simulate: %v", errSim)
	}

	assertEdges(t, c, res, "z", []Jump{{At: 2, Value: true}, {At: 3, Value: false}})

	resp := c.buildResponse(res)
	var gotPulse *Pulse
	for i := range resp.Pulses {
		if resp.Pulses[i].Net == "z" {
			gotPulse = &resp.Pulses[i]
		}
	}
	if gotPulse == nil {
		t.Fatalf("未在 z 上检测到宽度 < 2 的脉冲: %+v", resp.Pulses)
	}
	if gotPulse.Width != 1 || !gotPulse.Value || gotPulse.From != 2 || gotPulse.To != 3 {
		t.Fatalf("脉冲字段错误: %+v", gotPulse)
	}

	// 阈值取 1 时宽度 1 不满足“严格小于”，不应上报。
	req.GlitchWidth = 1
	if resp2, _ := Run(req); len(resp2.Pulses) != 0 {
		t.Fatalf("阈值=1 时宽度1不应上报, got %+v", resp2.Pulses)
	}

	// 初值回归：A/g1 稳态为 0、z 稳态为 0；init 不得被终态污染。
	resp3, _ := Run(req)
	for _, tl := range resp3.Timelines {
		if tl.Net == "z" && tl.Init != false {
			t.Fatalf("z init 应为 false, got %v", tl.Init)
		}
	}
}

// TestSameTickCancellation 同刻抵消：
// p、q 同为 1，t=1 同时翻成 0，XOR 两个输入同拍变化、前后都相等，输出始终为 0。
func TestSameTickCancellation(t *testing.T) {
	req := &Request{
		Inputs: []Input{
			{ID: "p", Init: true, Events: []Event{{At: 1}}},
			{ID: "q", Init: true, Events: []Event{{At: 1}}},
		},
		Gates: []Gate{
			gate(XOR, "x", 2, "p", "q"),
		},
		Observe:     []string{"x"},
		GlitchWidth: 3,
	}
	c, err := compile(req)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res, errSim := c.simulate()
	if errSim != nil {
		t.Fatalf("simulate: %v", errSim)
	}
	assertEdges(t, c, res, "x", nil)
}

// TestEqualDelayReconvergeNoGlitch 两条路径延迟相同：重汇处输入同时翻转，无毛刺。
func TestEqualDelayReconvergeNoGlitch(t *testing.T) {
	// a->n1(NOT,1)->z(AND,1); a->n2(NOT,1)->z。
	// z = AND(NOT a, NOT a) = NOT a；两路同刻到达，只会整体翻转一次。
	req := &Request{
		Inputs: []Input{{ID: "a", Init: false, Events: []Event{{At: 4}}}},
		Gates: []Gate{
			gate(NOT, "n1", 1, "a"),
			gate(NOT, "n2", 1, "a"),
			gate(AND, "z", 1, "n1", "n2"),
		},
		Observe:     []string{"z", "n1", "n2"},
		GlitchWidth: 5,
	}
	c, err := compile(req)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res, errSim := c.simulate()
	if errSim != nil {
		t.Fatalf("simulate: %v", errSim)
	}
	assertEdges(t, c, res, "n1", []Jump{{At: 5, Value: false}})
	assertEdges(t, c, res, "n2", []Jump{{At: 5, Value: false}})
	assertEdges(t, c, res, "z", []Jump{{At: 6, Value: false}})
	if resp := c.buildResponse(res); len(resp.Pulses) != 0 {
		t.Fatalf("等延迟重汇不应有脉冲: %+v", resp.Pulses)
	}
}

// TestMultiGatePropagation 跨多个门、延迟各异的链式传播。
func TestMultiGatePropagation(t *testing.T) {
	req := &Request{
		Inputs: []Input{{ID: "a", Init: false, Events: []Event{{At: 1}, {At: 2}}}},
		Gates: []Gate{
			gate(NOT, "g1", 1, "a"),
			gate(NOT, "g2", 3, "g1"),
			gate(NOT, "g3", 2, "g2"),
		},
		Observe:     []string{"a", "g1", "g2", "g3"},
		GlitchWidth: 10,
	}
	c, err := compile(req)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res, errSim := c.simulate()
	if errSim != nil {
		t.Fatalf("simulate: %v", errSim)
	}
	// a: t1=1 t2=0。
	// g1=!a 延迟1：t2=0、t3=1。
	// g2=!g1：在 t2 见 g1=0 排 t5=1；在 t3 见 g1=1 排 t6=0。
	// g3=!g2 延迟2：t7=0、t8=1。
	assertEdges(t, c, res, "a", []Jump{{At: 1, Value: true}, {At: 2, Value: false}})
	assertEdges(t, c, res, "g1", []Jump{{At: 2, Value: false}, {At: 3, Value: true}})
	assertEdges(t, c, res, "g2", []Jump{{At: 5, Value: true}, {At: 6, Value: false}})
	assertEdges(t, c, res, "g3", []Jump{{At: 7, Value: false}, {At: 8, Value: true}})
}

// TestTransportDoesNotCancel 关键传输延迟性质：
// 输入端相邻两拍 0->1->0，宽 1 的脉冲必须穿过 NOT 门（延迟1），
// 即门输出 t2=0、t3=1。若错误地“后来变化取消前一待发事件”，t2 的事件会丢。
func TestTransportDoesNotCancel(t *testing.T) {
	req := &Request{
		Inputs: []Input{{ID: "a", Init: false, Events: []Event{{At: 1}, {At: 2}}}},
		Gates: []Gate{
			gate(NOT, "w", 1, "a"),
		},
		Observe:     []string{"w"},
		GlitchWidth: 2,
	}
	c, err := compile(req)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res, errSim := c.simulate()
	if errSim != nil {
		t.Fatalf("simulate: %v", errSim)
	}
	assertEdges(t, c, res, "w", []Jump{{At: 2, Value: false}, {At: 3, Value: true}})
	resp := c.buildResponse(res)
	if len(resp.Pulses) != 1 || resp.Pulses[0].Width != 1 || resp.Pulses[0].Value {
		t.Fatalf("应检出一个宽度1的低电平脉冲, got %+v", resp.Pulses)
	}
}

// TestSameValueEventIgnored 同值到期事件不产生跳变：
// 先让门输出排一个“变回当前值”的事件，期间无其它路径改变输出，
// 到期时必须被忽略，时间线上不出现同值重复。
func TestSameValueEventIgnored(t *testing.T) {
	// z = OR(a, hold)，hold 恒 1：a 翻转前后 z 恒为 1。
	// 受影响的 z 每拍仍排队到期事件（传输语义），但到期值=当前值，必须忽略。
	req := &Request{
		Inputs: []Input{
			{ID: "a", Init: false, Events: []Event{{At: 3}}},
			{ID: "b", Init: true},
		},
		Gates: []Gate{
			gate(NOT, "nb", 1, "b"),
			gate(OR, "hold", 1, "b", "nb"),
			gate(OR, "z", 2, "a", "hold"),
		},
		Observe: []string{"z", "hold", "nb"},
	}
	c, err := compile(req)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res, errSim := c.simulate()
	if errSim != nil {
		t.Fatalf("simulate: %v", errSim)
	}
	if es := res.edges[c.idIndex["z"]]; len(es) != 0 {
		t.Fatalf("z 应始终为 1，不应有跳变: %v", es)
	}
	if !res.initV[c.idIndex["z"]] {
		t.Fatalf("z 初态应为 1")
	}
}

// TestInitialSteadyState 初始稳态在 t=0 前已按组合逻辑收敛，无“上电”跳变。
func TestInitialSteadyState(t *testing.T) {
	req := &Request{
		Inputs: []Input{{ID: "a", Init: true}},
		Gates: []Gate{
			gate(NOT, "n1", 1, "a"),
			gate(AND, "z", 8, "n1", "a"), // 初态 a=1,n1=0 -> z=0，两个不同输入
		},
		Observe: []string{"n1", "z"},
	}
	c, err := compile(req)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res, errSim := c.simulate()
	if errSim != nil {
		t.Fatalf("simulate: %v", errSim)
	}
	if res.initV[c.idIndex["n1"]] != false || res.initV[c.idIndex["z"]] != false {
		t.Fatalf("初始稳态错误: n1=%v z=%v", res.initV[c.idIndex["n1"]], res.initV[c.idIndex["z"]])
	}
	if len(res.edges[c.idIndex["n1"]]) != 0 || len(res.edges[c.idIndex["z"]]) != 0 {
		t.Fatalf("无外部变化时不应有任何跳变")
	}
	resp, _ := Run(req)
	// n1 = NOT(a)，a 初值 true -> n1 初值 false；z = AND(n1,a) -> false。
	n1tl, ztl := resp.Timelines[0], resp.Timelines[1]
	if n1tl.Net != "n1" || n1tl.Init != false || len(n1tl.Jumps) != 0 {
		t.Fatalf("n1 时间线异常: %+v", n1tl)
	}
	if ztl.Net != "z" || ztl.Init != false || len(ztl.Jumps) != 0 {
		t.Fatalf("z 时间线异常: %+v", ztl)
	}
}

// TestTimelineInitNotFinal 回归：观察点的 init 必须是初始稳态，
// 不能被模拟结束时的当前电平（终态）污染。
func TestTimelineInitNotFinal(t *testing.T) {
	req := &Request{
		// a 初始 0、最终 1；w=NOT(a) 初始 1、最终 0。
		Inputs:  []Input{{ID: "a", Init: false, Events: []Event{{At: 1}}}},
		Gates:   []Gate{gate(NOT, "w", 1, "a")},
		Observe: []string{"a", "w"},
	}
	resp, err := Run(req)
	if err != nil {
		t.Fatal(err)
	}
	initOf := func(net string) bool {
		for _, tl := range resp.Timelines {
			if tl.Net == net {
				return tl.Init
			}
		}
		t.Fatalf("缺少 %s", net)
		return false
	}
	if initOf("a") != false {
		t.Fatalf("a.init 应为 false（被终态污染？）")
	}
	if initOf("w") != true {
		t.Fatalf("w.init 应为 true（被终态污染？）")
	}
}

// ---- 对拍：事件驱动引擎 vs 独立逐时刻参考模拟器 ----

func crossCheck(t *testing.T, req *Request) {
	t.Helper()
	c, err := compile(req)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, errSim := c.simulate()
	if errSim != nil {
		t.Fatalf("simulate: %v", errSim)
	}

	ref := refBuild(t, req)
	ref.run()

	if len(got.edges) != len(ref.edges) {
		t.Fatalf("线网数量不一致: %d vs %d", len(got.edges), len(ref.edges))
	}
	for idx := range got.edges {
		name := c.names[idx]
		ridx, ok := ref.idx[name]
		if !ok {
			t.Fatalf("参考模拟器缺少线网 %s", name)
		}
		g, w := got.edges[idx], ref.edges[ridx]
		if len(g) == 0 {
			g = nil
		}
		if len(w) == 0 {
			w = nil
		}
		if !reflect.DeepEqual(g, w) {
			t.Fatalf("线网 %s 对拍不一致:\n engine     %v\n reference  %v", name, g, w)
		}
		// 初始稳态快照一致。
		if got.initV[idx] != ref.initV[ridx] {
			t.Fatalf("线网 %s 初始稳态不一致: engine=%v reference=%v", name, got.initV[idx], ref.initV[ridx])
		}
		// 终态 = 初态叠加全部跳变边；与参考模拟器回放结束时电平一致。
		final := got.initV[idx]
		if len(g) > 0 {
			final = g[len(g)-1].Value
		}
		if final != ref.v[ridx] {
			t.Fatalf("线网 %s 最终电平不一致: engine=%v reference=%v", name, final, ref.v[ridx])
		}
	}
}

func TestCrossCheckHandcrafted(t *testing.T) {
	cases := []*Request{
		// 重汇毛刺
		{
			Inputs: []Input{
				{ID: "A", Init: false, Events: []Event{{At: 1}}},
				{ID: "B", Init: false},
			},
			Gates: []Gate{
				gate(NOT, "nb", 1, "B"),
				gate(OR, "hold", 1, "B", "nb"),
				gate(AND, "g1", 1, "A", "hold"),
				gate(XOR, "z", 1, "g1", "A"),
			},
		},
		// 同刻抵消 + 多输入
		{
			Inputs: []Input{
				{ID: "p", Init: true, Events: []Event{{At: 1}}},
				{ID: "q", Init: true, Events: []Event{{At: 1}}},
				{ID: "r", Init: false, Events: []Event{{At: 1}, {At: 4}}},
			},
			Gates: []Gate{
				gate(XOR, "x", 2, "p", "q", "r"),
				gate(AND, "y", 3, "p", "r"),
				gate(OR, "zz", 1, "x", "y"),
			},
		},
		// 多级链
		{
			Inputs: []Input{{ID: "a", Init: false, Events: []Event{{At: 1}, {At: 2}, {At: 10}}}},
			Gates: []Gate{
				gate(NOT, "g1", 1, "a"),
				gate(NOT, "g2", 3, "g1"),
				gate(NOT, "g3", 2, "g2"),
				gate(AND, "g4", 8, "g3", "a"),
			},
		},
		// 无任何外部事件
		{
			Inputs: []Input{{ID: "a", Init: true}, {ID: "b", Init: false}},
			Gates: []Gate{
				gate(XOR, "x", 1, "a", "b"),
				gate(NOT, "y", 2, "x"),
			},
		},
	}
	for i, req := range cases {
		t.Run(fmt.Sprintf("case%d", i), func(t *testing.T) { crossCheck(t, req) })
	}
}

// genRandomDAG 随机构造一个无环门电路（每条新门的输入只引用更早出现的线网）。
func genRandomDAG(rng *rand.Rand) *Request {
	types := []GateType{AND, OR, XOR}
	nInputs := 1 + rng.Intn(4)
	req := &Request{}
	avail := []string{}
	for i := 0; i < nInputs; i++ {
		id := fmt.Sprintf("i%d", i)
		in := Input{ID: id, Init: rng.Intn(2) == 0}
		// 0~3 个翻转，时间允许不同输入同刻（合法）。
		nev := rng.Intn(4)
		prev := 0
		for k := 0; k < nev; k++ {
			prev += 1 + rng.Intn(4)
			in.Events = append(in.Events, Event{At: prev})
		}
		req.Inputs = append(req.Inputs, in)
		avail = append(avail, id)
	}
	nGates := 1 + rng.Intn(20)
	for i := 0; i < nGates; i++ {
		id := fmt.Sprintf("g%d", i)
		// 可用输入不足两个时先放一个 NOT，保证 AND/OR/XOR 始终有合法扇入。
		if len(avail) < 2 {
			req.Gates = append(req.Gates, gate(NOT, id, 1+rng.Intn(8), avail[rng.Intn(len(avail))]))
			avail = append(avail, id)
			continue
		}
		gt := types[rng.Intn(len(types))]
		arity := 2 + rng.Intn(2) // 2 或 3 输入
		if arity > len(avail) {
			arity = len(avail)
		}
		// 从已有线网中不重复抽取。
		perm := rng.Perm(len(avail))[:arity]
		ins := make([]string, arity)
		for k, p := range perm {
			ins[k] = avail[p]
		}
		req.Gates = append(req.Gates, gate(gt, id, 1+rng.Intn(8), ins...))
		avail = append(avail, id)
	}
	return req
}

func TestCrossCheckRandomDAGs(t *testing.T) {
	rng := rand.New(rand.NewSource(20260925))
	for iter := 0; iter < 400; iter++ {
		req := genRandomDAG(rng)
		crossCheck(t, req)
	}
}

// 含 NOT 门的随机电路：NOT 单输入，其余门至少两输入。
func genRandomDAGWithNot(rng *rand.Rand) *Request {
	req := genRandomDAG(rng)
	// 把若干门替换为 NOT，输入截断为 1。
	for i := range req.Gates {
		if rng.Intn(3) == 0 {
			req.Gates[i].Type = NOT
			req.Gates[i].Inputs = req.Gates[i].Inputs[:1]
		}
	}
	return req
}

func TestCrossCheckRandomWithNot(t *testing.T) {
	rng := rand.New(rand.NewSource(424242))
	for iter := 0; iter < 300; iter++ {
		req := genRandomDAGWithNot(rng)
		crossCheck(t, req)
	}
}

// TestPulsesCrossCheckRandom 随机电路下，用参考模拟器的边列表独立计算窄脉冲，
// 与引擎的 buildResponse 对拍（阈值随机，覆盖宽度边界）。
func TestPulsesCrossCheckRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	for iter := 0; iter < 300; iter++ {
		req := genRandomDAGWithNot(rng)
		req.GlitchWidth = 1 + rng.Intn(6)
		c, err := compile(req)
		if err != nil {
			t.Fatal(err)
		}
		res, errSim := c.simulate()
		if errSim != nil {
			t.Fatalf("simulate: %v", errSim)
		}
		// 观察全部线网。
		req.Observe = append(req.Observe, c.names...)
		resp := c.buildResponse(res)

		// 独立期望：枚举所有线网相邻边，键为 (线网, 起始时刻)。
		type pk struct {
			net  string
			from int
		}
		wantM := map[pk]Pulse{}
		for idx, es := range res.edges {
			for i := 0; i+1 < len(es); i++ {
				w := es[i+1].At - es[i].At
				if w < req.GlitchWidth {
					p := Pulse{Net: c.names[idx], From: es[i].At, To: es[i+1].At, Width: w, Value: es[i].Value}
					wantM[pk{p.Net, p.From}] = p
				}
			}
		}
		gotM := map[pk]Pulse{}
		for _, p := range resp.Pulses {
			gotM[pk{p.Net, p.From}] = p
		}
		if !reflect.DeepEqual(gotM, wantM) {
			t.Fatalf("iter=%d 阈值=%d 脉冲集合不一致:\n got %v\n want %v", iter, req.GlitchWidth, gotM, wantM)
		}
	}
}

// ---- 整份拒绝 ----

func TestRejections(t *testing.T) {
	base := func() *Request {
		return &Request{
			Inputs: []Input{{ID: "a", Init: false}, {ID: "b", Init: false}},
			Gates:  []Gate{gate(AND, "z", 1, "a", "b")},
		}
	}
	check := func(name string, mutate func(*Request)) {
		t.Helper()
		req := base()
		mutate(req)
		if _, err := Run(req); err == nil {
			t.Fatalf("%s: 期望拒绝但通过", name)
		} else if _, ok := err.(*ErrReject); !ok {
			t.Fatalf("%s: 期望 *ErrReject, got %T (%v)", name, err, err)
		}
	}

	check("重复驱动-门撞输入", func(r *Request) { r.Gates[0].ID = "a" })
	check("重复驱动-两门同输出", func(r *Request) {
		r.Gates = append(r.Gates, gate(OR, "z", 2, "a", "b"))
	})
	check("组合环", func(r *Request) {
		r.Gates = []Gate{
			gate(AND, "x", 1, "a", "y"),
			gate(OR, "y", 1, "x", "b"),
		}
	})
	check("自环", func(r *Request) {
		r.Gates[0].Inputs = []string{"a", "z"}
	})
	check("非法门类型", func(r *Request) { r.Gates[0].Type = "NAND" })
	check("非法延迟0", func(r *Request) { r.Gates[0].Delay = 0 })
	check("非法延迟9", func(r *Request) { r.Gates[0].Delay = 9 })
	check("NOT输入数错", func(r *Request) {
		r.Gates[0].Type = NOT
	})
	check("AND输入不足", func(r *Request) {
		r.Gates[0].Inputs = []string{"a"}
	})
	check("输入重复扇入", func(r *Request) {
		r.Gates[0].Inputs = []string{"a", "a"}
	})
	check("引用不存在线网", func(r *Request) {
		r.Gates[0].Inputs = []string{"a", "ghost"}
	})
	check("主输入同刻翻转", func(r *Request) {
		r.Inputs[0].Events = []Event{{At: 5}, {At: 5}}
	})
	check("翻转事件乱序", func(r *Request) {
		r.Inputs[0].Events = []Event{{At: 5}, {At: 3}}
	})
	check("翻转时刻非正", func(r *Request) {
		r.Inputs[0].Events = []Event{{At: 0}}
	})
	check("观察点不存在", func(r *Request) { r.Observe = []string{"nope"} })
	check("门数超40", func(r *Request) {
		r.Gates = r.Gates[:0]
		for i := 0; i < 41; i++ {
			// 链式无环
			prev := "a"
			if i > 0 {
				prev = fmt.Sprintf("x%d", i-1)
			}
			r.Gates = append(r.Gates, gate(AND, fmt.Sprintf("x%d", i), 1, prev, "b"))
		}
	})
}

func TestAcceptLegal(t *testing.T) {
	// 不同主输入同一时刻翻转合法。
	req := &Request{
		Inputs: []Input{
			{ID: "a", Init: false, Events: []Event{{At: 7}}},
			{ID: "b", Init: false, Events: []Event{{At: 7}}},
		},
		Gates: []Gate{gate(XOR, "z", 1, "a", "b")},
	}
	if _, err := Run(req); err != nil {
		t.Fatalf("不同输入同刻翻转应合法: %v", err)
	}
}

// ---- HTTP 服务 ----

func TestHTTPEndpoint(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	body := `{
	  "inputs": [{"id":"a","init":false,"events":[{"at":1},{"at":2}]}],
	  "gates": [{"type":"NOT","id":"w","delay":1,"inputs":["a"]}],
	  "observe": ["w"],
	  "glitch_width": 2
	}`
	resp, err := http.Post(srv.URL+"/simulate", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Timelines) != 1 || out.Timelines[0].Net != "w" {
		t.Fatalf("时间线异常: %+v", out.Timelines)
	}
	wantJumps := []Jump{{At: 2, Value: false}, {At: 3, Value: true}}
	if !reflect.DeepEqual(out.Timelines[0].Jumps, wantJumps) {
		t.Fatalf("跳变不符: %+v", out.Timelines[0].Jumps)
	}
	if len(out.Pulses) != 1 || out.Pulses[0].Width != 1 {
		t.Fatalf("脉冲不符: %+v", out.Pulses)
	}

	// 非法电路 -> 400
	bad := `{"inputs":[],"gates":[{"type":"AND","id":"z","delay":1,"inputs":["z"]}]}`
	resp2, err := http.Post(srv.URL+"/simulate", "application/json", bytes.NewBufferString(bad))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法电路应 400, got %d", resp2.StatusCode)
	}
	var errBody map[string]string
	if err := json.NewDecoder(resp2.Body).Decode(&errBody); err != nil {
		t.Fatal(err)
	}
	if errBody["error"] == "" {
		t.Fatalf("错误响应缺少 error 字段")
	}

	// 坏 JSON -> 400
	resp3, _ := http.Post(srv.URL+"/simulate", "application/json", bytes.NewBufferString("{bad"))
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("坏 JSON 应 400, got %d", resp3.StatusCode)
	}
	resp3.Body.Close()

	// GET 不允许 -> 405
	resp4, _ := http.Get(srv.URL + "/simulate")
	if resp4.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应 405, got %d", resp4.StatusCode)
	}
	resp4.Body.Close()
}
