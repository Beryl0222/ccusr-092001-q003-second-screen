// 城市第二现场联动服务入口。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
)

const serviceID = "city-second-screen"

func health() map[string]string { return map[string]string{"status": "ok", "service": serviceID} }

func main() {
	check := flag.Bool("check", false, "检查基础配置")
	demo := flag.Bool("demo", false, "内置乱序重放演示（不写日志文件，输出汇总 JSON）")
	logPath := flag.String("log", "data/events.jsonl", "事件日志路径（空字符串表示纯内存）")
	addr := flag.String("addr", ":8000", "监听地址")
	flag.Parse()

	if *check {
		fmt.Println("基础检查通过")
		return
	}

	if *demo {
		if err := RunDemo(); err != nil {
			panic(err)
		}
		return
	}

	log, err := NewEventLog(*logPath)
	if err != nil {
		panic(err)
	}
	eng := NewEngine(log)
	srv := NewServer(eng)
	fmt.Printf("%s 启动，事件日志 %s，监听 %s\n", serviceID, *logPath, *addr)
	_ = http.ListenAndServe(*addr, srv.Routes())
}

// 以下保留旧入口的健康检查函数可被测试引用。
var _ = json.Marshal
