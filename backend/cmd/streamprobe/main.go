// Command streamprobe connects to the API's live stream and reports what
// arrives. It is a verification tool, not part of the service.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"sort"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func main() {
	url := flag.String("url", "ws://localhost:8080/api/v1/stream", "stream url")
	dur := flag.Duration("for", 60*time.Second, "how long to listen")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, *url, nil)
	if err != nil {
		fmt.Println("dial failed:", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(4 << 20)

	_ = wsjson.Write(ctx, conn, map[string]any{
		"op": "sub", "topics": []string{"pools.new", "pools.update", "head", "rpc.health", "watch.hits"},
	})

	counts := map[string]int{}
	var samples []string
	backfilledNew := 0
	for {
		var f struct {
			T     string          `json:"t"`
			Seq   uint64          `json:"seq"`
			Chain string          `json:"chain"`
			D     json.RawMessage `json:"d"`
		}
		if err := wsjson.Read(ctx, conn, &f); err != nil {
			break
		}
		counts[f.T]++
		var p struct {
			Pair       string `json:"pair"`
			Dex        string `json:"dex"`
			Backfilled bool   `json:"backfilled"`
			Buyers     *int   `json:"buyers1h"`
			Sellers    *int   `json:"sellers1h"`
			Risk       *struct {
				Band  string `json:"band"`
				Score int    `json:"score"`
			} `json:"risk"`
		}
		_ = json.Unmarshal(f.D, &p)
		switch f.T {
		case "pools.new":
			if p.Backfilled {
				backfilledNew++
			}
			if len(samples) < 6 {
				samples = append(samples, fmt.Sprintf("NEW    %-5s %-28s %s", f.Chain, p.Pair, p.Dex))
			}
		case "pools.update":
			if p.Pair != "" && p.Risk != nil && len(samples) < 12 {
				samples = append(samples, fmt.Sprintf("UPDATE %-5s %-28s buyers=%v sellers=%v %s %d",
					f.Chain, p.Pair, deref(p.Buyers), deref(p.Sellers), p.Risk.Band, p.Risk.Score))
			}
		}
	}

	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("frames received in %s:\n", *dur)
	for _, k := range keys {
		fmt.Printf("  %-14s %d\n", k, counts[k])
	}
	fmt.Println("backfilled pools sent as pools.new:", backfilledNew)
	fmt.Println("samples:")
	for _, s := range samples {
		fmt.Println("  " + s)
	}
}

func deref(p *int) any {
	if p == nil {
		return "—"
	}
	return *p
}
