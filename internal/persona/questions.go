package persona

import (
	"fmt"
	"sort"
	"strconv"
)

// Question types (BENCHMARK-V2.md §5.1). QTAbstention MUST contain "abstention"
// — the scorer/judge key their needle-absent clause on that substring.
const (
	QTSingleSession         = "single-session-recall"
	QTMultiSession          = "multi-session"
	QTTemporal              = "temporal-reasoning"
	QTKnowledgeUpdate       = "knowledge-update"
	QTPreference            = "preference"
	QTPreferenceApplication = "preference-application"
	QTContradiction         = "contradiction"
	QTAbstention            = "abstention"
)

// Difficulty tiers (§4.3). Fixed per-run quotas over these make difficulty
// identical across seeds — a variance reducer and a calibration lever.
const (
	TierEasy   = "easy"
	TierMedium = "medium"
	TierHard   = "hard"
)

// Question is a derived memory-suite question with its exact ground truth. It is
// a pure function of the plan (DeriveQuestions takes no entropy): the seed's
// randomness is already baked into the plan. Answer is the canonical expected
// answer used by the deterministic grader; Numeric marks a count answer (the
// deterministic number-token path). Abstain marks a needle-absent question whose
// correct behavior is a grounded decline (Answer empty — the gen layer fills the
// decline sentinel). Evidence lists the fact IDs the answer depends on.
type Question struct {
	ID       string
	Type     string
	Tier     string
	Text     string
	Answer   string
	Numeric  bool
	Abstain  bool
	Evidence []string
}

// scalar recall question text, keyed by attribute. "current" wording disambiguates
// the latest-value-wins semantics for updated attributes.
var scalarAsk = map[string]string{
	"city":       "Which city do I currently live in?",
	"occupation": "What is my current job?",
	"employer":   "Who is my current employer?",
	"car":        "What car do I currently drive?",
	"hometown":   "What is my hometown?",
	"partner":    "What is my partner's name?",
	"instrument": "What instrument do I play?",
	"alma_mater": "Where did I go to university?",
	// software domain
	"primary_language": "What is my primary programming language?",
	"code_editor":      "What code editor do I use?",
	// medical domain
	"diagnosis":  "What is my current medical diagnosis?",
	"medication": "What medication am I currently taking?",
	// legal domain
	"practice_area": "What area of law do I practice?",
	"bar_admission": "Which state's bar am I admitted to?",
	// finance domain
	"risk_tolerance": "What is my current risk tolerance?",
	"brokerage":      "Which brokerage do I use?",
}

var prefAsk = map[string]string{
	"favorite_cuisine": "What is my favorite cuisine?",
	"dietary":          "What is my dietary preference?",
	"favorite_color":   "What is my favorite color?",
}

// prefApply is the preference-APPLICATION request: a task whose correct answer
// must honor the seeded preference without the preference being restated (§5.1).
var prefApply = map[string]string{
	"favorite_cuisine": "I'm booking a restaurant for dinner tonight. What kind of place should I pick for me?",
	"dietary":          "Suggest a main course for me to cook this evening.",
	"favorite_color":   "I'm repainting my study and want a color I'll love. What would you suggest for me?",
}

var listCountAsk = map[string]string{
	"project": "How many different projects have I told you about?",
	"trip":    "How many separate trips have I mentioned taking?",
	"pet":     "How many pets do I have?",
	// domain list families
	"service":      "How many services do I maintain?",
	"allergy":      "How many things am I allergic to?",
	"legal_matter": "How many legal matters am I handling?",
	"holding":      "How many holdings are in my portfolio?",
}

var listAllAsk = map[string]string{
	"project": "List all the projects I have mentioned.",
	"trip":    "Which places have I told you I traveled to?",
	"pet":     "What are the names of all my pets?",
	// domain list families
	"service":      "List all the services I maintain.",
	"allergy":      "What am I allergic to?",
	"legal_matter": "List all the legal matters I've told you about.",
	"holding":      "List all the holdings in my portfolio.",
}

// absentAttributes are plausible personal facts the persona generator NEVER
// emits, so a question about one is genuinely needle-absent — the grounded-
// decline (abstention) material, with no evidence to withhold.
var absentAttributes = []string{
	"What is my blood type?",
	"What is my shoe size?",
	"Which month is my birthday in?",
	"How tall am I?",
	"What is my favorite song?",
	"What is my star sign?",
	"What is my middle name?",
	"What is my favorite sports team?",
	"What is my eye color?",
	"What is my favorite film?",
	"What is my mobile phone number?",
}

