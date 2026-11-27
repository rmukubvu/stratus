package advise

import "sort"

// Verdict is the outcome of judging one option.
type Verdict string

const (
	VerdictFits         Verdict = "fits"
	VerdictFitsCaveats  Verdict = "fits_with_caveats"
	VerdictRuledOut     Verdict = "ruled_out"
	scorePerCaveat              = 15
	scorePerStrength            = 5
	maxScore                    = 100
	verdictRankFits             = 0
	verdictRankCaveats          = 1
	verdictRankRuledOut         = 2
)

// ReasonKind says how a reason bears on the verdict.
type ReasonKind string

const (
	// ReasonRuledOut breaks a hard requirement.
	ReasonRuledOut ReasonKind = "ruled_out"
	// ReasonCaveat works, with a cost or a condition worth recording.
	ReasonCaveat ReasonKind = "caveat"
	// ReasonStrength is something the option does well for this workload.
	ReasonStrength ReasonKind = "strength"
	// ReasonUnchecked could not be checked, for example because a quota
	// is not modelled. It never changes the verdict.
	ReasonUnchecked ReasonKind = "unchecked"
)

// Citation backs a reason with a documented quota, a page, or both.
type Citation struct {
	QuotaID string `json:"quota_id,omitempty"`
	URL     string `json:"url,omitempty"`
}

// Reason is one sentence of judgement about an option.
type Reason struct {
	Kind     ReasonKind `json:"kind"`
	Text     string     `json:"text"`
	Citation *Citation  `json:"citation,omitempty"`
}

// Assessment is one option judged against the requirements.
type Assessment struct {
	Option  string   `json:"option"`
	Verdict Verdict  `json:"verdict"`
	Score   int      `json:"score"`
	Reasons []Reason `json:"reasons"`
	// Cost is the option's monthly estimate, when the family prices it.
	Cost *Estimate `json:"cost,omitempty"`
}

// Judge builds an assessment from its reasons: any rule-out rules the
// option out, any caveat qualifies it, and the score starts at 100,
// loses 15 a caveat and gains 5 a strength. Strengths cannot buy back
// a caveat entirely: each caveat also lowers the ceiling by 5, so only
// an option without caveats scores 100.
func Judge(option string, reasons []Reason) Assessment {
	a := Assessment{Option: option, Verdict: VerdictFits, Reasons: append([]Reason{}, reasons...)}
	score, ceiling := maxScore, maxScore
	for _, r := range reasons {
		switch r.Kind {
		case ReasonRuledOut:
			a.Verdict = VerdictRuledOut
		case ReasonCaveat:
			if a.Verdict == VerdictFits {
				a.Verdict = VerdictFitsCaveats
			}
			score -= scorePerCaveat
			ceiling -= scorePerStrength
		case ReasonStrength:
			score += scorePerStrength
		}
	}
	switch {
	case a.Verdict == VerdictRuledOut:
		score = 0
	case score > ceiling:
		score = ceiling
	case score < 1:
		score = 1
	}
	a.Score = score
	return a
}

func (v Verdict) rank() int {
	switch v {
	case VerdictFits:
		return verdictRankFits
	case VerdictFitsCaveats:
		return verdictRankCaveats
	default:
		return verdictRankRuledOut
	}
}

// rank orders assessments best first: by verdict, then score, then
// lower ops overhead, then option ID so the order is stable.
func rank(as []Assessment, opsTotal func(option string) int) {
	sort.SliceStable(as, func(i, j int) bool {
		a, b := as[i], as[j]
		if a.Verdict.rank() != b.Verdict.rank() {
			return a.Verdict.rank() < b.Verdict.rank()
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if oa, ob := opsTotal(a.Option), opsTotal(b.Option); oa != ob {
			return oa < ob
		}
		return a.Option < b.Option
	})
}
