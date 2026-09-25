// Package logic 实现事件驱动的门电路回放器。
//
// 语义要点：
//   - 纯传输延迟（transport delay）：门在某时刻 t 依据输入算出的结果，
//     在 t+delay 时刻到达输出端；后发生的调度绝不取消先前已经排队的事件，
//     因而短脉冲可以穿过多级门（惯性延迟才会过滤窄脉冲）。
//   - 同一时刻的外部翻转与到期的门输出事件先批量生效，
//     随后只重算“本时刻有输入发生变化”的门，并按各自延迟排队后续事件。
//   - 忽略同值重复事件：某时刻排到期的值若与输出端当前值相同，
//     则该到期事件不产生跳变，也不再向扇出传播。
package logic

import "fmt"

// GateType 为支持的门类型。
type GateType string

const (
	AND GateType = "AND"
	OR  GateType = "OR"
	XOR GateType = "XOR"
	NOT GateType = "NOT"
)

// Input 是主输入：id 与各门输出共享同一个线网命名空间。
type Input struct {
	ID     string  `json:"id"`
	Init   bool    `json:"init"`             // t=0 的初值
	Events []Event `json:"events,omitempty"` // 按时间严格升序排列的翻转事件
}

// Event 是主输入在 at 时刻发生的一次翻转（toggle：旧值取反）。
type Event struct {
	At int `json:"at"`
}

// Gate 为单驱动布尔门。Inputs 中每个线网 id 必须唯一。
type Gate struct {
	Type   GateType `json:"type"`
	ID     string   `json:"id"`    // 输出线网 id（单驱动）
	Delay  int      `json:"delay"` // 传输延迟，1..8 个整数时刻
	Inputs []string `json:"inputs"`
}

// Request 为 POST /simulate 接收的 JSON。
type Request struct {
	Inputs      []Input  `json:"inputs"`
	Gates       []Gate   `json:"gates"`
	Observe     []string `json:"observe"`      // 需要输出时间线的观察点
	GlitchWidth int      `json:"glitch_width"` // 脉冲宽度阈值：宽度 < 该值即上报
}

// Jump 是观察点时间线上的一次跳变。
type Jump struct {
	At    int  `json:"at"`
	Value bool `json:"value"`
}

// Pulse 是宽度严格小于阈值的一段电平（正毛刺或负毛刺都报告）。
type Pulse struct {
	Net   string `json:"net"`
	From  int    `json:"from"` // 脉冲开始（含）
	To    int    `json:"to"`   // 脉冲结束（不含），宽度 = To - From
	Width int    `json:"width"`
	Value bool   `json:"value"` // 该窄脉冲期间的电平
}

// Timeline 是单个观察点的完整跳变时间线。
type Timeline struct {
	Net   string `json:"net"`
	Init  bool   `json:"init"`
	Jumps []Jump `json:"jumps"` // 严格按时间升序
}

// Response 为模拟结果。
type Response struct {
	Timelines []Timeline `json:"timelines"`
	Pulses    []Pulse    `json:"pulses"`
}

// ErrReject 表示整份请求被拒绝（重复驱动、组合环、非法同时翻转等）。
// 其余内部错误直接返回普通 error。
type ErrReject struct {
	Reason string
}

func (e *ErrReject) Error() string { return "reject: " + e.Reason }

func rejectf(format string, args ...any) error {
	return &ErrReject{Reason: fmt.Sprintf(format, args...)}
}
