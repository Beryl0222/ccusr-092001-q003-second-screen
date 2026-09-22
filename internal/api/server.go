// Package api 暴露第二现场联动服务的 HTTP 接口。
//
//	POST /v1/events                上报事件（容量快照/计数/事件/授权/回执/处置/心跳/清理）
//	GET  /v1/summary               分区热度、占用、建议与告警总览（?session=&as_of=）
//	GET  /v1/venues/{id}           单点位明细，含建议的完整数据依据
//	GET  /v1/decisions/{id}        解释一次导流/暂停判断使用了哪些有效数据、排除了什么
//	POST /v1/decisions/{id}/ack    运营接受/驳回（写入不可变审计）
//	GET  /v1/alerts                唯一告警台账（?session=）
//	GET  /v1/content               内容分发状态与停发指令（?session=）
//	GET  /v1/receipts              通知回执台账（?session=）
//	GET  /v1/audit                 运营处置审计（?session=）
//	GET  /v1/replay/stream?since=  失联恢复：增量事件 + 当前水位线
//	GET  /health                   值守巡检
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/beryl0222/city-second-screen/internal/engine"
	"github.com/beryl0222/city-second-screen/internal/service"
	"github.com/beryl0222/city-second-screen/model"
)

type Server struct {
	svc *service.Service
	mux *http.ServeMux
}

func NewServer(svc *service.Service) *Server {
	s := &Server{svc: svc, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) routes() {
	s.mux.HandleFunc("/health", s.health)
	s.mux.HandleFunc("/v1/events", s.postEvent)
	s.mux.HandleFunc("/v1/summary", s.summary)
	s.mux.HandleFunc("/v1/venues/", s.venue)
	s.mux.HandleFunc("/v1/decisions/", s.decisions)
	s.mux.HandleFunc("/v1/alerts", s.alerts)
	s.mux.HandleFunc("/v1/content", s.content)
	s.mux.HandleFunc("/v1/receipts", s.receipts)
	s.mux.HandleFunc("/v1/audit", s.audit)
	s.mux.HandleFunc("/v1/replay/stream", s.replayStream)
}

const serviceID = "city-second-screen"

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": serviceID})
}

func (s *Server) postEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	body := http.MaxBytesReader(w, r.Body, 64*1024*1024)
	buf, err := io.ReadAll(body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "事件体积超过 64MiB")
			return
		}
		writeError(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
		return
	}
	ev, res, err := s.svc.Ingest(buf)
	if err != nil {
		var ve *model.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, ve.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"event_id":  ev.EventID,
		"version":   res.Version,
		"duplicate": res.Duplicate,
		"accepted":  true,
	})
}

// summaryResp 是面向运营大屏的精简视图。
type summaryResp struct {
	AsOf       time.Time                 `json:"as_of"`
	Version    int64                     `json:"version"`
	SessionID  string                    `json:"session_id,omitempty"`
	Venues     []venueBrief              `json:"venues"`
	Alerts     []*engine.AlertOccurrence `json:"alerts"`
	StopOrders []*engine.ContentState    `json:"content_stop_orders,omitempty"`
}

type venueBrief struct {
	VenueID      string  `json:"venue_id"`
	Level        string  `json:"level,omitempty"`
	Heat         string  `json:"heat"`
	Occupancy    int     `json:"occupancy"`
	Capacity     int     `json:"capacity"`
	Ratio        float64 `json:"ratio"`
	Online       bool    `json:"online"`
	EntryHeld    bool    `json:"entry_held"`
	Quality      string  `json:"quality"`
	Directive    string  `json:"directive,omitempty"`
	TargetVenue  string  `json:"target_venue,omitempty"`
	DecisionID   string  `json:"decision_id,omitempty"`
	IncidentCode string  `json:"incident_code,omitempty"`
}

