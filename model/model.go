// Package model 定义第二现场客流联动服务的领域枚举、事件模型与校验规则。
//
// 隐私边界（对应 domain.json 的“客流计数只交换聚合值，不传递个人标识”）：
// 入离场计数、快照占用均为聚合值；事件载荷中出现姓名、手机号、证件号等
// 个人标识字段或可识别个人的设备硬件号一律拒绝，从入口阻断 PII 进入服务。
// 允许出现的标识只有：点位 venue_id、场次 session_id、采集设备 device_id
// （基础设施资产编号）与值班岗位 operator_id，均不指向单个观众。
package model

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// 与 domain.json 对齐的枚举值。
const (
	LevelCore   = "核心点位"
	LevelCoop   = "协同点位"
	LevelBackup = "备用点位"

	PriorityNormal  = "普通"
	PriorityCrowd   = "拥挤"
	PriorityControl = "管制"
	PriorityEvac    = "紧急疏散"

	ReceiptAccepted = "已接收"
	ReceiptExecuted = "已执行"
	ReceiptRejected = "已驳回"
	ReceiptExpired  = "已过期"

	// 中心下发给点位的动作（建议/指令）。
	DirectiveDivert  = "导流"
	DirectiveHold    = "暂停入场"
	DirectiveRelease = "恢复入场"
)

// 数据质量标记，汇总接口据此披露建议所依赖数据的完整性。
const (
	QualityFresh        = "fresh"         // 关键数据均在新鲜窗口内
	QualityLateArrivals = "late_arrivals" // 评估窗口内合并过迟到后补数据
	QualityGap          = "stale_gap"     // 存在超出容忍窗口的报数缺口
	QualityRecovered    = "recovered"     // 点位/中心刚从失联恢复
	QualityStale        = "stale"         // 持续无任何有效报数，占用不可采信
)

var (
	ValidLevel = map[string]bool{
		LevelCore: true, LevelCoop: true, LevelBackup: true,
	}
	ValidPriority = map[string]bool{
		PriorityNormal: true, PriorityCrowd: true, PriorityControl: true, PriorityEvac: true,
	}
	ValidReceipt = map[string]bool{
		ReceiptAccepted: true, ReceiptExecuted: true, ReceiptRejected: true, ReceiptExpired: true,
	}
)

// 事件类型。
const (
	TypeVenueRegister = "venue_register"        // 点位注册（等级、容量）
	TypeCount         = "count_report"          // 入/离场聚合计数
	TypeSnapshot      = "capacity_snapshot"     // 绝对占用快照（权威锚点）
	TypeIncident      = "incident"              // 突发拥挤/管制/疏散及其解除
	TypeAuthorize     = "content_authorization" // 内容授权时段
	TypeReceipt       = "receipt"               // 通知回执
	TypeDecisionAck   = "decision_ack"          // 运营对自动建议的接受/驳回
	TypeHeartbeat     = "heartbeat"             // 设备/链路心跳（失联与恢复）
	TypeClear         = "clear_session"         // 场次清理（跨午夜隔离）
)

var validType = map[string]bool{
	TypeVenueRegister: true, TypeCount: true, TypeSnapshot: true, TypeIncident: true,
	TypeAuthorize: true, TypeReceipt: true, TypeDecisionAck: true,
	TypeHeartbeat: true, TypeClear: true,
}

// Event 是校验后的统一事件表示。
type Event struct {
	EventID    string    `json:"event_id"`
	Type       string    `json:"type"`
	VenueID    string    `json:"venue_id"`
	DeviceID   string    `json:"device_id"` // 采集设备资产编号，非观众标识
	SessionID  string    `json:"session_id"`
	Seq        int64     `json:"seq"`
	OccTime    time.Time `json:"occ_time"`    // 采集端声称的发生时间（可能迟到、可能乱序）
	ReceivedAt time.Time `json:"received_at"` // 服务端接收入库时间（权威时间线）
	Version    int64     `json:"version"`     // 服务端分配的全局追加版本号

	Register    *RegisterPayload    `json:"register,omitempty"`
	Count       *CountPayload       `json:"count,omitempty"`
	Snapshot    *SnapshotPayload    `json:"snapshot,omitempty"`
	Incident    *IncidentPayload    `json:"incident,omitempty"`
	Authorize   *AuthorizePayload   `json:"authorize,omitempty"`
	Receipt     *ReceiptPayload     `json:"receipt,omitempty"`
	DecisionAck *DecisionAckPayload `json:"decision_ack,omitempty"`
	Heartbeat   *HeartbeatPayload   `json:"heartbeat,omitempty"`
	Clear       *ClearPayload       `json:"clear,omitempty"`
}

type RegisterPayload struct {
	Level           string   `json:"level"`
	Capacity        int      `json:"capacity"`
	AlternateVenues []string `json:"alternate_venues"`
}

