package advise

// PriceTier is one band of a tiered AWS price: units from From up to To
// (no upper bound when To is 0) cost USD each.
type PriceTier struct {
	From float64
	To   float64
	USD  float64
}

// Price is one AWS on-demand price in one region, as a family's cost
// model reads it.
type Price struct {
	ID    string
	Unit  string
	Tiers []PriceTier
	// AsOf is the date AWS published the price, YYYY-MM-DD.
	AsOf string
	// Source is the AWS price list it came from.
	Source string
}

// Cost prices quantity units across the tiers, as AWS bills tiered usage.
func (p Price) Cost(quantity float64) float64 {
	total := 0.0
	for _, t := range p.Tiers {
		if quantity <= t.From {
			break
		}
		upper := quantity
		if t.To != 0 && t.To < upper {
			upper = t.To
		}
		total += (upper - t.From) * t.USD
	}
	return total
}

// PriceBook looks up AWS prices. The app backs it with the dated snapshot
// in internal/pricing, so every estimate says how old its prices are.
type PriceBook interface {
	Price(id, region string) (Price, bool)
}

// PriceFunc adapts a function to PriceBook.
type PriceFunc func(id, region string) (Price, bool)

// Price calls f.
func (f PriceFunc) Price(id, region string) (Price, bool) { return f(id, region) }

// Coster is implemented by a family that can estimate what each option
// costs a month. A family without one still assesses; its ADR has no cost
// section.
type Coster interface {
	Costs(Requirements) []Estimate
}

// CostLine is one priced item of an estimate.
type CostLine struct {
	Item     string  `json:"item"`
	PriceID  string  `json:"price_id"`
	Quantity float64 `json:"quantity"`
	Unit     string  `json:"unit"`
	USD      float64 `json:"usd"`
}

// Estimate is an option's monthly cost at the stated average load, from
// on-demand list prices. It never hides a price it could not find: those
// go in Unpriced, and the total is then a lower bound.
type Estimate struct {
	Option     string     `json:"option"`
	Region     string     `json:"region"`
	MonthlyUSD float64    `json:"monthly_usd"`
	Lines      []CostLine `json:"lines"`
	// Unpriced names prices missing from the price book for the region.
	Unpriced []string `json:"unpriced,omitempty"`
	// Assumptions say what the usage model took for granted, such as no
	// batching or three brokers.
	Assumptions []string `json:"assumptions,omitempty"`
	PricesAsOf  string   `json:"prices_as_of,omitempty"`
	Source      string   `json:"source,omitempty"`
}

// Complete reports whether every price the estimate needed was found.
func (e Estimate) Complete() bool { return len(e.Unpriced) == 0 }

// EstimateBuilder adds up an estimate line by line for one option.
type EstimateBuilder struct {
	book PriceBook
	e    Estimate
}

// NewEstimate starts an estimate for option in region.
func NewEstimate(book PriceBook, option, region string) *EstimateBuilder {
	return &EstimateBuilder{book: book, e: Estimate{Option: option, Region: region}}
}

// Add prices quantity units of a price. A missing price is recorded as
// unpriced rather than counted as free.
func (b *EstimateBuilder) Add(item, priceID string, quantity float64) *EstimateBuilder {
	var p Price
	ok := b.book != nil
	if ok {
		p, ok = b.book.Price(priceID, b.e.Region)
	}
	if !ok {
		b.e.Unpriced = append(b.e.Unpriced, priceID)
		return b
	}
	usd := p.Cost(quantity)
	b.e.Lines = append(b.e.Lines, CostLine{Item: item, PriceID: priceID, Quantity: quantity, Unit: p.Unit, USD: usd})
	b.e.MonthlyUSD += usd
	if p.AsOf > b.e.PricesAsOf {
		b.e.PricesAsOf = p.AsOf
	}
	if b.e.Source == "" {
		b.e.Source = p.Source
	}
	return b
}

// Assume records an assumption of the usage model.
func (b *EstimateBuilder) Assume(text string) *EstimateBuilder {
	b.e.Assumptions = append(b.e.Assumptions, text)
	return b
}

// Build returns the estimate.
func (b *EstimateBuilder) Build() Estimate { return b.e }
