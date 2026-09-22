// 领域模型：枚举、事件信封、载荷与对外视图。
// 枚举取值与 domain.json 保持一致；客流相关载荷只包含聚合值。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ---- 枚举（与 domain.json 一致） ----

type VenueLevel string

const (
	LevelCore   VenueLevel = "核心点位"
	LevelCoord  VenueLevel = "协同点位"
	LevelBackup VenueLevel = "备用点位"
)

type Priority string

const (
	PriNormal  Priority = "普通"
	PriCrowd   Priority = "拥挤"
	PriControl Priority = "管制"
	PriEvac    Priority = "紧急疏散"
)

func (p Priority) rank() int {
	switch p {
	case PriCrowd:
		return 1
	case PriControl:
		return 2
	case PriEvac:
		return 3
	default:
		return 0
	}
}

// AtLeastControl 管制及以上：触发暂停入场与内容分发中止。
func (p Priority) AtLeastControl() bool { return p.rank() >= PriControl.rank() }

type Receipt string

const (
	RcptReceived Receipt = "已接收"
	RcptExecuted Receipt = "已执行"
	RcptRejected Receipt = "已驳回"
	RcptExpired  Receipt = "已过期"
)

type Decision string

const (
	DecisionAccept Decision = "接受"
	DecisionReject Decision = "驳回"
)

type Heat string

const (
	HeatGreen  Heat = "绿"
	HeatYellow Heat = "黄"
	HeatRed    Heat = "红"
)

const (
	KindGuide   = "导流"
	KindSuspend = "暂停入场"
)

// ---- 事件类型 ----

const (
	EvVenueSnapshot    = "venue.snapshot"
	EvCountDelta       = "count.delta"
	EvCountAnchor      = "count.anchor"
	EvIncident         = "incident.reported"
	EvAuthzGranted     = "authz.granted"
	EvAuthzRevoked     = "authz.revoked"
	EvDirectiveReceipt = "directive.receipt"

	// 系统追加
	EvProposed    = "directive.proposed"
	EvDecision    = "directive.decision"
	EvIssued      = "directive.issued"
	EvEnforcement = "enforcement.log"
)

// Event 是追加日志中的统一信封。
// Seq 为设备单调序列号（仅计数事件需要），OccurredAt 为事件发生时间，
// ReceivedAt 为中心接收时间；两者分离以支持乱序与迟到判定。
type Event struct {
	EventID    string          `json:"eventId"`
	Type       string          `json:"type"`
	SessionID  string          `json:"sessionId"`
	VenueID    string          `json:"venueId,omitempty"`
	DeviceID   string          `json:"deviceId,omitempty"`
	Seq        int64           `json:"seq,omitempty"`
	OccurredAt time.Time       `json:"occurredAt"`
	ReceivedAt time.Time       `json:"receivedAt,omitempty"`
	Operator   string          `json:"operator,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// ---- 摄入载荷 ----

type VenueSnapshot struct {
	Name      string     `json:"name"`
	Level     VenueLevel `json:"level"`
	Capacity  int        `json:"capacity"`
	Entrances int        `json:"entrances,omitempty"`
}

// CountDelta 入离场聚合计数，绝不包含个人标识。
type CountDelta struct {
	Enter int `json:"enter"`
	Leave int `json:"leave"`
}

// CountAnchor 计数锚点：现场核验后的绝对在场真值，覆盖此前累积偏差。
type CountAnchor struct {
	Occupancy int    `json:"occupancy"`
	Reason    string `json:"reason,omitempty"`
}

type Incident struct {
	Priority Priority `json:"priority"`
	Active   bool     `json:"active"`
	Note     string   `json:"note,omitempty"`
	Source   string   `json:"source,omitempty"`
}

type AuthzGrant struct {
	ContentID string    `json:"contentId"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
}

type AuthzRevoke struct {
	ContentID string `json:"contentId"`
	Reason    string `json:"reason,omitempty"`
}

