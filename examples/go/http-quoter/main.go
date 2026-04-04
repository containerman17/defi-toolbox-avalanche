package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"

	"defi-toolbox/quoter"
	"defi-toolbox/statedb"
)

func main() {
	stateServer := flag.String("state-server", "ws://localhost:7449/live", "state server WebSocket URL")
	poolLimit := flag.Int("pool-limit", 5000, "max pools to load")
	maxHops := flag.Int("max-hops", 4, "max hops per route")
	listen := flag.String("listen", ":8080", "HTTP listen address")
	flag.Parse()
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unknown argument: %s\n", flag.Arg(0))
		flag.Usage()
		os.Exit(1)
	}

	ls, err := statedb.Connect(*stateServer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "connected to %s, block=%d\n", *stateServer, ls.Block())

	q := quoter.NewQuoter(ls, *poolLimit, *maxHops)
	q.StartBlockLoop()

	http.HandleFunc("/quote", func(w http.ResponseWriter, r *http.Request) {
		req := quoter.QuoteRequest{
			TokenIn:  r.URL.Query().Get("tokenIn"),
			TokenOut: r.URL.Query().Get("tokenOut"),
			AmountIn: r.URL.Query().Get("amountIn"),
			Split:    r.URL.Query().Get("split") == "true",
		}
		if req.TokenIn == "" || req.TokenOut == "" || req.AmountIn == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "missing tokenIn, tokenOut, or amountIn"})
			return
		}
		resp, err := q.Quote(req)
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	fmt.Fprintf(os.Stderr, "listening on %s\n", *listen)
	if err := http.ListenAndServe(*listen, nil); err != nil {
		fmt.Fprintf(os.Stderr, "listen failed: %v\n", err)
		os.Exit(1)
	}
}
