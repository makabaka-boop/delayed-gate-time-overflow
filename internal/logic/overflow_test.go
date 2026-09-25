package logic

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 本组验收围绕“64 位有符号时刻上界”回放：
//   - 上界以内的固定整数时刻必须照常成功，初始稳态、同刻批量生效、
//     传输延迟语义与旧响应格式保持不变；
//   - 任何门输出到期时刻越过 math.MaxInt（2^63-1）时，整次回放必须
//     稳定拒绝（*ErrReject / HTTP 400），不得返回部分或回绕后的成功结果。
//
// 验收用小电路：单门、两级门、同一输入两次翻转。

// maxT 即协议支持的时刻上界（目标平台 int 为 64 位，math.MaxInt == 2^63-1）。
const maxT = math.MaxInt

// assertSaneTimeline 校验一条时间线：时刻非负且严格升序（单调性），
// 相邻跳变构成的区间宽度必须等于 to-from 且为正（脉冲起止不互相矛盾）。
func assertSaneTimeline(t *testing.T, tl Timeline) {
	t.Helper()
	for i, j := range tl.Jumps {
		if j.At < 0 {
			t.Fatalf("线网 %s 出现负时刻 %d：时刻越界回绕", tl.Net, j.At)
		}
		if i > 0 {
			prev := tl.Jumps[i-1]
			if j.At <= prev.At {
				t.Fatalf("线网 %s 跳变非严格升序: %d 后接 %d", tl.Net, prev.At, j.At)
			}
			if j.At-prev.At <= 0 {
				t.Fatalf("线网 %s 区间宽度非正: [%d,%d)", tl.Net, prev.At, j.At)
			}
		}
	}
}

