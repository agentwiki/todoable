package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync"
)

func main() {
	var mu sync.Mutex
	http.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		b, e := io.ReadAll(r.Body)
		if e != nil {
			http.Error(w, e.Error(), 500)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		f, e := os.OpenFile("/tmp/events.jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			http.Error(w, e.Error(), 500)
			return
		}
		defer f.Close()
		json.NewEncoder(f).Encode(map[string]any{"delivery": r.Header.Get("X-Gitea-Delivery"), "event": r.Header.Get("X-Gitea-Event"), "body": json.RawMessage(b)})
		w.WriteHeader(204)
	})
	panic(http.ListenAndServe("127.0.0.1:9876", nil))
}
