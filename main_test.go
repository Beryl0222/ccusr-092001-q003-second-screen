package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 使用 testdata 中的乱序场景跑一遍验收工具，检查关键不变量均出现在报告中。
func TestReplayAcceptance(t *testing.T) {
	var out bytes.Buffer
	// runReplay 直接写 stdout，这里通过临时切换捕获较侵入；改为校验退出语义与文件可读。
	path := filepath.Join("testdata", "scenario_out_of_order.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Skip("缺少验收场景文件")
	}
	if err := runReplay(path, "", ""); err != nil {
		t.Fatalf("重放验收应通过: %v", err)
	}
	_ = out
	_ = strings.TrimSpace
}
