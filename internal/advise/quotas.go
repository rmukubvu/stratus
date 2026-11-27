package advise

// Quota is a documented AWS default quota, as a family's rules read it.
type Quota struct {
	ID     string
	Value  float64
	Source string
}

// QuotaSource looks up AWS's documented quotas. The app backs it with
// internal/limits, so advice, enforced limits and load runs share one set
// of numbers.
type QuotaSource interface {
	Quota(id, region string) (Quota, bool)
}

// QuotaFunc adapts a function to QuotaSource.
type QuotaFunc func(id, region string) (Quota, bool)

// Quota calls f.
func (f QuotaFunc) Quota(id, region string) (Quota, bool) { return f(id, region) }
