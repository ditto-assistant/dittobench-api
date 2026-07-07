package persona

import (
	"fmt"
	"math/rand"
	"sort"
)

// FactKind classifies a fact for question derivation (WP B3).
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
	// never a correct answer; it exists to pressure retrieval (§4.1).
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
// it must survive surface realization unchanged (verified in WP B2). Display is
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
	// realization (WP B2) fails verification.
	UserText string
	AsstText string
}

// Beat is one turn-pair script in a session. A fact beat renders (WP B2) into a
// user/assistant MemoryPair that asserts Fact; a noise beat renders into
// on-topic chit-chat with no recoverable fact.
type Beat struct {
	Kind   BeatKind
	FactID string // set for BeatFact
	Topic  string // set for BeatNoise
	// UserText / AsstText are the Layer-1 TEMPLATE rendering — deterministic,
	// always present, and the fallback if LLM surface realization (B2) fails
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
// plan. WP B3 derives Opts from the run profile; DefaultOpts is used by tests
// and as the medium baseline.
type Opts struct {
	Sessions     int // number of conversation sessions
	Projects     int // list-attribute items (multi-session synthesis material)
	Trips        int
	Pets         int
	UpdateChains int // scalar attributes that receive a value update
	Reversals    int // opinion facts that get reversed (contradiction material)
	DecoyPeople  int // near-miss distractor entities
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

// BuildPlan produces the Layer-1 plan for (seed, opts). It is a PURE function:
// the only entropy source is a math/rand stream seeded from seed; there is no
// wall clock, no crypto-rand, and every iteration is over an ordered slice (no
// Go map range), so the same inputs yield a byte-identical plan (§4.2, §11.3).
func BuildPlan(seed int64, opts Opts) *Plan {
	opts = opts.normalized()
	r := rand.New(rand.NewSource(seed))

	name := pick(r, firstNames) + " " + pick(r, lastNames)
	p := &Plan{Seed: seed, Name: name}

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
	// Choose which updatable scalars get a value update this run.
	updatableIdx := make([]int, 0, len(scalarSpecs))
	for i, s := range scalarSpecs {
		if s.updatable {
			updatableIdx = append(updatableIdx, i)
		}
	}
	shuffleInts(r, updatableIdx)
	updates := map[int]bool{}
	for i := 0; i < opts.UpdateChains && i < len(updatableIdx); i++ {
		updates[updatableIdx[i]] = true
	}

	half := opts.Sessions / 2
	if half < 1 {
		half = 1
	}
	for i, s := range scalarSpecs {
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
		// random scalar spec, so the haystack holds a plausible near-miss for
		// that attribute's recall question.
		s := scalarSpecs[r.Intn(len(scalarSpecs))]
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
	return Beat{
		Kind:     BeatNoise,
		Topic:    t,
		UserText: capitalize("anyway, " + t + " has been on my mind."),
		AsstText: "Thanks for sharing — happy to chat about " + t + ".",
	}
}