// DeriveQuestions builds the full candidate question pool from a plan. It is
// deterministic and side-effect free; the gen layer stratifies + samples this
// pool to the run's memory-case quota. Every question carries exact ground truth
// derived from the plan's canonical values.
func DeriveQuestions(p *Plan) []Question {
	var qs []Question
	// distractorAttrs: attributes with a near-miss decoy present (bumps tier).
	distractorAttrs := map[string]bool{}
	for _, f := range p.Facts {
		if f.Kind == KindDistractor {
			distractorAttrs[f.Attribute] = true
		}
	}

	// --- scalar recall + knowledge-update ---
	// Data-driven: iterate the plan's current scalar facts (universal AND any
	// domain facts) and look the question phrasing up by attribute, so adding a
	// domain fact family requires only pool + spec + scalarAsk/factLabel entries.
	for _, cur := range currentScalarFacts(p) {
		ask := scalarAsk[cur.Attribute]
		if ask == "" {
			continue
		}
		if cur.Supersedes != "" {
			// updated attribute → knowledge-update (latest value wins).
			qs = append(qs, Question{
				ID:       "q-ku-" + cur.Attribute,
				Type:     QTKnowledgeUpdate,
				Tier:     pick3(distractorAttrs[cur.Attribute], TierHard, TierMedium),
				Text:     ask,
				Answer:   cur.Value,
				Evidence: []string{cur.ID, cur.Supersedes},
			})
		} else {
			qs = append(qs, Question{
				ID:       "q-rec-" + cur.Attribute,
				Type:     QTSingleSession,
				Tier:     pick3(distractorAttrs[cur.Attribute], TierMedium, TierEasy),
				Text:     ask,
				Answer:   cur.Value,
				Evidence: []string{cur.ID},
			})
		}
	}

	// --- preference recall + application ---
	for _, f := range p.Facts {
		if f.Kind != KindPreference {
			continue
		}
		if ask := prefAsk[f.Attribute]; ask != "" {
			qs = append(qs, Question{
				ID:       "q-pref-" + f.Attribute,
				Type:     QTPreference,
				Tier:     TierEasy,
				Text:     ask,
				Answer:   f.Value,
				Evidence: []string{f.ID},
			})
		}
		if req := prefApply[f.Attribute]; req != "" {
			qs = append(qs, Question{
				ID:       "q-prefapp-" + f.Attribute,
				Type:     QTPreferenceApplication,
				Tier:     TierMedium,
				Text:     req,
				Answer:   f.Value, // the honored preference must surface in the answer
				Evidence: []string{f.ID},
			})
		}
	}

	// --- multi-session synthesis over list attributes (count questions) ---
	// Iterate the distinct list attributes present (universal + domain) in
	// timeline order, so a domain list family (e.g. services, medications) is
	// picked up from listCountAsk/listAllAsk without touching this loop.
	for _, attr := range listAttributesPresent(p) {
		items := listFacts(p, attr)
		if len(items) == 0 {
			continue
		}
		ask := listCountAsk[attr]
		if ask == "" {
			continue
		}
		ev := make([]string, 0, len(items))
		sessions := map[int]bool{}
		for _, f := range items {
			ev = append(ev, f.ID)
			sessions[f.Session] = true
		}
		qs = append(qs, Question{
			ID:       "q-count-" + attr,
			Type:     QTMultiSession,
			Tier:     pick3(len(sessions) >= 4, TierHard, TierMedium),
			Text:     ask,
			Answer:   strconv.Itoa(len(items)),
			Numeric:  true,
			Evidence: ev,
		})
		// list-all variant (judge-graded synthesis): the answer must enumerate
		// every item, so under-recall is penalized even when the count is known.
		vals := make([]string, 0, len(items))
		for _, f := range items {
			vals = append(vals, f.Value)
		}
		qs = append(qs, Question{
			ID:       "q-list-" + attr,
			Type:     QTMultiSession,
			Tier:     pick3(len(sessions) >= 4, TierHard, TierMedium),
			Text:     listAllAsk[attr],
			Answer:   joinComma(vals),
			Evidence: ev,
		})
	}

	// --- contradiction (change-of-mind reversals) ---
	for _, f := range p.Facts {
		if f.Kind != KindOpinion || !f.Reversal {
			continue
		}
		qs = append(qs, Question{
			ID:       "q-contra-" + f.ID,
			Type:     QTContradiction,
			Tier:     TierHard,
			Text:     fmt.Sprintf("How do I feel about %s these days?", f.Value),
			Answer:   fmt.Sprintf("I no longer do %s — I used to enjoy it but have since given it up.", f.Value),
			Evidence: []string{f.ID, f.Supersedes},
		})
	}

	// --- temporal ordering (which came first) ---
	qs = append(qs, temporalQuestions(p)...)

	// --- abstention (needle-absent) ---
	for i, q := range absentAttributes {
		qs = append(qs, Question{
			ID:      "q-abs-" + strconv.Itoa(i),
			Type:    QTAbstention,
			Tier:    TierMedium,
			Text:    q,
			Abstain: true,
		})
	}

	return qs
}