type ReceiptPayload struct {
	DirectiveID string  `json:"directiveId"`
	Receipt     Receipt `json:"receipt"`
}

// ---- 系统载荷（服务端追加，审计依据） ----

// EvidenceItem 证据项：某次判定实际使用（或显式排除）的一条数据。
type EvidenceItem struct {
	EventID    string `json:"eventId"`
	Type       string `json:"type"`
	OccurredAt string `json:"occurredAt"`
	Detail     string `json:"detail"`
	Used       bool   `json:"used"`
}

// DirectiveProposal 是建议的完整快照，生成时即固化进 directive.proposed 事件。
type DirectiveProposal struct {
	DirectiveID string         `json:"directiveId"`
	Kind        string         `json:"kind"`
	Priority    string         `json:"priority"`
	VenueID     string         `json:"venueId"`
	VenueName   string         `json:"venueName"`
	Targets     []TargetChoice `json:"targets,omitempty"`
	Reason      string         `json:"reason"`
	Evidence    []EvidenceItem `json:"evidence"`
	ProposedAt  time.Time      `json:"proposedAt"`
	TTLSeconds  int            `json:"ttlSeconds"`
}

type TargetChoice struct {
	VenueID   string  `json:"venueId"`
	Name      string  `json:"name"`
	Level     string  `json:"level"`
	Ratio     float64 `json:"ratio"`
	FreeSeats int     `json:"freeSeats"`
}

type DecisionPayload struct {
	DirectiveID string   `json:"directiveId"`
	Decision    Decision `json:"decision"`
	Operator    string   `json:"operator"`
	Reason      string   `json:"reason,omitempty"`
}

type EnforcementPayload struct {
	VenueID   string `json:"venueId"`
	ContentID string `json:"contentId,omitempty"`
	Reason    string `json:"reason"`
}

// ---- 对外视图 ----

type ExcludedItem struct {
	EventID    string `json:"eventId"`
	Type       string `json:"type"`
	VenueID    string `json:"venueId,omitempty"`
	DeviceID   string `json:"deviceId,omitempty"`
	Seq        int64  `json:"seq,omitempty"`
	OccurredAt string `json:"occurredAt,omitempty"`
	Reason     string `json:"reason"`
}

type VenueView struct {
	VenueID        string  `json:"venueId"`
	Name           string  `json:"name"`
	Level          string  `json:"level"`
	Capacity       int     `json:"capacity"`
	Occupancy      int     `json:"occupancy"`
	Ratio          float64 `json:"ratio"`
	Heat           string  `json:"heat"`
	Held           bool    `json:"held"`
	Incident       string  `json:"incident,omitempty"`
	LastCountAt    string  `json:"lastCountAt,omitempty"`
	DataFreshnessS int64   `json:"dataFreshnessS"`
	Stale          bool    `json:"stale"`
	Registered     bool    `json:"registered"`
}

type AlarmView struct {
	AlarmID        string `json:"alarmId"`
	VenueID        string `json:"venueId"`
	VenueName      string `json:"venueName"`
	Priority       string `json:"priority"`
	StartedAt      string `json:"startedAt"`
	TriggerEventID string `json:"triggerEventId"`
	Active         bool   `json:"active"`
	EndedAt        string `json:"endedAt,omitempty"`
	Reason         string `json:"reason"`
	// AttributedTo 非空时，表示该拥堵被归因为某条已生效导流指令。
	AttributedTo    string  `json:"attributedTo,omitempty"`
	AttributionNote string  `json:"attributionNote,omitempty"`
	PostEnterRate   float64 `json:"postEnterRate,omitempty"`
	PreEnterRate    float64 `json:"preEnterRate,omitempty"`
}

