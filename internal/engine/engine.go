// Package engine 把追加日志中的事件折叠为确定的点位状态。
//
// 关键性质（对应验收要求“重放乱序事件，观察到稳定的点位状态和唯一告警”）：
//
//  1. 纯内容定序：折叠结果只依赖事件集合与评估时刻 asOf，与接收顺序无关。
//     同一批事件以任意顺序（甚至任意版本号分配）重放，输出逐字段一致。
//     定序规则为 (occ_time, event_id) 全序，杜绝同分歧义。
//  2. 迟到数据：occ_time 早、接收晚的计数只要 occ_time ≤ asOf 就照常纳入，
//     并在数据依据中标记 late=true，质量标记置为 late_arrivals。
//  3. 计数修正：corrects_event_id 指向的旧批次整体作废，修正批次按“被修正
//     批次的发生时刻”计入，占用重建不跳变。
//  4. 重复上报：存储层按 event_id 去重；引擎再按 (device_id, seq) 去掉
//     “换了 event_id 的重发”，并在排除清单中说明原因。
//  5. 权威锚点：取 occ_time ≤ asOf 的最新权威快照为锚，其后有效增量求和；
//     早于锚点的计数不再计入占用（但保留在依据中）。
//  6. 跨午夜：一切状态以 session_id 隔离，场次之间不串数；clear_session 清零。
package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"

	"github.com/beryl0222/city-second-screen/model"
)

// Config 控制新鲜度、阈值与迟到判定，均有默认值。
type Config struct {
	FreshThreshold time.Duration // 最新数据晚于 asOf 该时长内算 fresh
	GapThreshold   time.Duration // 超过则标记 stale_gap
	StaleThreshold time.Duration // 超过则占用不可采信（stale）
	LateThreshold  time.Duration // receive-occtime 超过该值视为迟到后补
	RecoverWindow  time.Duration // 失联恢复标记的保留窗口
	CrowdRatio     float64       // 触发导流建议的占用率
	HoldRatio      float64       // 触发暂停入场的占用率
}

// DefaultConfig 给出赛事场景的默认口径（计数心跳约 30s 一次）。
func DefaultConfig() Config {
	return Config{
		FreshThreshold: 60 * time.Second,
		GapThreshold:   180 * time.Second,
		StaleThreshold: 300 * time.Second,
		LateThreshold:  60 * time.Second,
		RecoverWindow:  5 * time.Minute,
		CrowdRatio:     0.75,
		HoldRatio:      0.90,
	}
}

// Heat 热度分区取值。
const (
	HeatUnknown = "未知"
	HeatCalm    = "舒适"
	HeatBusy    = "繁忙"
	HeatCrowded = "拥挤"
	HeatFull    = "饱和"
	HeatControl = "管控中"
)

// BasisPoint 是一条“被用于判断的有效数据”。
type BasisPoint struct {
	EventID   string    `json:"event_id"`
	Type      string    `json:"type"`
	VenueID   string    `json:"venue_id"`
	OccTime   time.Time `json:"occ_time"`
	Late      bool      `json:"late"`             // 接收时已迟到
	Role      string    `json:"role"`             // 在判断中的角色：anchor/delta/correction/config/incident/heartbeat/reference
	Detail    string    `json:"detail,omitempty"` // 人类可读说明
	Occupancy int       `json:"occupancy,omitempty"`
	Enter     int       `json:"enter,omitempty"`
	Exit      int       `json:"exit,omitempty"`
}

// ExcludedPoint 是一条“被排除的数据”及原因，保证每一次导流判断都可解释。
type ExcludedPoint struct {
	EventID string    `json:"event_id"`
	Type    string    `json:"type"`
	OccTime time.Time `json:"occ_time"`
	Reason  string    `json:"reason"`
}

// Evidence 汇总某次点位评估的有效数据与排除数据。
type Evidence struct {
	Used     []BasisPoint    `json:"used"`
	Excluded []ExcludedPoint `json:"excluded"`
	DataHash string          `json:"data_hash"` // 有效数据规范摘要，决策 ID 由它派生
}

// Recommendation 是引擎对单个点位的自动建议（冻结其依据）。
type Recommendation struct {
	DecisionID    string   `json:"decision_id"` // 由 DataHash+动作确定性派生
	Directive     string   `json:"directive"`   // 导流/暂停入场/恢复入场
	TargetVenueID string   `json:"target_venue_id,omitempty"`
	Reasons       []string `json:"reasons"`
	Confidence    string   `json:"confidence"` // high/medium/low，随数据质量下降
	Evidence      Evidence `json:"evidence"`
	// Acked 记录运营对该建议的处置（接受/驳回均留痕）；未处置为 nil。
	Acked *AuditEntry `json:"acked,omitempty"`
}

