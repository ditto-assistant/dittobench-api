package persona

import (
	"fmt"
	"math/rand"
	"sort"
)

// FactKind classifies a fact for question derivation.
type FactKind string

const (
	// KindScalar is a single-valued attribute (city, occupation, car). Some
	// scalars carry an update chain (Superseded set on the current fact).
	KindScalar FactKind = "scalar"
	// KindList is one item of a multi-valued attribute (projects, trips, pets) —
	// the material for multi-session "how many / list all" synthesis questions.
	KindList FactKind = "list"
	// KindPreference is a taste the persona holds (cuisine, dietary style, color)
	// — recall plus preference-application questions.
	KindPreference FactKind = "preference"
	// KindOpinion is a reversible stance on a hobby (loved → later can't stand):
	// the raw material for contradiction / change-of-mind questions.
	KindOpinion FactKind = "opinion"
	// KindDistractor is a near-miss decoy about another entity, drawn from the
	// SAME pools as a target fact (same attribute, different entity/value). It is
	// never a correct answer; it exists to pressure retrieval.
	KindDistractor FactKind = "distractor"
)

// BeatKind classifies a session beat.
type BeatKind string

const (
	BeatFact  BeatKind = "fact"  // introduces / updates / reverses a fact
	BeatNoise BeatKind = "noise" // chit-chat, carries no ground truth
)

// Fact is a typed atom of the persona universe: (entity, attribute, value) with
// a creation session and timeline position. Value is the CANONICAL answer token
// — a memory answer is checked for normalized containment of Value verbatim, so
// it must survive surface realization unchanged (verified in Layer 2). Display is
// the natural phrase used when the fact is spoken in a beat.
type Fact struct {
	ID        string
	Kind      FactKind
	Entity    string // "self" for the persona; a decoy name for distractors
	Attribute string // canonical key, e.g. "city", "occupation", "project"
	Value     string // canonical answer token (verbatim-preserved)
	Display   string // natural phrase for the value, e.g. "a tortoise named Biscuit"
	Session   int    // session index that introduces this fact
	Seq       int    // global timeline order (0-based, unique)
	// Supersedes is the ID of the fact this one replaces (an update chain or a
	// reversal); "" for an original assertion. Reversal distinguishes a
	// change-of-mind ("I can't stand X anymore") from a plain value update.
	Supersedes string
	Reversal   bool
	// Current is true for the fact that holds as of the end of the timeline
	// (latest value wins). A superseded fact has Current=false.
	Current bool
	// UserText / AsstText are the Layer-1 TEMPLATE rendering of the beat that
	// introduces this fact — deterministic, always carrying Value verbatim. They
	// are copied into the fact's session Beat and are the fallback surface if LLM
	// realization fails verification.
	UserText string
	AsstText string
}

// Beat is one turn-pair script in a session. A fact beat renders into a
// user/assistant MemoryPair that asserts Fact; a noise beat renders into
// on-topic chit-chat with no recoverable fact.
type Beat struct {
	Kind   BeatKind
	FactID string // set for BeatFact
	Topic  string // set for BeatNoise
	// UserText / AsstText are the Layer-1 TEMPLATE rendering — deterministic,
	// always present, and the fallback if LLM surface realization fails
	// verification. They already carry Fact.Value verbatim.
	UserText string
	AsstText string
}

// Session is an ordered run of beats with a seed-derived day offset (days after
// the persona timeline's start; anchored to the pinned dataset epoch by the
// caller, never the wall clock).
type Session struct {
	Index     int
	DayOffset int
	Beats     []Beat
}

// Plan is the complete Layer-1 ground truth for one seed: the persona name, the
// full fact timeline (including superseded values and decoy distractors), and
// the session scripts that introduce them. It is a pure function of (seed,
// Opts) — see BuildPlan.
type Plan struct {
	Seed     int64
	Name     string
	Facts    []Fact
	Sessions []Session
}

// FactByID returns the fact with the given ID (and whether it was found).
func (p *Plan) FactByID(id string) (Fact, bool) {
	for _, f := range p.Facts {
		if f.ID == id {
			return f, true
		}
	}
	return Fact{}, false
}