type DirectiveView struct {
	DirectiveID string            `json:"directiveId"`
	Kind        string            `json:"kind"`
	Priority    string            `json:"priority"`
	VenueID     string            `json:"venueId"`
	VenueName   string            `json:"venueName"`
	Targets     []TargetChoice    `json:"targets,omitempty"`
	Reason      string            `json:"reason"`
	Status      string            `json:"status"`
	ProposedAt  string            `json:"proposedAt"`
	DecidedAt   string            `json:"decidedAt,omitempty"`
	Operator    string            `json:"operator,omitempty"`
	DecisionRsn string            `json:"decisionReason,omitempty"`
	TTLSeconds  int               `json:"ttlSeconds"`
	Receipts    map[string]string `json:"receipts,omitempty"`
	Evidence    []EvidenceItem    `json:"evidence"`
}

type DeviceGap struct {
	VenueID      string  `json:"venueId"`
	DeviceID     string  `json:"deviceId"`
	MaxSeq       int64   `json:"maxSeq"`
	NextExpected int64   `json:"nextExpected"`
	Missing      []int64 `json:"missing,omitempty"`
}

type Attribution struct {
	DirectiveID string  `json:"directiveId"`
	SourceVenue string  `json:"sourceVenue"`
	TargetVenue string  `json:"targetVenue"`
	AlarmID     string  `json:"alarmId"`
	PreRate     float64 `json:"preEnterRatePerMin"`
	PostRate    float64 `json:"postEnterRatePerMin"`
	Note        string  `json:"note"`
}

type SummaryView struct {
	SessionID    string          `json:"sessionId"`
	AsOf         string          `json:"asOf"`
	Venues       []VenueView     `json:"venues"`
	Alarms       []AlarmView     `json:"alarms"`
	Directives   []DirectiveView `json:"directives"`
	Attributions []Attribution   `json:"attributions"`
	Gaps         []DeviceGap     `json:"gaps"`
	Excluded     []ExcludedItem  `json:"excluded"`
}

// ---- 校验 ----

var validLevels = map[VenueLevel]bool{LevelCore: true, LevelCoord: true, LevelBackup: true}
var validPriorities = map[Priority]bool{PriNormal: true, PriCrowd: true, PriControl: true, PriEvac: true}
var validReceipts = map[Receipt]bool{RcptReceived: true, RcptExecuted: true, RcptRejected: true, RcptExpired: true}
var validDecisions = map[Decision]bool{DecisionAccept: true, DecisionReject: true}

var knownTypes = map[string]bool{
	EvVenueSnapshot: true, EvCountDelta: true, EvCountAnchor: true, EvIncident: true,
	EvAuthzGranted: true, EvAuthzRevoked: true, EvDirectiveReceipt: true,
	EvProposed: true, EvDecision: true, EvIssued: true, EvEnforcement: true,
}

// forbiddenPersonalKeys 递归拒绝的个人标识字段（点位名称 name 不在其列）。
var forbiddenPersonalKeys = map[string]bool{
	"姓名": true, "name_cn": true, "realname": true, "fullname": true,
	"证件": true, "证件号": true, "身份证": true, "身份证号": true, "idcard": true, "idcardno": true, "passport": true,
	"手机": true, "手机号": true, "电话": true, "phone": true, "mobile": true, "tel": true,
	"人脸": true, "人脸数据": true, "face": true, "facedata": true,
	"指纹": true, "fingerprint": true,
	"设备指纹": true, "devicefingerprint": true, "fingerprintjs": true,
	"观众id": true, "用户id": true, "userid": true, "passengerid": true, "customerid": true,
	"mac": true, "imsi": true, "imei": true,
}

