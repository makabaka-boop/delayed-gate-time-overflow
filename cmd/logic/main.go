// Command logic 启动门电路回放器的 HTTP 服务（Compose 中的 logic 服务）。
//
//	POST /simulate  body 为 logic.Request 的 JSON
//	  200 -> logic.Response
//	  400 -> {"error":"拒绝原因"}（重复驱动、组合环、非法同时翻转、JSON 非法等）
//	GET  /healthz -> ok
//
// 监听地址可用环境变量 LOGIC_ADDR 覆盖，默认 :8080。
package main

import (
	"log"
	"net/http"
	"os"

	"gateflow/internal/logic"
)

func main() {
	addr := os.Getenv("LOGIC_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Printf("logic service listening on %s", addr)
	if err := http.ListenAndServe(addr, logic.Handler()); err != nil {
		log.Fatal(err)
	}
}