// CountPayload 为入/离场聚合增量（非累计值）。
type CountPayload struct {
	Enter int `json:"enter"`
	Exit  int `json:"exit"`
	// 多闸机/多相机的分项聚合；折叠时与 Enter/Exit 一并求和。
	Gates map[string]GateCount `json:"gates"`
	// 非空表示本条是对先到计数事件的修正（以新值替换旧批次）。
	CorrectsEventID string `json:"corrects_event_id"`
	// Confidence 为 0~100；缺省按 100 处理。
	Confidence int `json:"confidence"`
}

type GateCount struct {
	Enter int `json:"enter"`
	Exit  int `json:"exit"`
}

type SnapshotPayload struct {
	Occupancy     int  `json:"occupancy"`
	Capacity      int  `json:"capacity"`      // >0 时更新点位容量
	Authoritative bool `json:"authoritative"` // 权威快照会锚定占用；非权威仅参考
}

type IncidentPayload struct {
	Priority     string `json:"priority"`
	Code         string `json:"code"`
	Message      string `json:"message"`
	Resolved     bool   `json:"resolved"`
	ResolvesCode string `json:"resolves_code"` // 解除事件引用被解除事件的 code
}

type AuthorizePayload struct {
	ContentID string    `json:"content_id"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Revoked   bool      `json:"revoked"` // 提前撤销
}

type ReceiptPayload struct {
	NotificationID string `json:"notification_id"`
	Status         string `json:"status"`
	OperatorID     string `json:"operator_id"` // 值班岗位标识，允许
	Detail         string `json:"detail"`
}

type DecisionAckPayload struct {
	DecisionID string `json:"decision_id"`
	Accepted   *bool  `json:"accepted"`
	OperatorID string `json:"operator_id"`
	Reason     string `json:"reason"`
}

type HeartbeatPayload struct {
	Online     bool  `json:"online"`
	GapSeconds int64 `json:"gap_seconds"` // 重连后首包携带的离线时长，>0 表示曾失联
}

type ClearPayload struct {
	KeepConfig bool `json:"keep_config"`
}

// ---------------- 校验 ----------------

// ValidationError 表示事件因内容不合法被拒绝，HTTP 层映射为 400。
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func ve(format string, args ...any) error { return &ValidationError{Msg: fmt.Sprintf(format, args...)} }

type rawEnvelope struct {
	EventID   string          `json:"event_id"`
	Type      string          `json:"type"`
	VenueID   string          `json:"venue_id"`
	DeviceID  string          `json:"device_id"`
	SessionID string          `json:"session_id"`
	Seq       int64           `json:"seq"`
	OccTime   time.Time       `json:"occ_time"`
	Payload   json.RawMessage `json:"content"`
}

// 禁止出现的字段名（小写匹配）。设备资产号与值班岗位号不在其列。
var forbiddenKeys = map[string]bool{
	"name": true, "姓名": true, "realname": true, "real_name": true,
	"phone": true, "mobile": true, "tel": true, "手机": true, "手机号": true, "电话": true,
	"idcard": true, "id_card": true, "idnumber": true, "id_number": true,
	"证件号": true, "身份证": true, "passport": true,
	"face_id": true, "faceid": true, "face_data": true, "face_token": true, "人脸": true,
	"mac": true, "mac_address": true, "imei": true, "imsi": true, "idfa": true,
	"user_id": true, "userid": true, "user_name": true, "username": true,
	"customer_id": true, "customerid": true,
	"visitor_id": true, "visitorid": true, "visitor_name": true,
	"audience_id": true, "person_id": true,
}

var (
	rePhone  = regexp.MustCompile(`^1[3-9]\d{9}$`)
	reIDCard = regexp.MustCompile(`^\d{17}[\dXx]$`)
)

// scanPII 递归检查任意 JSON 节点，发现疑似个人标识的键或字符串值即拒绝。
func scanPII(node any, path string) error {
	switch v := node.(type) {
	case map[string]any:
		for k, sub := range v {
			lk := strings.ToLower(strings.TrimSpace(k))
			if forbiddenKeys[lk] {
				return ve("载荷包含禁止的个人标识字段: %s（仅允许聚合值）", joinPath(path, k))
			}
			if s, ok := sub.(string); ok && looksLikePIIValue(s) {
				return ve("字段 %s 的值疑似个人标识（手机号/证件号），已拒绝", joinPath(path, k))
			}
			if err := scanPII(sub, joinPath(path, k)); err != nil {
				return err
			}
		}
	case []any:
		for i, sub := range v {
			if err := scanPII(sub, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func joinPath(a, b string) string {
	if a == "" {
		return b
	}
	return a + "." + b
}

func looksLikePIIValue(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 11 {
		return false
	}
	return rePhone.MatchString(s) || reIDCard.MatchString(s)
}

// ParseEnvelope 解析并校验一条上报事件。now 作为接收时间由存储层传入，
// 保证时间线不依赖不可信的客户端时钟。
func ParseEnvelope(b []byte, now time.Time) (*Event, error) {
	var raw rawEnvelope
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		// 含未知字段时退回宽松解析，但仍做 PII 与结构校验。
		if err2 := json.Unmarshal(b, &raw); err2 != nil {
			return nil, ve("事件不是合法 JSON: %v", err2)
		}
	}
	if raw.EventID == "" {
		return nil, ve("event_id 必填（用于幂等去重）")
	}
	if !validType[raw.Type] {
		return nil, ve("未知事件类型: %q", raw.Type)
	}
	if raw.VenueID == "" {
		return nil, ve("venue_id 必填")
	}
	if raw.SessionID == "" {
		return nil, ve("session_id 必填（跨午夜场次隔离）")
	}
	if raw.OccTime.IsZero() {
		return nil, ve("occ_time 必填（发生时间，用于乱序/迟到判定）")
	}
	if raw.OccTime.After(now.Add(24 * time.Hour)) {
		return nil, ve("occ_time 超出允许的时钟偏差范围")
	}

	// PII 扫描覆盖整个信封（含载荷原文）。
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return nil, ve("事件不是合法 JSON: %v", err)
	}
	if err := scanPII(generic, ""); err != nil {
		return nil, err
	}

	ev := &Event{
		EventID: raw.EventID, Type: raw.Type, VenueID: raw.VenueID,
		DeviceID: raw.DeviceID, SessionID: raw.SessionID, Seq: raw.Seq,
		OccTime: raw.OccTime, ReceivedAt: now,
	}
	if len(raw.Payload) > 0 {
		if err := parsePayload(ev, raw.Payload); err != nil {
			return nil, err
		}
	}
	if err := validateEvent(ev); err != nil {
		return nil, err
	}
	return ev, nil
}

func parsePayload(ev *Event, raw json.RawMessage) error {
	var err error
	switch ev.Type {
	case TypeVenueRegister:
		p := &RegisterPayload{}
		err = json.Unmarshal(raw, p)
		ev.Register = p
	case TypeCount:
		p := &CountPayload{}
		err = json.Unmarshal(raw, p)
		ev.Count = p
	case TypeSnapshot:
		p := &SnapshotPayload{}
		err = json.Unmarshal(raw, p)
		ev.Snapshot = p
	case TypeIncident:
		p := &IncidentPayload{}
		err = json.Unmarshal(raw, p)
		ev.Incident = p
	case TypeAuthorize:
		p := &AuthorizePayload{}
		err = json.Unmarshal(raw, p)
		ev.Authorize = p
	case TypeReceipt:
		p := &ReceiptPayload{}
		err = json.Unmarshal(raw, p)
		ev.Receipt = p
	case TypeDecisionAck:
		p := &DecisionAckPayload{}
		err = json.Unmarshal(raw, p)
		ev.DecisionAck = p
	case TypeHeartbeat:
		p := &HeartbeatPayload{}
		err = json.Unmarshal(raw, p)
		ev.Heartbeat = p
	case TypeClear:
		p := &ClearPayload{}
		err = json.Unmarshal(raw, p)
		ev.Clear = p
	}
	return err
}

func validateEvent(ev *Event) error {
	switch ev.Type {
	case TypeVenueRegister:
		p := ev.Register
		if !ValidLevel[p.Level] {
			return ve("点位等级非法: %q", p.Level)
		}
		if p.Capacity < 0 {
			return ve("容量不能为负")
		}
	case TypeCount:
		p := ev.Count
		if p.Confidence < 0 || p.Confidence > 100 {
			return ve("confidence 须在 0~100 之间")
		}
		if p.Confidence == 0 {
			p.Confidence = 100 // 缺省
		}
	case TypeSnapshot:
		p := ev.Snapshot
		if p.Occupancy < 0 {
			return ve("占用不能为负")
		}
		if p.Capacity < 0 {
			return ve("容量不能为负")
		}
	case TypeIncident:
		p := ev.Incident
		if !ValidPriority[p.Priority] {
			return ve("事件优先级非法: %q", p.Priority)
		}
		if p.Code == "" {
			return ve("incident.code 必填（用于唯一告警与解除引用）")
		}
	case TypeAuthorize:
		p := ev.Authorize
		if p.ContentID == "" {
			return ve("content_id 必填")
		}
		if !p.Revoked && !p.End.After(p.Start) {
			return ve("授权结束时间必须晚于开始时间")
		}
	case TypeReceipt:
		p := ev.Receipt
		if p.NotificationID == "" {
			return ve("notification_id 必填")
		}
		if !ValidReceipt[p.Status] {
			return ve("回执状态非法: %q", p.Status)
		}
	case TypeDecisionAck:
		p := ev.DecisionAck
		if p.DecisionID == "" {
			return ve("decision_id 必填")
		}
		if p.Accepted == nil {
			return ve("accepted 必填（运营接受或驳回）")
		}
		if p.OperatorID == "" {
			return ve("operator_id 必填（审计责任人）")
		}
	case TypeHeartbeat, TypeClear:
		// 无额外约束
	}
	return nil
}
