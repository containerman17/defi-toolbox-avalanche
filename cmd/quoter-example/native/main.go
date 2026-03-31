package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sync"

	"defi-toolbox/cmd/quoter-example/shared"
	"defi-toolbox/statedb"
)

func main() {
	stateServer := flag.String("state-server", "ws://localhost:7449/live", "state server WebSocket URL")
	poolLimit := flag.Int("pool-limit", 5000, "max pools to load")
	maxHops := flag.Int("max-hops", 4, "max hops per route")
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

	q := shared.NewQuoter(ls, *poolLimit, *maxHops)
	q.StartBlockLoop()
	fmt.Fprintf(os.Stderr, "ready, reading from stdin\n")

	var outMu sync.Mutex
	enc := json.NewEncoder(os.Stdout)

	writeResp := func(resp *shared.QuoteResponse) {
		outMu.Lock()
		enc.Encode(resp)
		outMu.Unlock()
	}

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := make([]byte, len(scanner.Bytes()))
		copy(line, scanner.Bytes())

		go func() {
			var req shared.QuoteRequest
			if err := json.Unmarshal(line, &req); err != nil {
				writeResp(&shared.QuoteResponse{Error: err.Error()})
				return
			}
			resp, err := q.Quote(req)
			if err != nil {
				writeResp(&shared.QuoteResponse{ID: req.ID, Error: err.Error()})
				return
			}
			resp.ID = req.ID
			writeResp(resp)
		}()
	}
}