// assertSanePulses 校验脉冲与时间线一致：from<to、width==to-from，
// 且 (from,value)/(to) 恰好是时间线上相邻的两条跳变。
func assertSanePulses(t *testing.T, resp *Response) {
	t.Helper()
	jumpsOf := map[string][]Jump{}
	for _, tl := range resp.Timelines {
		jumpsOf[tl.Net] = tl.Jumps
		assertSaneTimeline(t, tl)
	}
	for _, p := range resp.Pulses {
		if p.From < 0 || p.To < 0 {
			t.Fatalf("脉冲出现负时刻: %+v", p)
		}
		if !(p.From < p.To) {
			t.Fatalf("脉冲起止矛盾（from 必须 < to）: %+v", p)
		}
		if p.Width != p.To-p.From || p.Width <= 0 {
			t.Fatalf("脉冲宽度与起止不一致: %+v", p)
		}
		js := jumpsOf[p.Net]
		found := false
		for i := 0; i+1 < len(js); i++ {
			if js[i].At == p.From && js[i+1].At == p.To && js[i].Value == p.Value {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("脉冲 %+v 在 %s 时间线上找不到对应相邻跳变 %v", p, p.Net, js)
		}
	}
}

func assertRejected(t *testing.T, req *Request) {
	t.Helper()
	resp, err := Run(req)
	if err == nil {
		t.Fatalf("越界回放必须整份拒绝，却返回成功: %+v", resp)
	}
	if resp != nil {
		t.Fatalf("拒绝时不得返回部分结果, got %+v", resp)
	}
	var rej *ErrReject
	if !errors.As(err, &rej) {
		t.Fatalf("期望 *ErrReject, got %T (%v)", err, err)
	}
	// 稳定性：再次回放必须得到同样的拒绝。
	if _, err2 := Run(req); err2 == nil || err2.Error() != err.Error() {
		t.Fatalf("越界回放拒绝不稳定: %v vs %v", err, err2)
	}
}

// TestTimeBoundSingleGate 单级门：正延迟把理论跳变推过上界。
func TestTimeBoundSingleGate(t *testing.T) {
	// 上界之外：输入在上界处翻转，NOT(delay=1) 的输出到期时刻 = 上界+1。
	outside := &Request{
		Inputs:  []Input{{ID: "a", Init: false, Events: []Event{{At: maxT}}}},
		Gates:   []Gate{gate(NOT, "w", 1, "a")},
		Observe: []string{"a", "w"},
	}
	assertRejected(t, outside)

	// 上界之内：输入在上界-1 翻转，输出恰好落在上界，必须成功且单调。
	inside := &Request{
		Inputs:      []Input{{ID: "a", Init: false, Events: []Event{{At: maxT - 1}}}},
		Gates:       []Gate{gate(NOT, "w", 1, "a")},
		Observe:     []string{"a", "w"},
		GlitchWidth: 2,
	}
	resp, err := Run(inside)
	if err != nil {
		t.Fatalf("上界内回放应成功: %v", err)
	}
	assertSanePulses(t, resp)
	want := map[string][]Jump{
		"a": {{At: maxT - 1, Value: true}},
		"w": {{At: maxT, Value: false}},
	}
	for _, tl := range resp.Timelines {
		js, ok := want[tl.Net]
		if !ok {
			t.Fatalf("意外的时间线 %s", tl.Net)
		}
		if len(tl.Jumps) != len(js) || tl.Jumps[0] != js[0] {
			t.Fatalf("%s 跳变不符: got %+v want %+v", tl.Net, tl.Jumps, js)
		}
	}
}

// TestTimeBoundTwoStages 两级门：上游跳变仍在正时刻、下游才越界时，
// 旧实现会把回绕成负值的下游事件经事件堆排在上游之前（因果倒置）。
func TestTimeBoundTwoStages(t *testing.T) {
	twoStages := func(at int) *Request {
		return &Request{
			Inputs: []Input{{ID: "a", Init: false, Events: []Event{{At: at}}}},
			Gates: []Gate{
				gate(NOT, "g1", 1, "a"),
				gate(NOT, "g2", 1, "g1"),
			},
			Observe:     []string{"a", "g1", "g2"},
			GlitchWidth: 3,
		}
	}

	// 上界之外：a=上界-1 -> g1=上界（正）-> g2=上界+1（越界）：整份拒绝。
	assertRejected(t, twoStages(maxT-1))

	// 上界之内：a=上界-2 -> g1=上界-1 -> g2=上界，全部为正，因果次序保持。
	resp, err := Run(twoStages(maxT - 2))
	if err != nil {
		t.Fatalf("上界内两级回放应成功: %v", err)
	}
	assertSanePulses(t, resp)
	atOf := map[string]int{}
	for _, tl := range resp.Timelines {
		if len(tl.Jumps) != 1 {
			t.Fatalf("%s 应恰有一条跳变: %+v", tl.Net, tl.Jumps)
		}
		atOf[tl.Net] = tl.Jumps[0].At
	}
	if !(atOf["a"] < atOf["g1"] && atOf["g1"] < atOf["g2"]) {
		t.Fatalf("因果次序被破坏: a=%d g1=%d g2=%d", atOf["a"], atOf["g1"], atOf["g2"])
	}
	if atOf["a"] != maxT-2 || atOf["g1"] != maxT-1 || atOf["g2"] != maxT {
		t.Fatalf("时刻不符: %+v", atOf)
	}
}

// TestTimeBoundTwoToggles 同一输入在上界附近连续翻转，
// 只有后一次的门输出越界时不得交付“先大正、后小负”的矛盾时间线/脉冲。
func TestTimeBoundTwoToggles(t *testing.T) {
	// 上界之外：两次翻转 上界-2、上界；NOT(delay=1) 的第一批输出落在
	// 上界-1（正），最后一次重算的输出落在 上界+1（越界）——旧实现下
	// 时间线会变成“大正时刻后接回绕负时刻”，必须整份拒绝。
	outside := &Request{
		Inputs: []Input{{
			ID:     "a",
			Init:   false,
			Events: []Event{{At: maxT - 2}, {At: maxT}},
		}},
		Gates:       []Gate{gate(NOT, "w", 1, "a")},
		Observe:     []string{"w"},
		GlitchWidth: 2,
	}
	assertRejected(t, outside)

	// 上界之内：两次翻转 上界-3、上界-2；输出区间 [上界-2, 上界-1)，
	// 宽 1 的低电平窄脉冲，起止与宽度自洽。
	inside := &Request{
		Inputs: []Input{{
			ID:     "a",
			Init:   false,
			Events: []Event{{At: maxT - 3}, {At: maxT - 2}},
		}},
		Gates:       []Gate{gate(NOT, "w", 1, "a")},
		Observe:     []string{"w"},
		GlitchWidth: 2,
	}
	resp, err := Run(inside)
	if err != nil {
		t.Fatalf("上界内两次翻转应成功: %v", err)
	}
	assertSanePulses(t, resp)
	tlW := resp.Timelines[0]
	if len(tlW.Jumps) != 2 ||
		tlW.Jumps[0].At != maxT-2 || tlW.Jumps[0].Value != false ||
		tlW.Jumps[1].At != maxT-1 || tlW.Jumps[1].Value != true {
		t.Fatalf("w 跳变不符: %+v", tlW.Jumps)
	}
	if len(resp.Pulses) != 1 {
		t.Fatalf("应有一个宽度1的低电平窄脉冲: %+v", resp.Pulses)
	}
	p := resp.Pulses[0]
	if p.Net != "w" || p.From != maxT-2 || p.To != maxT-1 || p.Width != 1 || p.Value {
		t.Fatalf("脉冲字段错误: %+v", p)
	}
}

// TestTimeBoundHTTP 端点对越界回放返回 400 且只有 error 字段；
// 上界内返回 200。
func TestTimeBoundHTTP(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	post := func(body string) (int, []byte) {
		resp, err := http.Post(srv.URL+"/simulate", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}

	outside := `{
	  "inputs": [{"id":"a","init":false,"events":[{"at":9223372036854775807}]}],
	  "gates": [{"type":"NOT","id":"w","delay":1,"inputs":["a"]}],
	  "observe": ["w"]
	}`
	if code, raw := post(outside); code != http.StatusBadRequest {
		t.Fatalf("越界应 400, got %d body=%s", code, raw)
	} else {
		var eb map[string]string
		if err := json.Unmarshal(raw, &eb); err != nil || eb["error"] == "" {
			t.Fatalf("400 响应应为 {\"error\":...}: %s", raw)
		}
	}

	inside := `{
	  "inputs": [{"id":"a","init":false,"events":[{"at":9223372036854775806}]}],
	  "gates": [{"type":"NOT","id":"w","delay":1,"inputs":["a"]}],
	  "observe": ["w"]
	}`
	if code, raw := post(inside); code != http.StatusOK {
		t.Fatalf("上界内应 200, got %d body=%s", code, raw)
	} else {
		var out Response
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Timelines) != 1 || len(out.Timelines[0].Jumps) != 1 ||
			out.Timelines[0].Jumps[0].At != math.MaxInt {
			t.Fatalf("上界内响应跳变不符: %s", raw)
		}
	}
}

// TestNormalTimeResponseUnchanged 普通时间输入的旧响应格式与内容保持不变
// （字节级锁定：字段、顺序、初值、跳变、脉冲）。
func TestNormalTimeResponseUnchanged(t *testing.T) {
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
	raw, _ := io.ReadAll(resp.Body)
	want := `{"timelines":[{"net":"w","init":true,"jumps":[{"at":2,"value":false},{"at":3,"value":true}]}],"pulses":[{"net":"w","from":2,"to":3,"width":1,"value":false}]}` + "\n"
	if string(raw) != want {
		t.Fatalf("普通时间响应发生变化:\n got  %s\n want %s", raw, want)
	}
}