func (s *Server) summary(w http.ResponseWriter, r *http.Request) {
	sess := r.URL.Query().Get("session")
	asOf := parseAsOf(r)
	sum, err := s.svc.Snapshot(asOf)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := summaryResp{AsOf: sum.AsOf, Version: s.svc.Version(), SessionID: sess}
	sids := sortedSessions(sum, sess)
	for _, sid := range sids {
		st := sum.Sessions[sid]
		ids := make([]string, 0, len(st.Venues))
		for id := range st.Venues {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			v := st.Venues[id]
			b := venueBrief{
				VenueID: id, Level: v.Level, Heat: v.Heat, Occupancy: v.Occupancy,
				Capacity: v.Capacity, Ratio: v.Ratio, Online: v.Online,
				EntryHeld: v.EntryHeld, Quality: v.Quality,
			}
			if v.ActiveIncident != nil {
				b.IncidentCode = v.ActiveIncident.Code
			}
			if v.Recommendation != nil {
				b.Directive = v.Recommendation.Directive
				b.TargetVenue = v.Recommendation.TargetVenueID
				b.DecisionID = v.Recommendation.DecisionID
			}
			resp.Venues = append(resp.Venues, b)
		}
		resp.Alerts = append(resp.Alerts, st.Alerts...)
		for _, c := range st.Content {
			if c.StopOrder != nil {
				resp.StopOrders = append(resp.StopOrders, c)
			}
		}
	}
	sort.SliceStable(resp.Alerts, func(i, j int) bool {
		if resp.Alerts[i].VenueID != resp.Alerts[j].VenueID {
			return resp.Alerts[i].VenueID < resp.Alerts[j].VenueID
		}
		return resp.Alerts[i].RaisedAt.Before(resp.Alerts[j].RaisedAt)
	})
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) venue(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/venues/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "点位不存在")
		return
	}
	sess := r.URL.Query().Get("session")
	sum, err := s.svc.Snapshot(parseAsOf(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, sid := range sortedSessions(sum, sess) {
		if v := sum.Sessions[sid].Venues[id]; v != nil {
			writeJSON(w, http.StatusOK, v)
			return
		}
	}
	writeError(w, http.StatusNotFound, "点位不存在或尚无数据")
}

// decisions 处理建议解释与运营处置：
//
//	GET  /v1/decisions/{id}        -> 建议 + 完整数据依据
//	POST /v1/decisions/{id}/ack    -> 接受/驳回（审计）
func (s *Server) decisions(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/decisions/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		writeError(w, http.StatusNotFound, "decision_id 缺失")
		return
	}
	if len(parts) == 2 && parts[1] == "ack" {
		s.ackDecision(w, r, id)
		return
	}
	if len(parts) != 1 || r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 GET 与 .../ack POST")
		return
	}
	sess := r.URL.Query().Get("session")
	sum, err := s.svc.Snapshot(parseAsOf(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, sid := range sortedSessions(sum, sess) {
		for _, v := range sum.Sessions[sid].Venues {
			if v.Recommendation != nil && v.Recommendation.DecisionID == id {
				writeJSON(w, http.StatusOK, map[string]any{
					"session_id":     sid,
					"venue_id":       v.VenueID,
					"recommendation": v.Recommendation,
					"venue_quality":  v.Quality,
					"quality_notes":  v.QualityNotes,
				})
				return
			}
		}
	}
	writeError(w, http.StatusNotFound, "建议不存在（可能其数据依据已变化，请以最新决策为准）")
}

func (s *Server) ackDecision(w http.ResponseWriter, r *http.Request, decisionID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var in struct {
		Accepted   *bool      `json:"accepted"`
		OperatorID string     `json:"operator_id"`
		Reason     string     `json:"reason"`
		SessionID  string     `json:"session_id"`
		VenueID    string     `json:"venue_id"`
		DeviceID   string     `json:"device_id"`
		OccTime    *time.Time `json:"occ_time"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "请求不是合法 JSON: "+err.Error())
		return
	}
	if in.Accepted == nil {
		writeError(w, http.StatusBadRequest, "accepted 必填（true=接受 false=驳回）")
		return
	}
	if in.OperatorID == "" {
		writeError(w, http.StatusBadRequest, "operator_id 必填（审计责任人）")
		return
	}
	if in.SessionID == "" || in.VenueID == "" {
		writeError(w, http.StatusBadRequest, "session_id 与 venue_id 必填")
		return
	}
	occ := time.Now()
	if in.OccTime != nil {
		occ = *in.OccTime
	}
	// 处置本身也是事件：复用同一条只追加日志，天然不可篡改、可重放。
	payload, _ := json.Marshal(map[string]any{
		"decision_id": decisionID,
		"accepted":    *in.Accepted,
		"operator_id": in.OperatorID,
		"reason":      in.Reason,
	})
	env, _ := json.Marshal(map[string]any{
		"event_id":   "ack-" + decisionID + "-" + in.OperatorID + "-" + occ.Format("20060102T150405.000000000"),
		"type":       model.TypeDecisionAck,
		"venue_id":   in.VenueID,
		"device_id":  in.DeviceID,
		"session_id": in.SessionID,
		"occ_time":   occ,
		"content":    json.RawMessage(payload),
	})
	if _, _, err := s.svc.Ingest(env); err != nil {
		var ve *model.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, ve.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"decision_id": decisionID,
		"accepted":    *in.Accepted,
		"audited":     true,
	})
}

func (s *Server) alerts(w http.ResponseWriter, r *http.Request) {
	sess := r.URL.Query().Get("session")
	sum, err := s.svc.Snapshot(parseAsOf(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var out []*engine.AlertOccurrence
	for _, sid := range sortedSessions(sum, sess) {
		out = append(out, sum.Sessions[sid].Alerts...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": out})
}

func (s *Server) content(w http.ResponseWriter, r *http.Request) {
	sess := r.URL.Query().Get("session")
	sum, err := s.svc.Snapshot(parseAsOf(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type row struct {
		SessionID string `json:"session_id"`
		*engine.ContentState
	}
	var out []row
	for _, sid := range sortedSessions(sum, sess) {
		for _, c := range sum.Sessions[sid].Content {
			out = append(out, row{SessionID: sid, ContentState: c})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": out, "as_of": sum.AsOf})
}

func (s *Server) receipts(w http.ResponseWriter, r *http.Request) {
	sess := r.URL.Query().Get("session")
	sum, err := s.svc.Snapshot(parseAsOf(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type row struct {
		SessionID string `json:"session_id"`
		*engine.ReceiptView
	}
	var out []row
	for _, sid := range sortedSessions(sum, sess) {
		ids := make([]string, 0, len(sum.Sessions[sid].Receipts))
		for nid := range sum.Sessions[sid].Receipts {
			ids = append(ids, nid)
		}
		sort.Strings(ids)
		for _, nid := range ids {
			out = append(out, row{SessionID: sid, ReceiptView: sum.Sessions[sid].Receipts[nid]})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"receipts": out})
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	sess := r.URL.Query().Get("session")
	sum, err := s.svc.Snapshot(parseAsOf(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type row struct {
		SessionID string `json:"session_id"`
		*engine.AuditEntry
	}
	var out []row
	for _, sid := range sortedSessions(sum, sess) {
		for _, a := range sum.Sessions[sid].Audit {
			out = append(out, row{SessionID: sid, AuditEntry: a})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": out})
}

// replayStream 支持中心失联恢复：客户端携带本地水位 since，服务端返回之后
// 的全部事件与最新水位；客户端对同一事件集做 Fold 即可收敛到一致状态。
func (s *Server) replayStream(w http.ResponseWriter, r *http.Request) {
	since := int64(0)
	if v := r.URL.Query().Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since 必须是整数版本号")
			return
		}
		since = n
	}
	evs, latest, err := s.svc.EventsSince(since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"since_version":  since,
		"latest_version": latest,
		"caught_up":      latest == since,
		"events":         evs,
	})
}

// ---------------- helpers ----------------

func sortedSessions(sum *engine.Summary, only string) []string {
	var ids []string
	for sid := range sum.Sessions {
		if only == "" || sid == only {
			ids = append(ids, sid)
		}
	}
	sort.Strings(ids)
	return ids
}

func parseAsOf(r *http.Request) time.Time {
	if v := r.URL.Query().Get("as_of"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg, "code": code})
}
