package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
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
	// q.StartBlockLoop() // disabled for sequential benchmark
	fmt.Fprintf(os.Stderr, "ready, reading from stdin\n")

	enc := json.NewEncoder(os.Stdout)

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req shared.QuoteRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			enc.Encode(&shared.QuoteResponse{Error: err.Error()})
			continue
		}
		resp, err := q.Quote(req)
		if err != nil {
			enc.Encode(&shared.QuoteResponse{ID: req.ID, Error: err.Error()})
			continue
		}
		resp.ID = req.ID
		enc.Encode(resp)
	}
}
