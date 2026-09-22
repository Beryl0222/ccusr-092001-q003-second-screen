package store

import (
	"path/filepath"
	"testing"
	"time"
)

const sampleEvent = `{"event_id":"e1","type":"count_report","venue_id":"v1","device_id":"d1",
"session_id":"s1","seq":1,"occ_time":"2026-09-22T19:00:00Z","content":{"enter":5,"exit":1}}`

func TestIdempotentAppend(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 9, 22, 19, 0, 1, 0, time.UTC)
	_, r1, err := s.Append([]byte(sampleEvent), now)
	if err != nil || r1.Duplicate || r1.Version != 1 {
		t.Fatalf("首次追加异常: %+v err=%v", r1, err)
	}
	// 同一设备重复上报（相同 event_id 的重试）-> 幂等，版本不变。
	_, r2, err := s.Append([]byte(sampleEvent), now.Add(time.Minute))
	if err != nil || !r2.Duplicate || r2.Version != 1 {
		t.Fatalf("重复追加应幂等: %+v err=%v", r2, err)
	}
	if s.Version() != 1 {
		t.Fatalf("全局版本应停留在 1，实际 %d", s.Version())
	}
	evs, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("日志应只有 1 条，实际 %d", len(evs))
	}
	// 重复读取应保留首次接收时间。
	if !evs[0].ReceivedAt.Equal(now) {
		t.Fatal("幂等重放不应改写首次接收时间")
	}
}

func TestSinceAndRecovery(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 22, 19, 0, 0, 0, time.UTC)

	mk := func(id string, seq int64) []byte {
		return []byte(`{"event_id":"` + id + `","type":"count_report","venue_id":"v1","device_id":"d1",` +
			`"session_id":"s1","seq":` + itoa(seq) + `,"occ_time":"2026-09-22T19:00:` +
			pad2(seq) + `Z","content":{"enter":1,"exit":0}}`)
	}
	func() {
		s, err := Open(filepath.Join(dir, "log"))
		if err != nil {
			t.Fatal(err)
		}
		for i := int64(1); i <= 3; i++ {
			if _, _, err := s.Append(mk("e"+itoa(i), i), now.Add(time.Duration(i)*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
		s.Close()
	}()

	// 重新打开：WAL 恢复索引。
	s2, err := Open(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.Version() != 3 {
		t.Fatalf("恢复后版本应为 3，实际 %d", s2.Version())
	}
	// 增量恢复：只取版本 > 2。
	evs, err := s2.Since(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].EventID != "e3" {
		t.Fatalf("Since(2) 应只返回 e3，实际 %+v", evs)
	}
	// 重复 e2 仍然去重，新版本继续分配为 4。
	_, r, err := s2.Append(mk("e2", 2), now.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Duplicate {
		t.Fatal("恢复后重复事件应继续识别")
	}
	_, r, err = s2.Append(mk("e4", 4), now.Add(11*time.Second))
	if err != nil || r.Duplicate || r.Version != 4 {
		t.Fatalf("新事件版本应为 4，实际 %+v", r)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func pad2(n int64) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}
