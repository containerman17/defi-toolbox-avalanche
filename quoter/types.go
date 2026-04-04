package quoter

// QuoteRequest is the input for a two-way quote.
// ID is optional — if set, it is echoed back in the response for matching.
type QuoteRequest struct {
	ID       int    `json:"id,omitempty"`
	TokenIn  string `json:"tokenIn"`
	TokenOut string `json:"tokenOut"`
	AmountIn string `json:"amountIn"`
	Split    bool   `json:"split,omitempty"` // if true, run split routing via splitter.Split()
}

// QuoteResponse contains forward and (optionally) reverse quotes.
// Reverse is nil when tokenIn == tokenOut (cyclic/round-trip).
type QuoteResponse struct {
	ID      int          `json:"id,omitempty"`
	Forward *QuoteResult `json:"forward"`
	Reverse *QuoteResult `json:"reverse,omitempty"`
	Split   *SplitResult `json:"split,omitempty"` // present when Split=true
	Error   string       `json:"error,omitempty"`
}

// SplitResult is the output of split routing.
type SplitResult struct {
	AmountOut string     `json:"amountOut"`
	Legs      []SplitLeg `json:"legs"`
	TotalGas  uint64     `json:"totalGas"`
	ElapsedUs int64      `json:"elapsedUs"`
}

// SplitLeg is one piece of a split route.
type SplitLeg struct {
	AmountIn  string     `json:"amountIn"`
	AmountOut string     `json:"amountOut"`
	Path      []PathStep `json:"path"`
	GasUsed   uint64     `json:"gasUsed"`
}

// QuoteResult is one direction of a quote.
type QuoteResult struct {
	TokenIn   string     `json:"tokenIn"`
	TokenOut  string     `json:"tokenOut"`
	AmountIn  string     `json:"amountIn"`
	AmountOut string     `json:"amountOut"`
	Path      []PathStep `json:"path"`
	GasUsed   uint64     `json:"gasUsed"`
}

// PathStep is a single hop in a swap path.
type PathStep struct {
	Pool     string `json:"pool"`
	TokenIn  string `json:"tokenIn"`
	TokenOut string `json:"tokenOut"`
	Dex      string `json:"dex"`
}
