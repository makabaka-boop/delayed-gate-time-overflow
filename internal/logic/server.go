package logic

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Handler 返回 logic 服务的 HTTP 路由：
//
//	POST /simulate  body 为 Request JSON；200 返回 Response，400 返回 {"error":...}
//	GET  /healthz   存活探针
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/simulate", simulateHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func simulateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	defer r.Body.Close()

	var req Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON 解析失败: " + err.Error()})
		return
	}

	resp, err := Run(&req)
	if err != nil {
		var rej *ErrReject
		if errors.As(err, &rej) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": rej.Reason})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
