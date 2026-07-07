// Package persona is the DittoBench v2 plan layer (BENCHMARK-V2.md §4.1). It
// generates a fresh, procedural "persona universe" — a fact graph plus the
// session scripts that introduce those facts — as a PURE function of a single
// int64 seed. No wall clock, no crypto-rand, no map-iteration order: same seed
// ⇒ byte-identical plan (the reproducibility contract, §4.2 / §11.3).
//
// The plan is Layer 1 of the two-layer engine: the deterministic ground truth.
// Layer 2 (surface realization, WP B2) renders each beat into a natural
// user/assistant pair with an LLM, verified against the plan's canonical
// values; question derivation and difficulty tiers (WP B3) read the plan to
// build the memory suite. This package intentionally has NO LLM dependency —
// it is the thing everything else is checked against.
//
// # Combinatorics (≥10⁹ distinct universes)
//
// The persona skeleton draws a set of INDEPENDENT scalar attributes, each from
// one of the pools below. A conservative lower bound on the number of distinct
// skeletons, counting only nine always-present scalar attributes:
//
//	firstNames × lastNames × cities(residence) × occupations × companies ×
//	carModels × cities(hometown) × firstNames(partner) × universities
//	= 48 × 40 × 40 × 40 × 32 × 28 × 40 × 48 × 28
//	≈ 3.0 × 10¹⁵  distinct skeletons.
//
// That is six orders of magnitude past the §B1 ≥10⁹ target BEFORE counting the
// list attributes (projects/trips/pets, each a multi-draw subset), the
// preference and opinion facts, the update/reversal choices, the near-miss
// distractor draws, or the seed-derived timeline — every one of which
// multiplies the space further. There is nothing to memorize.
package persona

// The pools are the combinatorial vocabulary of the persona universe. They are
// deliberately large and disjoint enough that same-attribute/different-value
// near-miss distractors (§4.1) are easy to draw. Values are clean canonical
// tokens: a memory answer is checked by normalized containment against the
// value verbatim (scorer.deterministicMemoryHit), so each entry is a single,
// unambiguous phrase with no trailing punctuation.

var firstNames = []string{
	"Alice", "Marcus", "Priya", "Diego", "Yuki", "Fatima", "Sven", "Lena",
	"Omar", "Chloe", "Ravi", "Ingrid", "Tariq", "Nadia", "Kenji", "Rosa",
	"Bjorn", "Amara", "Luca", "Mei", "Hassan", "Freya", "Pablo", "Anika",
	"Dmitri", "Zoe", "Kofi", "Sofia", "Isaac", "Lin", "Mateo", "Hana",
	"Elias", "Noor", "Theo", "Camila", "Aran", "Yara", "Nikolai", "Esme",
	"Rafael", "Iris", "Malik", "Greta", "Sanjay", "Wren", "Otto", "Delphine",
}

var lastNames = []string{
	"Okafor", "Petrov", "Nakamura", "Silva", "Haugen", "Bianchi", "Kowalski",
	"Reyes", "Andersson", "Farouk", "Chen", "Dubois", "Mbeki", "Larsen",
	"Costa", "Nguyen", "Sharma", "Volkov", "Ferrari", "Hassan", "Bergström",
	"Kim", "Rossi", "Novak", "Adeyemi", "Ito", "Moreau", "Sato", "Vargas",
	"Lindqvist", "Diallo", "Marchetti", "Yilmaz", "Sorensen", "Kaur", "Dupont",
	"Oduya", "Fischer", "Rahman", "Castellano",
}

// cities is used both for residence and (independently) for hometown and trips,
// so it is generous to keep those draws distinct.
var cities = []string{
	"Lisbon", "Osaka", "Nairobi", "Reykjavik", "Medellin", "Tallinn", "Kyoto",
	"Porto", "Bergen", "Marrakesh", "Ljubljana", "Chiang Mai", "Valparaiso",
	"Gdansk", "Hobart", "Cusco", "Ghent", "Kanazawa", "Trondheim", "Oaxaca",
	"Riga", "Ljubljana Hills", "Antwerp", "Sapporo", "Bilbao", "Split",
	"Galway", "Utrecht", "Wroclaw", "Queenstown", "Fez", "Nara", "Aarhus",
	"Bratislava", "Ponta Delgada", "Kotor", "Leuven", "Matera", "Lucerne",
	"Tromso",
}

var occupations = []string{
	"marine biologist", "typographer", "wind-turbine technician", "sommelier",
	"cartographer", "beekeeper", "prosthetist", "arborist", "glassblower",
	"epidemiologist", "luthier", "hydrologist", "millwright", "ceramicist",
	"actuary", "ornithologist", "stonemason", "perfumer", "cryptographer",
	"paleontologist", "falconer", "geodesist", "cheesemaker", "seismologist",
	"puppeteer", "horologist", "cetologist", "vexillologist", "conservator",
	"agronomist", "diplomat", "audiologist", "cooper", "toxicologist",
	"lexicographer", "volcanologist", "farrier", "philatelist", "botanist",
	"radiographer",
}