// Opts are the deterministic (NON-entropy) size knobs for a plan. They are part
// of the plan's identity alongside the seed: same (seed, Opts) ⇒ identical
// plan. Opts are derived from the run profile; DefaultOpts is used by tests
// and as the medium baseline.
type Opts struct {
	Sessions     int // number of conversation sessions
	Projects     int // list-attribute items (multi-session synthesis material)
	Trips        int
	Pets         int
	UpdateChains int // scalar attributes that receive a value update
	Reversals    int // opinion facts that get reversed (contradiction material)
	DecoyPeople  int // near-miss distractor entities
	DomainItems  int // items drawn for each professional-domain list family
	// LongChain is the length of ONE extended update trajectory (≥3 → an N-state
	// chain for state-tracking questions; <3 → all chains stay 2-state). The long
	// chain is drawn from the same attributes eligible for an update.
	LongChain int
}

// DefaultOpts is a medium-sized, well-populated universe: enough facts for the
// full memory suite's stratified question quotas with headroom.
func DefaultOpts() Opts {
	return Opts{
		Sessions:     7,
		Projects:     5,
		Trips:        4,
		Pets:         3,
		UpdateChains: 3,
		Reversals:    2,
		DecoyPeople:  6,
		DomainItems:  3,
		LongChain:    3,
	}
}

func (o Opts) normalized() Opts {
	if o.Sessions < 2 {
		o.Sessions = 2
	}
	if o.Projects < 0 {
		o.Projects = 0
	}
	if o.Trips < 0 {
		o.Trips = 0
	}
	if o.Pets < 0 {
		o.Pets = 0
	}
	if o.UpdateChains < 0 {
		o.UpdateChains = 0
	}
	if o.Reversals < 0 {
		o.Reversals = 0
	}
	if o.DecoyPeople < 0 {
		o.DecoyPeople = 0
	}
	if o.DomainItems < 0 {
		o.DomainItems = 0
	}
	if o.LongChain < 0 {
		o.LongChain = 0
	}
	return o
}

// scalarSpec describes one single-valued persona attribute and how to speak it.
type scalarSpec struct {
	attr      string
	label     string // human noun for the attribute (recall question + distractors)
	pool      []string
	updatable bool
	// stmt/ack are chosen by seed; %s is the value. update{Stmt,Ack} phrase a
	// LATER change of the same attribute (used for update chains).
	stmt       []string
	ack        []string
	updateStmt []string
	updateAck  []string
}

// scalarSpecs is the ordered scalar-attribute registry. Order is fixed (a slice,
// never a map range) so the plan is reproducible.
var scalarSpecs = []scalarSpec{
	{
		attr: "city", label: "city", pool: cities, updatable: true,
		stmt:       []string{"I just moved to %s.", "We relocated to %s a few weeks ago.", "I've settled into %s now."},
		ack:        []string{"%s is a lovely place — hope the move went smoothly.", "Noted that you live in %s now."},
		updateStmt: []string{"Update: we've moved again, this time to %s.", "I've since relocated to %s."},
		updateAck:  []string{"Got it — updating your city to %s.", "Noted, %s is your home now."},
	},
	{
		attr: "occupation", label: "job", pool: occupations, updatable: true,
		stmt:       []string{"I work as a %s.", "My job is being a %s.", "Professionally I'm a %s."},
		ack:        []string{"A %s — that's a fascinating line of work.", "Noted that you work as a %s."},
		updateStmt: []string{"I've changed careers — I'm a %s now.", "Career update: I retrained as a %s."},
		updateAck:  []string{"Congratulations on the switch to being a %s.", "Updating your job to %s."},
	},
	{
		attr: "employer", label: "employer", pool: companies, updatable: true,
		stmt:       []string{"I work at %s.", "My employer is %s.", "I joined %s recently."},
		ack:        []string{"%s — good to know where you work.", "Noted, you're at %s."},
		updateStmt: []string{"I've moved on to a new job at %s.", "I left and now work at %s."},
		updateAck:  []string{"Noted your new employer, %s.", "Updating your workplace to %s."},
	},
	{
		attr: "car", label: "car", pool: carModels, updatable: true,
		stmt:       []string{"I drive a %s.", "My car is a %s.", "I get around in a %s."},
		ack:        []string{"A %s — solid choice.", "Noted that you drive a %s."},
		updateStmt: []string{"I traded the old car in for a %s.", "I've upgraded to a %s."},
		updateAck:  []string{"Nice upgrade to the %s.", "Updating your car to a %s."},
	},
	{
		attr: "hometown", label: "hometown", pool: cities,
		stmt: []string{"I grew up in %s.", "I'm originally from %s.", "My hometown is %s."},
		ack:  []string{"%s — that must have shaped a lot of memories.", "Noted, you're from %s."},
	},
	{
		attr: "partner", label: "partner's name", pool: firstNames,
		stmt: []string{"My partner's name is %s.", "I live with my partner, %s.", "%s and I have been together for years."},
		ack:  []string{"Lovely — say hi to %s.", "Noted your partner, %s."},
	},
	{
		attr: "instrument", label: "instrument", pool: instruments,
		stmt: []string{"I play the %s.", "I've been learning the %s.", "My instrument is the %s."},
		ack:  []string{"The %s is a beautiful instrument.", "Noted that you play the %s."},
	},
	{
		attr: "alma_mater", label: "university", pool: universities,
		stmt: []string{"I studied at %s.", "I went to university in %s.", "My alma mater is %s."},
		ack:  []string{"%s has a great reputation.", "Noted, you studied at %s."},
	},
}

