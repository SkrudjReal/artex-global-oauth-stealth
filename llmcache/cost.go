package llmcache

// Rates holds optional manually configured token pricing in USD per 1M tokens.
// Nil fields mean the rate is unknown and must never be guessed or defaulted to assumed cache discounts.
type Rates struct {
	InputRatePer1M       *float64 `json:"input_rate_per_1m,omitempty"`
	CachedInputRatePer1M *float64 `json:"cached_input_rate_per_1m,omitempty"`
	OutputRatePer1M      *float64 `json:"output_rate_per_1m,omitempty"`
}

// DiscountStatus describes whether a billing discount on cached tokens is verified.
type DiscountStatus string

const (
	DiscountUnknown   DiscountStatus = "unknown"
	DiscountConfirmed DiscountStatus = "confirmed"
	DiscountNone      DiscountStatus = "none"
)

// CostEstimate summarizes token cost accounting without fabricating unconfirmed cache discounts.
type CostEstimate struct {
	RatesConfigured   bool           `json:"rates_configured"`
	DiscountStatus    DiscountStatus `json:"discount_status"`
	DiscountPercent   *float64       `json:"discount_percent,omitempty"`
	UncachedCostUSD   *float64       `json:"uncached_cost_usd,omitempty"`
	CachedCostUSD     *float64       `json:"cached_cost_usd,omitempty"`
	OutputCostUSD     *float64       `json:"output_cost_usd,omitempty"`
	TotalCostUSD      *float64       `json:"total_cost_usd,omitempty"`
	SavingsUSD        *float64       `json:"savings_usd,omitempty"`
	MaxWithoutDiscUSD *float64       `json:"max_without_discount_usd,omitempty"`
}

// ComputeCost calculates exact token cost using configured rates and cached/uncached counts.
// When cached_tokens > 0 and CachedInputRatePer1M is nil, TotalCostUSD stays nil and DiscountStatus
// stays "unknown" so ARTEX never invents an unverified reseller discount.
func ComputeCost(uncachedInput, cachedInput, output int64, rates Rates) CostEstimate {
	est := CostEstimate{
		DiscountStatus: DiscountUnknown,
	}
	if rates.InputRatePer1M == nil && rates.CachedInputRatePer1M == nil && rates.OutputRatePer1M == nil {
		return est
	}
	est.RatesConfigured = true

	if rates.InputRatePer1M != nil && *rates.InputRatePer1M >= 0 {
		v := float64(uncachedInput) * (*rates.InputRatePer1M) / 1_000_000.0
		est.UncachedCostUSD = &v
	}
	if rates.OutputRatePer1M != nil && *rates.OutputRatePer1M >= 0 {
		v := float64(output) * (*rates.OutputRatePer1M) / 1_000_000.0
		est.OutputCostUSD = &v
	}
	if rates.CachedInputRatePer1M != nil && *rates.CachedInputRatePer1M >= 0 {
		v := float64(cachedInput) * (*rates.CachedInputRatePer1M) / 1_000_000.0
		est.CachedCostUSD = &v
	}

	if rates.InputRatePer1M != nil && rates.CachedInputRatePer1M != nil {
		inR := *rates.InputRatePer1M
		cR := *rates.CachedInputRatePer1M
		if inR > 0 && cR < inR {
			est.DiscountStatus = DiscountConfirmed
			pct := (inR - cR) / inR * 100.0
			est.DiscountPercent = &pct
			sav := float64(cachedInput) * (inR - cR) / 1_000_000.0
			est.SavingsUSD = &sav
		} else {
			est.DiscountStatus = DiscountNone
			zero := 0.0
			est.DiscountPercent = &zero
			est.SavingsUSD = &zero
		}
	}

	// Upper bound if cached tokens were billed at regular input rate.
	if rates.InputRatePer1M != nil && rates.OutputRatePer1M != nil {
		maxUSD := float64(uncachedInput+cachedInput)*(*rates.InputRatePer1M)/1_000_000.0 +
			float64(output)*(*rates.OutputRatePer1M)/1_000_000.0
		est.MaxWithoutDiscUSD = &maxUSD
	}

	// Exact TotalCostUSD is only reported when every non-zero component has an explicit rate.
	if est.UncachedCostUSD != nil && est.OutputCostUSD != nil {
		if cachedInput == 0 {
			tot := *est.UncachedCostUSD + *est.OutputCostUSD
			est.TotalCostUSD = &tot
		} else if est.CachedCostUSD != nil {
			tot := *est.UncachedCostUSD + *est.CachedCostUSD + *est.OutputCostUSD
			est.TotalCostUSD = &tot
		}
	}

	return est
}