// AlertOccurrence 是唯一告警的一次生命周期（同一 code 解除后再发生算新一次）。
type AlertOccurrence struct {
	Key            string     `json:"key"` // session|venue|code#序号
	VenueID        string     `json:"venue_id"`
	Code           string     `json:"code"`
	Priority       string     `json:"priority"`
	Message        string     `json:"message"`
	RaisedAt       time.Time  `json:"raised_at"`
	RaisedEventID  string     `json:"raised_event_id"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	ResolveEventID string     `json:"resolve_event_id,omitempty"`
	Status         string     `json:"status"` // active/resolved
}

// AuditEntry 是运营处置的不可变审计记录。
type AuditEntry struct {
	DecisionID string    `json:"decision_id"`
	Accepted   bool      `json:"accepted"`
	OperatorID string    `json:"operator_id"`
	Reason     string    `json:"reason"`
	OccTime    time.Time `json:"occ_time"`
	EventID    string    `json:"event_id"`
	Orphaned   bool      `json:"orphaned,omitempty"` // 建议已不存在时的迟到处置
}

// ReceiptView 是通知回执台账的一行。
type ReceiptView struct {
	NotificationID string    `json:"notification_id"`
	VenueID        string    `json:"venue_id"`
	Status         string    `json:"status"`
	OperatorID     string    `json:"operator_id,omitempty"`
	Detail         string    `json:"detail,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
	EventID        string    `json:"event_id"`
}

// ContentState 是一路授权内容在 asOf 时的分发状态。
type ContentState struct {
	VenueID   string     `json:"venue_id"`
	ContentID string     `json:"content_id"`
	Status    string     `json:"status"` // distributing / expired / revoked / pending
	Start     time.Time  `json:"start"`
	End       time.Time  `json:"end"`
	StopOrder *StopOrder `json:"stop_order,omitempty"` // 过期/撤销时必须停发
}

// StopOrder 是“画面不得继续分发”的确定指令。
type StopOrder struct {
	Reason  string    `json:"reason"` // expired / revoked
	At      time.Time `json:"at"`
	EventID string    `json:"event_id"`
}

// VenueState 是单点位在 asOf 的完整口径。
type VenueState struct {
	VenueID      string     `json:"venue_id"`
	SessionID    string     `json:"session_id"`
	Level        string     `json:"level,omitempty"`
	Capacity     int        `json:"capacity"`
	Occupancy    int        `json:"occupancy"` // 截断到 [0,capacity] 的展示值
	RawOccupancy int        `json:"raw_occupancy"`
	Ratio        float64    `json:"ratio"` // -1 表示容量未知
	Heat         string     `json:"heat"`
	Online       bool       `json:"online"`
	LastReportAt *time.Time `json:"last_report_at,omitempty"`
	Quality      string     `json:"quality"`
	QualityNotes []string   `json:"quality_notes,omitempty"`

	ActiveIncident *IncidentView   `json:"active_incident,omitempty"`
	EntryHeld      bool            `json:"entry_held"`
	Recommendation *Recommendation `json:"recommendation,omitempty"`

	// CountsAccepted/CountsExcluded 是计数治理统计。
	CountsAccepted int `json:"counts_accepted"`
	CountsExcluded int `json:"counts_excluded"`
	// Anomalies 记录负占用、超员等异常，供值守核查。
	Anomalies []string `json:"anomalies,omitempty"`
}

// IncidentView 是当前生效的最高优先级事件。
type IncidentView struct {
	Code     string    `json:"code"`
	Priority string    `json:"priority"`
	Message  string    `json:"message"`
	Since    time.Time `json:"since"`
}

// SessionState 是一场赛事（可跨午夜）的全量状态。
type SessionState struct {
	SessionID string                  `json:"session_id"`
	Venues    map[string]*VenueState  `json:"venues"`
	Alerts    []*AlertOccurrence      `json:"alerts"`
	Content   []*ContentState         `json:"content"`
	Receipts  map[string]*ReceiptView `json:"receipts"`
	Audit     []*AuditEntry           `json:"audit"`
}

// Summary 是一次折叠的全部输出。
type Summary struct {
	AsOf     time.Time                `json:"as_of"`
	Sessions map[string]*SessionState `json:"sessions"`
}

// Fold 是纯函数：给定事件集合与评估时刻，产出确定状态。
// events 可为任意顺序；asOf 为零值时取最大接收时间（回放模式）。
func Fold(events []*model.Event, cfg Config, asOf time.Time) *Summary {
	// 全序排列：occ_time 为主，event_id 为终极 tiebreak，保证与接收顺序无关。
	sorted := make([]*model.Event, len(events))
	copy(sorted, events)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if !a.OccTime.Equal(b.OccTime) {
			return a.OccTime.Before(b.OccTime)
		}
		return a.EventID < b.EventID
	})
	if asOf.IsZero() && len(sorted) > 0 {
		asOf = sorted[len(sorted)-1].ReceivedAt
		if asOf.IsZero() {
			asOf = sorted[len(sorted)-1].OccTime
		}
	}

	out := &Summary{AsOf: asOf, Sessions: map[string]*SessionState{}}
	bySession := map[string][]*model.Event{}
	for _, ev := range sorted {
		bySession[ev.SessionID] = append(bySession[ev.SessionID], ev)
	}
	// 会话 ID 排序，保证输出稳定。
	sids := make([]string, 0, len(bySession))
	for sid := range bySession {
		sids = append(sids, sid)
	}
	sort.Strings(sids)
	for _, sid := range sids {
		out.Sessions[sid] = foldSession(sid, bySession[sid], cfg, asOf)
	}
	return out
}

// 派生命名辅助。
func hash8(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:8]
}