// listSpec describes a multi-valued attribute.
type listSpec struct {
	attr  string
	pool  []string
	stmt  []string
	ack   []string
	isPet bool // pets pair a type + name for the Display phrase
}

var (
	projectSpec = listSpec{
		attr: "project", pool: projectNames,
		stmt: []string{"I've started working on %s.", "This week I picked up %s.", "I'm making progress on %s."},
		ack:  []string{"%s sounds like a rewarding project.", "Noted your project, %s."},
	}
	tripSpec = listSpec{
		attr: "trip", pool: cities,
		stmt: []string{"I just got back from a trip to %s.", "I traveled to %s last month.", "I spent a week in %s."},
		ack:  []string{"%s must have been a wonderful trip.", "Noted your trip to %s."},
	}
	petSpec = listSpec{
		attr: "pet", pool: petNames, isPet: true,
		stmt: []string{"We adopted %s.", "Our household now includes %s.", "I got %s recently."},
		ack:  []string{"%s sounds adorable.", "Noted your pet, %s."},
	}
)

// prefSpec describes a preference attribute (recall + application).
type prefSpec struct {
	attr string
	pool []string
	stmt []string
	ack  []string
}

var prefSpecs = []prefSpec{
	{
		attr: "favorite_cuisine", pool: cuisines,
		stmt: []string{"My favorite cuisine is %s.", "I could eat %s food every day.", "I'm crazy about %s cooking."},
		ack:  []string{"%s food is a great pick.", "Noted, you love %s cuisine."},
	},
	{
		attr: "dietary", pool: dietaryStyles,
		stmt: []string{"I'm %s, by the way.", "Just so you know, I eat %s.", "I follow a %s diet."},
		ack:  []string{"Good to know you're %s — I'll keep that in mind.", "Noted your %s diet."},
	},
	{
		attr: "favorite_color", pool: colors,
		stmt: []string{"My favorite color is %s.", "I'm drawn to anything %s.", "I love the color %s."},
		ack:  []string{"%s is a wonderful color.", "Noted, your favorite color is %s."},
	},
}

// domainSpec bundles the professional-domain fact families layered onto a
// persona (software / medical / legal). Exactly one domain is chosen per seed
// (deterministically) and its scalars + list are emitted alongside the universal
// personal facts, so every run carries a specialist register (motivated by
// BEIR's cross-domain retrieval collapse and LongMemEval-V2's
// professional reframe). Adding a family here flows through DeriveQuestions with
// no change to its loops — it reads currentScalarFacts / listAttributesPresent
// and looks phrasing up by attribute (scalarAsk / listCountAsk / factLabel).
type domainSpec struct {
	name    string
	scalars []scalarSpec
	lists   []listSpecCount // list families + their per-persona item counts
}

// listSpecCount pairs a list family with how many items to draw for it.
type listSpecCount struct {
	spec  listSpec
	count int
}

