package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/beryl0222/city-second-screen/internal/engine"
	"github.com/beryl0222/city-second-screen/internal/service"
	"github.com/beryl0222/city-second-screen/model"
)

func newTestService(t *testing.T) *service.Service {
	t.Helper()
	svc, err := service.New("", engine.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	return svc
}

func postEvent(t *testing.T, srv *Server, raw string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(raw))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func envelope(id, typ, venue, sid, occ string, content any) string {
	c, _ := json.Marshal(content)
	b, _ := json.Marshal(map[string]any{
		"event_id": id, "type": typ, "venue_id": venue, "device_id": "dev-" + venue,
		"session_id": sid, "seq": 0, "occ_time": occ, "content": json.RawMessage(c),
	})
	return string(b)
}

func TestEndToEndFlow(t *testing.T) {
	svc := newTestService(t)
	srv := NewServer(svc)
	sid := "match-1"
	occ := func(m int) string {
		return time.Date(2026, 9, 22, 19, 0, m, 0, time.UTC).Format(time.RFC3339)
	}

	post := func(raw string) {
		if code, body := postEvent(t, srv, raw); code != http.StatusAccepted {
			t.Fatalf("事件应被接受，code=%d body=%v", code, body)
		}
	}

	// 注册两个点位。
	post(envelope("r1", model.TypeVenueRegister, "a", sid, occ(0),
		map[string]any{"level": model.LevelCore, "capacity": 100, "alternate_venues": []string{"b"}}))
	post(envelope("r2", model.TypeVenueRegister, "b", sid, occ(0),
		map[string]any{"level": model.LevelCoop, "capacity": 100}))
	// a 点 95% 饱和（权威快照），b 点空闲；均在新鲜窗口。
	post(envelope("sp1", model.TypeSnapshot, "a", sid, occ(10),
		map[string]any{"occupancy": 95, "authoritative": true}))
	post(envelope("sp2", model.TypeSnapshot, "b", sid, occ(10),
		map[string]any{"occupancy": 5, "authoritative": true}))
	// 心跳保证新鲜。
	post(envelope("h1", model.TypeHeartbeat, "a", sid, occ(15), map[string]any{"online": true}))
	post(envelope("h2", model.TypeHeartbeat, "b", sid, occ(15), map[string]any{"online": true}))

	// 总览：a 应暂停入场。
	get := func(path string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	code, sumBody := get("/v1/summary?session=" + sid + "&as_of=" + occ(16))
	if code != 200 {
		t.Fatalf("summary 失败: %v", sumBody)
	}
	venues := sumBody["venues"].([]any)
	var decisionID string
	for _, item := range venues {
		v := item.(map[string]any)
		if v["venue_id"] == "a" {
			if v["directive"] != model.DirectiveHold {
				t.Fatalf("a 点应暂停入场，实际 %v", v["directive"])
			}
			decisionID, _ = v["decision_id"].(string)
		}
	}
	if decisionID == "" {
		t.Fatal("未取得 decision_id")
	}

	// 决策解释：必须列出有效数据。
	code, decBody := get("/v1/decisions/" + decisionID + "?session=" + sid + "&as_of=" + occ(16))
	if code != 200 {
		t.Fatalf("决策解释失败: %v", decBody)
	}
	rec := decBody["recommendation"].(map[string]any)
	ev := rec["evidence"].(map[string]any)
	used := ev["used"].([]any)
	if len(used) == 0 {
		t.Fatal("导流判断必须可解释所用有效数据")
	}

	// 运营驳回 -> 审计。
	ackReq := map[string]any{
		"accepted": false, "operator_id": "op-9", "reason": "现场已人工疏导",
		"session_id": sid, "venue_id": "a", "occ_time": occ(50),
	}
	b, _ := json.Marshal(ackReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions/"+decisionID+"/ack", bytes.NewReader(b))
	recW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recW, req)
	if recW.Code != http.StatusAccepted {
		t.Fatalf("处置应被接受: %s", recW.Body.String())
	}
	code, auditBody := get("/v1/audit?session=" + sid + "&as_of=" + occ(59))
	if code != 200 {
		t.Fatalf("审计查询失败: %v", auditBody)
	}
	rows := auditBody["audit"].([]any)
	if len(rows) != 1 {
		t.Fatalf("应有 1 条审计，实际 %d", len(rows))
	}
	row := rows[0].(map[string]any)
	if row["accepted"] != false || row["operator_id"] != "op-9" {
		t.Fatalf("审计内容不符: %v", row)
	}
}

func TestPIIRejectedOverHTTP(t *testing.T) {
	svc := newTestService(t)
	srv := NewServer(svc)
	raw := `{"event_id":"p1","type":"count_report","venue_id":"a","session_id":"s",
"occ_time":"2026-09-22T19:00:00Z","content":{"enter":2,"phone":"13800138000"}}`
	code, body := postEvent(t, srv, raw)
	if code != http.StatusBadRequest {
		t.Fatalf("含 PII 的事件应返回 400，实际 %d %v", code, body)
	}
}

func TestDuplicateIsIdempotent(t *testing.T) {
	svc := newTestService(t)
	srv := NewServer(svc)
	raw := envelope("d1", model.TypeCount, "a", "s", "2026-09-22T19:00:00Z",
		map[string]any{"enter": 1, "exit": 0})
	c1, b1 := postEvent(t, srv, raw)
	c2, b2 := postEvent(t, srv, raw)
	if c1 != 202 || b1["duplicate"] != false {
		t.Fatalf("首次应 202 非重复: %d %v", c1, b1)
	}
	if c2 != 202 || b2["duplicate"] != true {
		t.Fatalf("二次应 202 且 duplicate=true: %d %v", c2, b2)
	}
}

func TestContentStopOrderVisible(t *testing.T) {
	svc := newTestService(t)
	srv := NewServer(svc)
	sid := "s"
	start := "2026-09-22T19:00:00Z"
	end := "2026-09-22T19:30:00Z"
	postEvent(t, srv, envelope("az", model.TypeAuthorize, "a", sid, start,
		map[string]any{"content_id": "feed-1", "start": start, "end": end}))
	req := httptest.NewRequest(http.MethodGet,
		"/v1/content?session="+sid+"&as_of=2026-09-22T19:45:00Z", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	rows := body["content"].([]any)
	cs := rows[0].(map[string]any)
	if cs["status"] != "expired" {
		t.Fatalf("授权应已过期，实际 %v", cs["status"])
	}
	if cs["stop_order"] == nil {
		t.Fatal("过期内容必须带有停发指令")
	}
}