// noPersonalIdentifiers 递归检查任意 JSON 结构，发现个人标识字段即拒绝。
func noPersonalIdentifiers(v any, path string) error {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			lk := strings.ToLower(strings.TrimSpace(k))
			p := path + "." + k
			if forbiddenPersonalKeys[lk] {
				return fmt.Errorf("疑似个人标识字段，拒绝摄入: %s", p)
			}
			if err := noPersonalIdentifiers(vv, p); err != nil {
				return err
			}
		}
	case []any:
		for i, vv := range x {
			if err := noPersonalIdentifiers(vv, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// strictUnmarshal 使用拒绝未知字段的方式解析载荷。
func strictUnmarshal(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return errors.New("缺少 payload")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("payload 解析失败: %w", err)
	}
	return nil
}

var systemGenerated = map[string]bool{
	EvProposed: true, EvDecision: true, EvIssued: true, EvEnforcement: true,
}

// Validate 校验一条外部摄入事件（系统事件由服务端自行构造，不再过此校验）。
func (e Event) Validate(now time.Time) error {
	if !knownTypes[e.Type] {
		return fmt.Errorf("未知事件类型: %q", e.Type)
	}
	if systemGenerated[e.Type] {
		return fmt.Errorf("事件类型 %s 仅可由系统生成", e.Type)
	}
	if e.SessionID == "" {
		return errors.New("sessionId 不能为空（跨午夜场次以显式场次ID归并）")
	}
	if e.VenueID == "" {
		return errors.New("venueId 不能为空")
	}
	if e.OccurredAt.IsZero() {
		return errors.New("occurredAt 不能为空")
	}
	if e.OccurredAt.After(now.Add(time.Hour)) {
		return errors.New("occurredAt 超出允许的时钟漂移范围")
	}

	var raw any
	if len(e.Payload) > 0 {
		if err := json.Unmarshal(e.Payload, &raw); err != nil {
			return fmt.Errorf("payload 不是合法 JSON: %w", err)
		}
		if err := noPersonalIdentifiers(raw, "$"); err != nil {
			return err
		}
	}

	switch e.Type {
	case EvVenueSnapshot:
		var p VenueSnapshot
		if err := strictUnmarshal(e.Payload, &p); err != nil {
			return err
		}
		if !validLevels[p.Level] {
			return fmt.Errorf("非法点位等级: %q", p.Level)
		}
		if p.Capacity <= 0 {
			return errors.New("容量必须为正数")
		}
	case EvCountDelta:
		var p CountDelta
		if err := strictUnmarshal(e.Payload, &p); err != nil {
			return err
		}
		if e.DeviceID == "" {
			return errors.New("count.delta 必须携带 deviceId")
		}
		if e.Seq <= 0 {
			return errors.New("count.delta 必须携带设备单调序列号 seq>0（用于去重与缺口发现）")
		}
		if p.Enter < 0 || p.Leave < 0 {
			return errors.New("入离场计数不能为负")
		}
	case EvCountAnchor:
		var p CountAnchor
		if err := strictUnmarshal(e.Payload, &p); err != nil {
			return err
		}
		if e.DeviceID == "" {
			return errors.New("count.anchor 必须携带 deviceId")
		}
		if e.Seq <= 0 {
			return errors.New("count.anchor 必须携带 seq")
		}
		if p.Occupancy < 0 {
			return errors.New("锚点在场数不能为负")
		}
	case EvIncident:
		var p Incident
		if err := strictUnmarshal(e.Payload, &p); err != nil {
			return err
		}
		if !validPriorities[p.Priority] {
			return fmt.Errorf("非法事件优先级: %q", p.Priority)
		}
	case EvAuthzGranted:
		var p AuthzGrant
		if err := strictUnmarshal(e.Payload, &p); err != nil {
			return err
		}
		if p.ContentID == "" {
			return errors.New("contentId 不能为空")
		}
		if !p.End.After(p.Start) {
			return errors.New("授权结束时间必须晚于开始时间")
		}
	case EvAuthzRevoked:
		var p AuthzRevoke
		if err := strictUnmarshal(e.Payload, &p); err != nil {
			return err
		}
		if p.ContentID == "" {
			return errors.New("contentId 不能为空")
		}
	case EvDirectiveReceipt:
		var p ReceiptPayload
		if err := strictUnmarshal(e.Payload, &p); err != nil {
			return err
		}
		if p.DirectiveID == "" || !validReceipts[p.Receipt] {
			return errors.New("回执必须包含 directiveId 与合法回执状态")
		}
	}
	return nil
}