var companies = []string{
	"Northwind Cartography", "Solvent Labs", "Meridian Optics", "Basalt Works",
	"Halcyon Freight", "Umbra Analytics", "Tidewater Instruments", "Kestrel Foods",
	"Verdant Grid", "Lodestar Robotics", "Cobalt & Reed", "Aster Biomes",
	"Pennant Aerospace", "Quillfeather Press", "Riverstone Ceramics",
	"Cindergate Steel", "Marrow & Vine", "Palisade Data", "Thornfield Optics",
	"Sablewood Timber", "Ochre Pigments", "Fathom Sonar", "Gantry Rail",
	"Wexler Instruments", "Selkie Marine", "Ambergris Perfumes", "Drift Textiles",
	"Kilnhouse Glass", "Ledger & Loom", "Foxglove Botanicals", "Talisker Freight",
	"Bramble Networks",
}

var carModels = []string{
	"Volvo XC40", "Subaru Outback", "Toyota Hilux", "Peugeot 308",
	"Skoda Octavia", "Mazda MX-5", "Renault Zoe", "Fiat Panda",
	"Kia Sportage", "Citroen C3", "Honda Jazz", "Dacia Duster",
	"Nissan Leaf", "Seat Ibiza", "Opel Corsa", "Ford Ranger",
	"Hyundai Kona", "Suzuki Jimny", "Mini Countryman", "Vauxhall Astra",
	"Polestar 2", "Cupra Formentor", "Alfa Romeo Tonale", "Genesis GV60",
	"BYD Atto 3", "MG ZS", "Jeep Avenger", "Ora Funky Cat",
}

var universities = []string{
	"Uppsala", "Otago", "Coimbra", "Leiden", "Trinity Dublin", "Padua",
	"Salamanca", "Aarhus", "Bologna", "Ghent", "Tartu", "Lund",
	"Groningen", "Heidelberg", "Basel", "Ljubljana", "Bergen", "Turku",
	"Cordoba", "Valencia", "Reykjavik", "Vilnius", "Wroclaw", "Debrecen",
	"Maribor", "Zagreb", "Tampere", "Umea",
}

var instruments = []string{
	"cello", "bouzouki", "hammered dulcimer", "bandoneon", "kora",
	"theremin", "nyckelharpa", "shakuhachi", "hurdy-gurdy", "oud",
	"marimba", "concertina", "bassoon", "sitar", "harpsichord", "kalimba",
}

// projectNames feeds the multi-session "list all / how many" synthesis
// questions: several are drawn for one persona.
var projectNames = []string{
	"the tide-clock", "the seed-library catalog", "the rooftop apiary",
	"the ferry-schedule app", "the lichen survey", "the model lighthouse",
	"the community darkroom", "the salt-marsh census", "the pinhole camera",
	"the wind-chime array", "the sourdough archive", "the star-map quilt",
	"the river cleanup map", "the typeface revival", "the moss terrarium",
	"the shortwave logbook", "the tidal-pool guide", "the paper-boat regatta",
	"the birdsong index", "the cyanotype series",
}

var petTypes = []string{
	"tortoise", "greyhound", "cockatiel", "corn snake", "ferret",
	"lop rabbit", "axolotl", "border collie", "maine coon", "budgerigar",
	"chinchilla", "bearded dragon", "hedgehog", "cocker spaniel",
}

var petNames = []string{
	"Biscuit", "Marmalade", "Pixel", "Waffles", "Comet", "Nimbus", "Pepper",
	"Juniper", "Tofu", "Clementine", "Bramble", "Sprocket", "Olive", "Miso",
	"Pumpkin", "Dill", "Cinder", "Poppy", "Basil", "Freckle", "Domino",
	"Marlow", "Saffron", "Widget",
}

var cuisines = []string{
	"Ethiopian", "Georgian", "Peranakan", "Basque", "Oaxacan", "Levantine",
	"Sichuan", "Sardinian", "Goan", "Yucatecan", "Uyghur", "Cape Malay",
	"Piedmontese", "Hokkaido", "Andalusian", "Tamil",
}

var dietaryStyles = []string{
	"vegetarian", "pescatarian", "gluten-free", "dairy-free", "vegan",
	"nut-free", "low-sodium", "shellfish-free",
}

var colors = []string{
	"teal", "ochre", "burgundy", "sage", "cobalt", "amber", "plum",
	"terracotta", "chartreuse", "indigo", "coral", "slate", "mustard",
	"lavender",
}

// hobbies feeds the reversible-opinion (contradiction) facts: the persona once
// loved a hobby, then changed their mind.
var hobbies = []string{
	"bouldering", "urban sketching", "sea kayaking", "letterpress printing",
	"foraging", "orienteering", "fly fishing", "contact juggling",
	"astrophotography", "bookbinding", "freediving", "geocaching",
	"wheel throwing", "trail running", "birdwatching", "whittling",
}

// noiseTopics are chit-chat beats (no fact) sprinkled into sessions so recall
// is not trivially "every turn is a fact". They carry no ground truth.
var noiseTopics = []string{
	"the weather this week", "a movie they watched", "weekend plans",
	"a book recommendation", "traffic on the commute", "a new coffee place",
	"a podcast episode", "the football scores", "a house plant that is wilting",
	"a recipe that went wrong",
}
