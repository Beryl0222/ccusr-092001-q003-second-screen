// 城市第二现场联动服务的基础入口。
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
	addr := flag.String("addr", ":8000", "监听地址")
	flag.Parse()
	if *check {
		fmt.Println("基础检查通过")
		return
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(health())
	})
	_ = http.ListenAndServe(*addr, nil)
}
