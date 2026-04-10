package main

// dump-size: Connect as subscriber, receive initial dump, report size stats.

import (
	"flag"
	"fmt"
	"os"

	"defi-toolbox/statedb"
)

func main() {
	url := flag.String("state-server", "ws://localhost:7449/live", "state server URL")
	flag.Parse()

	fmt.Fprintf(os.Stderr, "connecting to %s…\n", *url)
	ls, err := statedb.Connect(*url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		os.Exit(1)
	}

	im := ls.State().Immutable()
	storageCount := 0
	for _, slots := range im.Storage {
		storageCount += len(slots)
	}

	fmt.Fprintf(os.Stderr, "block=%d  contracts=%d  storage_slots=%d  accounts(code)=%d  balances=%d\n",
		ls.Block(), len(im.Storage), storageCount, len(im.Code), len(im.Balance))
	ls.Close()
}
