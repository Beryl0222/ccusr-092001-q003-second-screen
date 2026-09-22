package model

import (
	"strings"
	"testing"
	"time"
)

func TestRejectPII(t *testing.T) {
	now := time.Date(2026, 9, 22, 19, 0, 0, 0, time.UTC)
	base := `{
	  "event_id":"e1","type":"count_report","venue_id":"v1","device_id":"gate-3",
	  "session_id":"s1","seq":1,"occ_time":"2026-09-22T19:00:00Z",
	  "content":{"enter":3,"exit":1,%s}
	}`
	cases := map[string]string{
		"手机号字段": `"phone":"13800138000"`,
		"姓名字段":  `"姓名":"张三"`,
		"证件号字段": `"id_card":"110101199003071234"`,
		"人脸标识":  `"face_token":"abc"`,
		"观众ID":  `"visitor_id":"u-123"`,
		"手机号值":  `"note":"13800138000"`,
	}
	for name, inj := range cases {
		raw := strings.Replace(base, "%s", inj, 1)
		if _, err := ParseEnvelope([]byte(raw), now); err == nil {
			t.Fatalf("用例 %s 应被拒绝", name)
		} else if _, ok := err.(*ValidationError); !ok {
			t.Fatalf("用例 %s 应为校验错误，实际 %T", name, err)
		}
	}
}

func TestAcceptAggregateOnly(t *testing.T) {
	now := time.Date(2026, 9, 22, 19, 0, 0, 0, time.UTC)
	// 设备号、岗位号是基础设施标识，允许；聚合计数允许。
	raw := `{
	  "event_id":"e1","type":"count_report","venue_id":"v1","device_id":"gate-3",
	  "session_id":"s1","seq":1,"occ_time":"2026-09-22T19:00:00Z",
	  "content":{"enter":3,"exit":1,"gates":{"g1":{"enter":2,"exit":1}},"confidence":90}
	}`
	ev, err := ParseEnvelope([]byte(raw), now)
	if err != nil {
		t.Fatalf("聚合事件应被接受: %v", err)
	}
	if ev.Count.Enter != 3 || ev.Count.Gates["g1"].Enter != 2 || ev.Count.Confidence != 90 {
		t.Fatalf("计数解析错误: %+v", ev.Count)
	}
}

func TestEnumValidation(t *testing.T) {
	now := time.Now()
	raw := `{"event_id":"e","type":"venue_register","venue_id":"v","session_id":"s",
	  "occ_time":"2026-09-22T19:00:00Z","content":{"level":"虚构等级","capacity":10}}`
	if _, err := ParseEnvelope([]byte(raw), now); err == nil {
		t.Fatal("非法点位等级应被拒绝")
	}
}

func TestMissingFields(t *testing.T) {
	now := time.Now()
	if _, err := ParseEnvelope([]byte(`{"type":"count_report"}`), now); err == nil {
		t.Fatal("缺少必填字段应被拒绝")
	}
}
