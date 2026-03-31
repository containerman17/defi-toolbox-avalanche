package shared

// QuoteRequest is the input for a two-way quote.
// ID is optional — if set, it is echoed back in the response for matching.
type QuoteRequest struct {
	ID       int    `json:"id,omitempty"`
	TokenIn  string `json:"tokenIn"`
	TokenOut string `json:"tokenOut"`
	AmountIn string `json:"amountIn"`
}

// QuoteResponse contains forward and (optionally) reverse quotes.
// Reverse is nil when tokenIn == tokenOut (cyclic/round-trip).
type QuoteResponse struct {
	ID      int          `json:"id,omitempty"`
	Forward *QuoteResult `json:"forward"`
	Reverse *QuoteResult `json:"reverse,omitempty"`
	Error   string       `json:"error,omitempty"`
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