// domains is the ordered domain registry (a slice, never a map range, so domain
// choice is reproducible from the seed).
var domains = []domainSpec{
	{
		name: "software",
		scalars: []scalarSpec{
			{
				attr: "primary_language", label: "primary programming language", pool: softwareLanguages, updatable: true,
				stmt:       []string{"My primary language is %s these days.", "I mostly write %s at work.", "I do most of my coding in %s."},
				ack:        []string{"%s is a solid choice for that.", "Noted that you work mainly in %s."},
				updateStmt: []string{"I've switched my main language to %s.", "We migrated the codebase, so I'm writing %s now."},
				updateAck:  []string{"Got it — updating your primary language to %s.", "Noted, %s is your main language now."},
			},
			{
				attr: "code_editor", label: "code editor", pool: softwareEditors, updatable: true,
				stmt:       []string{"My editor of choice is %s.", "I do all my work in %s.", "I've settled on %s as my editor."},
				ack:        []string{"%s — a fine setup.", "Noted that you use %s."},
				updateStmt: []string{"I've switched editors to %s.", "I gave up my old editor and moved to %s."},
				updateAck:  []string{"Noted your new editor, %s.", "Updating your editor to %s."},
			},
		},
		lists: []listSpecCount{{
			spec: listSpec{
				attr: "service", pool: softwareServices,
				stmt: []string{"I maintain the %s service.", "I own the %s service now.", "I picked up the %s service this sprint."},
				ack:  []string{"Noted you maintain %s.", "Got it — %s is one of yours."},
			},
		}},
	},
	{
		name: "medical",
		scalars: []scalarSpec{
			{
				attr: "diagnosis", label: "medical diagnosis", pool: medicalDiagnoses, updatable: true,
				stmt:       []string{"I was diagnosed with %s.", "My doctor says I have %s.", "I'm managing %s."},
				ack:        []string{"Thanks for telling me — I'll remember your %s.", "Noted your diagnosis of %s."},
				updateStmt: []string{"My diagnosis was revised — it's actually %s.", "Update from my doctor: it's now %s, not what we thought."},
				updateAck:  []string{"Understood — updating your diagnosis to %s.", "Noted the change to %s."},
			},
			{
				attr: "medication", label: "medication", pool: medicalMedications, updatable: true,
				stmt:       []string{"I take %s daily.", "My doctor put me on %s.", "I'm currently on %s."},
				ack:        []string{"Noted that you take %s.", "Got it — %s is your current medication."},
				updateStmt: []string{"My doctor switched me from that to %s.", "I've changed medication — I'm on %s now."},
				updateAck:  []string{"Understood — updating your medication to %s.", "Noted the switch to %s."},
			},
		},
		lists: []listSpecCount{{
			spec: listSpec{
				attr: "allergy", pool: medicalAllergies,
				stmt: []string{"I'm allergic to %s.", "I have an allergy to %s.", "I react badly to %s."},
				ack:  []string{"Noted your %s allergy.", "Got it — I'll remember you react to %s."},
			},
		}},
	},
	{
		name: "legal",
		scalars: []scalarSpec{
			{
				attr: "practice_area", label: "area of law", pool: legalPracticeAreas, updatable: true,
				stmt:       []string{"I practice %s.", "My specialty is %s.", "I work in %s."},
				ack:        []string{"Noted that you practice %s.", "Got it — %s is your field."},
				updateStmt: []string{"I've moved my practice into %s.", "I switched specialties — I'm doing %s now."},
				updateAck:  []string{"Noted your move into %s.", "Updating your practice area to %s."},
			},
			{
				attr: "bar_admission", label: "state bar", pool: legalJurisdictions,
				stmt: []string{"I'm admitted to the %s bar.", "I passed the %s bar.", "I'm licensed to practice in %s."},
				ack:  []string{"Noted you're admitted in %s.", "Got it — %s bar."},
			},
		},
		lists: []listSpecCount{{
			spec: listSpec{
				attr: "legal_matter", pool: legalMatters,
				stmt: []string{"I'm handling %s.", "I picked up %s this week.", "I'm lead counsel on %s."},
				ack:  []string{"Noted your matter, %s.", "Got it — %s is on your docket."},
			},
		}},
	},
	{
		name: "finance",
		scalars: []scalarSpec{
			{
				attr: "risk_tolerance", label: "risk tolerance", pool: financeRiskProfiles, updatable: true,
				stmt:       []string{"My risk tolerance is %s.", "I'd describe my investing style as %s.", "I'm a %s investor."},
				ack:        []string{"Noted — a %s approach.", "Got it, a %s risk tolerance."},
				updateStmt: []string{"After the last downturn I've shifted to %s.", "I've rebalanced to a %s stance."},
				updateAck:  []string{"Understood — updating your risk tolerance to %s.", "Noted the shift to %s."},
			},
			{
				attr: "brokerage", label: "brokerage", pool: financeBrokerages,
				stmt: []string{"I hold my accounts at %s.", "My brokerage is %s.", "I invest through %s."},
				ack:  []string{"Noted, your brokerage is %s.", "Got it — %s."},
			},
		},
		lists: []listSpecCount{{
			spec: listSpec{
				attr: "holding", pool: financeHoldings,
				stmt: []string{"I hold %s in my portfolio.", "I added %s to my portfolio.", "I bought some %s."},
				ack:  []string{"Noted your position in %s.", "Got it — %s is in your portfolio."},
			},
		}},
	},
}