// temporalQuestions derives ordering questions from dated self-facts. The answer
// is a descriptive phrase (judge-graded); the two events are described by their
// attribute label + value so the question is answerable purely from the seeded
// timeline.
func temporalQuestions(p *Plan) []Question {
	type dated struct {
		f     Fact
		label string
	}
	var evs []dated
	for _, f := range p.Facts {
		if f.Entity != "self" || !f.Current {
			continue
		}
		lbl := factLabel(f)
		if lbl == "" {
			continue
		}
		evs = append(evs, dated{f: f, label: lbl})
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].f.Seq < evs[j].f.Seq })

	var qs []Question
	for i := 0; i+1 < len(evs); i += 2 {
		a, b := evs[i], evs[i+1]
		if a.f.Session == b.f.Session {
			continue // need a genuine ordering across sessions
		}
		// a precedes b by construction (sorted by Seq; earlier session first).
		earlier, later := a, b
		if earlier.f.Session > later.f.Session {
			earlier, later = later, earlier
		}
		gap := abs(later.f.Session - earlier.f.Session)
		qs = append(qs, Question{
			ID:   "q-temp-" + earlier.f.ID + "-" + later.f.ID,
			Type: QTTemporal,
			Tier: pick3(gap >= 3, TierHard, TierMedium),
			Text: fmt.Sprintf("Which did I tell you about first: %s, or %s?", earlier.label, later.label),
			Answer: fmt.Sprintf("You mentioned %s before %s.",
				earlier.label, later.label),
			Evidence: []string{earlier.f.ID, later.f.ID},
		})
	}
	return qs
}

// factLabel renders a short natural descriptor of a fact for temporal questions
// (e.g. "moving to Lisbon", "your Volvo XC40", "your trip to Osaka").
func factLabel(f Fact) string {
	switch f.Attribute {
	case "city":
		return "moving to " + f.Value
	case "occupation":
		return "becoming a " + f.Value
	case "employer":
		return "joining " + f.Value
	case "car":
		return "getting your " + f.Value
	case "hometown":
		return "growing up in " + f.Value
	case "partner":
		return "your partner " + f.Value
	case "instrument":
		return "playing the " + f.Value
	case "alma_mater":
		return "studying at " + f.Value
	case "project":
		return "starting " + f.Value
	case "trip":
		return "your trip to " + f.Value
	case "pet":
		return "adopting " + f.Display
	case "favorite_cuisine":
		return "your love of " + f.Value + " food"
	case "favorite_color":
		return "your favorite color " + f.Value
	// software domain
	case "primary_language":
		return "picking up " + f.Value
	case "code_editor":
		return "switching to " + f.Value
	case "service":
		return "taking on the " + f.Value + " service"
	// medical domain
	case "diagnosis":
		return "being diagnosed with " + f.Value
	case "medication":
		return "starting " + f.Value
	case "allergy":
		return "your " + f.Value + " allergy"
	// legal domain
	case "practice_area":
		return "moving into " + f.Value
	case "bar_admission":
		return "being admitted to the " + f.Value + " bar"
	case "legal_matter":
		return "taking on " + f.Value
	// finance domain
	case "risk_tolerance":
		return "adopting a " + f.Value + " risk tolerance"
	case "brokerage":
		return "opening your " + f.Value + " account"
	case "holding":
		return "buying " + f.Value
	default:
		return ""
	}
}

// currentScalarFacts returns every current-state scalar self-fact (universal AND
// domain), in timeline (Seq) order. This is the data-driven spine of scalar
// recall / knowledge-update question derivation: a new domain scalar family
// flows through automatically once its facts are planned and it has scalarAsk +
// factLabel entries — DeriveQuestions never hard-codes the attribute list.
func currentScalarFacts(p *Plan) []Fact {
	var out []Fact
	for _, f := range p.Facts {
		if f.Kind == KindScalar && f.Entity == "self" && f.Current {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// listFacts returns all list-attribute facts for attr, in timeline order.
func listFacts(p *Plan, attr string) []Fact {
	var out []Fact
	for _, f := range p.Facts {
		if f.Kind == KindList && f.Attribute == attr {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// listAttributesPresent returns the distinct list attributes present in the
// plan, ordered by first appearance (Seq). Like currentScalarFacts this keeps
// the multi-session synthesis loop data-driven: a domain list family is picked
// up from its listCountAsk/listAllAsk entries with no change to DeriveQuestions.
func listAttributesPresent(p *Plan) []string {
	facts := make([]Fact, 0, len(p.Facts))
	for _, f := range p.Facts {
		if f.Kind == KindList {
			facts = append(facts, f)
		}
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].Seq < facts[j].Seq })
	seen := map[string]bool{}
	var out []string
	for _, f := range facts {
		if !seen[f.Attribute] {
			seen[f.Attribute] = true
			out = append(out, f.Attribute)
		}
	}
	return out
}

func pick3(cond bool, ifTrue, ifFalse string) string {
	if cond {
		return ifTrue
	}
	return ifFalse
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func joinComma(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}