// BuildPlan produces the Layer-1 plan for (seed, opts). It is a PURE function:
// the only entropy source is a math/rand stream seeded from seed; there is no
// wall clock, no crypto-rand, and every iteration is over an ordered slice (no
// Go map range), so the same inputs yield a byte-identical plan.
func BuildPlan(seed int64, opts Opts) *Plan {
	opts = opts.normalized()
	r := rand.New(rand.NewSource(seed))

	name := pick(r, firstNames) + " " + pick(r, lastNames)
	p := &Plan{Seed: seed, Name: name}

	// Assign one professional domain (seed-derived) and fold its scalar families
	// into the universal registry, so scalar recall / update-chain / distractor
	// logic below treats domain attributes uniformly. The domain's list families
	// are emitted with the universal list families further down.
	domain := domains[r.Intn(len(domains))]
	scalars := make([]scalarSpec, 0, len(scalarSpecs)+len(domain.scalars))
	scalars = append(scalars, scalarSpecs...)
	scalars = append(scalars, domain.scalars...)

	seq := 0
	nextSeq := func() int { s := seq; seq++; return s }
	// spread assigns an introduction session in [lo,hi).
	spread := func(lo, hi int) int {
		if hi <= lo {
			return lo
		}
		return lo + r.Intn(hi-lo)
	}

	// --- scalar facts (some with update chains) ---
	// Choose which updatable scalars get a value update this run. Domain scalars
	// are in `scalars` too, so a knowledge-update can land on a professional
	// attribute (a re-diagnosis, a language migration) — the dynamic-state case.
	updatableIdx := make([]int, 0, len(scalars))
	for i, s := range scalars {
		if s.updatable {
			updatableIdx = append(updatableIdx, i)
		}
	}
	shuffleInts(r, updatableIdx)
	updates := map[int]bool{}
	for i := 0; i < opts.UpdateChains && i < len(updatableIdx); i++ {
		updates[updatableIdx[i]] = true
	}
	// One updated attribute becomes an N-state trajectory (opts.LongChain states)
	// for state-tracking / trajectory questions; the rest stay 2-state.
	longChainIdx := -1
	if opts.LongChain >= 3 && opts.UpdateChains >= 1 && len(updatableIdx) > 0 {
		longChainIdx = updatableIdx[0]
	}

	half := opts.Sessions / 2
	if half < 1 {
		half = 1
	}
	for i, s := range scalars {
		// N-state trajectory: opts.LongChain distinct values over increasing
		// sessions, each superseding the previous, only the last current. The
		// material for previous-value / ordered-history / state-at-event questions.
		if i == longChainIdx {
			k := opts.LongChain
			vals := pickN(r, s.pool, k)
			if len(vals) < 3 { // pool too small for a real trajectory → fall through
				longChainIdx = -1
			} else {
				k = len(vals)
				base := spread(0, maxInt(1, opts.Sessions-k+1))
				var prevID string
				for j, v := range vals {
					id := "f-" + s.attr
					if j > 0 {
						id = fmt.Sprintf("f-%s-%d", s.attr, j+1)
					}
					f := Fact{
						ID:         id,
						Kind:       KindScalar,
						Entity:     "self",
						Attribute:  s.attr,
						Value:      v,
						Display:    v,
						Session:    minInt(base+j, opts.Sessions-1),
						Seq:        nextSeq(),
						Supersedes: prevID,
						Current:    j == len(vals)-1,
					}
					if j == 0 {
						f.UserText, f.AsstText = fill(pickStr(r, s.stmt), v), fill(pickStr(r, s.ack), v)
					} else {
						f.UserText, f.AsstText = fill(pickStr(r, s.updateStmt), v), fill(pickStr(r, s.updateAck), v)
					}
					p.Facts = append(p.Facts, f)
					prevID = id
				}
				continue
			}
		}
		v1 := pick(r, s.pool)
		stmt := pickStr(r, s.stmt)
		ack := pickStr(r, s.ack)
		introSession := spread(0, half) // originals land early
		f1 := Fact{
			ID:        "f-" + s.attr,
			Kind:      KindScalar,
			Entity:    "self",
			Attribute: s.attr,
			Value:     v1,
			Display:   v1,
			Session:   introSession,
			Seq:       nextSeq(),
			Current:   true,
		}
		if updates[i] {
			v2 := pickDistinct(r, s.pool, v1)
			f1.Current = false
			f2 := Fact{
				ID:         "f-" + s.attr + "-2",
				Kind:       KindScalar,
				Entity:     "self",
				Attribute:  s.attr,
				Value:      v2,
				Display:    v2,
				Session:    spread(half, opts.Sessions), // update lands later
				Seq:        nextSeq(),
				Supersedes: f1.ID,
				Current:    true,
			}
			f1.UserText, f1.AsstText = fill(stmt, v1), fill(ack, v1)
			f2.UserText, f2.AsstText = fill(pickStr(r, s.updateStmt), v2), fill(pickStr(r, s.updateAck), v2)
			p.Facts = append(p.Facts, f1, f2)
		} else {
			f1.UserText, f1.AsstText = fill(stmt, v1), fill(ack, v1)
			p.Facts = append(p.Facts, f1)
		}
	}

	// --- list facts (projects, trips, pets) ---
	appendList := func(spec listSpec, count int) {
		vals := pickN(r, spec.pool, count)
		for j, v := range vals {
			display, value := v, v
			userText := fill(pickStr(r, spec.stmt), display)
			if spec.isPet {
				t := pick(r, petTypes)
				display = fmt.Sprintf("a %s named %s", t, v)
				value = v // the pet's name is the canonical answer token
				userText = fill(pickStr(r, spec.stmt), display)
			}
			p.Facts = append(p.Facts, Fact{
				ID:        fmt.Sprintf("f-%s-%d", spec.attr, j),
				Kind:      KindList,
				Entity:    "self",
				Attribute: spec.attr,
				Value:     value,
				Display:   display,
				Session:   spread(0, opts.Sessions),
				Seq:       nextSeq(),
				Current:   true,
				UserText:  userText,
				AsstText:  fill(pickStr(r, spec.ack), value),
			})
		}
	}
	appendList(projectSpec, opts.Projects)
	appendList(tripSpec, opts.Trips)
	appendList(petSpec, opts.Pets)
	// domain list families (services / allergies / legal matters).
	for _, lc := range domain.lists {
		appendList(lc.spec, opts.DomainItems)
	}

	// --- preference facts ---
	for _, s := range prefSpecs {
		v := pick(r, s.pool)
		p.Facts = append(p.Facts, Fact{
			ID:        "f-" + s.attr,
			Kind:      KindPreference,
			Entity:    "self",
			Attribute: s.attr,
			Value:     v,
			Display:   v,
			Session:   spread(0, opts.Sessions),
			Seq:       nextSeq(),
			Current:   true,
			UserText:  fill(pickStr(r, s.stmt), v),
			AsstText:  fill(pickStr(r, s.ack), v),
		})
	}

	// --- opinion facts with reversals (contradiction material) ---
	revHobbies := pickN(r, hobbies, opts.Reversals+1)
	for j := 0; j < opts.Reversals && j < len(revHobbies); j++ {
		h := revHobbies[j]
		orig := Fact{
			ID:        fmt.Sprintf("f-opinion-%d", j),
			Kind:      KindOpinion,
			Entity:    "self",
			Attribute: "hobby_opinion",
			Value:     h,
			Display:   h,
			Session:   spread(0, half),
			Seq:       nextSeq(),
			Current:   false, // the reversal supersedes the original stance
			UserText:  fill(pickStr(r, []string{"I absolutely love %s.", "I've really gotten into %s lately.", "%s is my favorite way to spend a weekend."}), h),
			AsstText:  fill(pickStr(r, []string{"%s sounds like a joy.", "Great that you enjoy %s."}), h),
		}
		rev := Fact{
			ID:         fmt.Sprintf("f-opinion-%d-rev", j),
			Kind:       KindOpinion,
			Entity:     "self",
			Attribute:  "hobby_opinion",
			Value:      h,
			Display:    h,
			Session:    spread(half, opts.Sessions),
			Seq:        nextSeq(),
			Supersedes: orig.ID,
			Reversal:   true,
			Current:    true,
			UserText:   fill(pickStr(r, []string{"Honestly, I can't stand %s anymore — I've given it up.", "I've completely gone off %s; I don't do it now.", "I used to love %s but I've quit it entirely."}), h),
			AsstText:   fill(pickStr(r, []string{"Understood — you no longer do %s.", "Noted that you've given up %s."}), h),
		}
		p.Facts = append(p.Facts, orig, rev)
	}

	// --- near-miss distractors: same attributes, different (decoy) entities ---
	relations := []string{"my colleague", "my neighbor", "my cousin", "my old roommate", "a friend", "my sister"}
	for d := 0; d < opts.DecoyPeople; d++ {
		who := pick(r, firstNames)
		rel := relations[d%len(relations)]
		// Each decoy asserts one same-attribute-different-value fact against a
		// random scalar spec (universal OR domain), so the haystack holds a
		// plausible near-miss for that attribute's recall question — including the
		// specialist attributes, where jargon most pressures retrieval.
		s := scalars[r.Intn(len(scalars))]
		v := pick(r, s.pool)
		p.Facts = append(p.Facts, Fact{
			ID:        fmt.Sprintf("f-distractor-%d", d),
			Kind:      KindDistractor,
			Entity:    who,
			Attribute: s.attr,
			Value:     v,
			Display:   v,
			Session:   spread(0, opts.Sessions),
			Seq:       nextSeq(),
			UserText:  fmt.Sprintf("By the way, %s %s's %s is %s.", rel, who, s.label, v),
			AsstText:  fmt.Sprintf("Noted — that %s is about %s, not you.", s.label, who),
		})
	}

	// --- assign facts to session scripts (ordered), interleaved with noise ---
	p.Sessions = buildSessions(r, p.Facts, opts.Sessions)
	return p
}

// buildSessions groups facts into their assigned sessions (fact order within a
// session follows Seq), interleaves a seed-chosen noise beat or two, and assigns
// each session a strictly increasing day offset.
func buildSessions(r *rand.Rand, facts []Fact, nSessions int) []Session {
	bySession := make([][]Fact, nSessions)
	for _, f := range facts {
		s := f.Session
		if s < 0 {
			s = 0
		}
		if s >= nSessions {
			s = nSessions - 1
		}
		bySession[s] = append(bySession[s], f)
	}

	sessions := make([]Session, 0, nSessions)
	day := r.Intn(20) // small seed-derived start offset
	for i := 0; i < nSessions; i++ {
		facts := bySession[i]
		sort.Slice(facts, func(a, b int) bool { return facts[a].Seq < facts[b].Seq })
		beats := make([]Beat, 0, len(facts)+2)
		// A leading noise beat on most sessions so not every turn is a fact.
		if r.Intn(3) != 0 {
			beats = append(beats, noiseBeat(r))
		}
		for _, f := range facts {
			beats = append(beats, Beat{Kind: BeatFact, FactID: f.ID, UserText: f.UserText, AsstText: f.AsstText})
			if r.Intn(4) == 0 { // occasional interstitial chit-chat
				beats = append(beats, noiseBeat(r))
			}
		}
		if len(beats) == 0 { // never emit an empty session
			beats = append(beats, noiseBeat(r))
		}
		sessions = append(sessions, Session{Index: i, DayOffset: day, Beats: beats})
		day += 1 + r.Intn(14) // strictly increasing gaps (1..14 days)
	}
	return sessions
}

func noiseBeat(r *rand.Rand) Beat {
	t := pick(r, noiseTopics)
	tmpl := noiseTemplates[r.Intn(len(noiseTemplates))]
	return Beat{
		Kind:     BeatNoise,
		Topic:    t,
		UserText: capitalize(fill(tmpl.user, t)),
		AsstText: fill(tmpl.asst, t),
	}
}
