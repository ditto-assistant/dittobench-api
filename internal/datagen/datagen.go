// Package datagen procedurally generates small, fresh, randomized DittoBench
// tool-calling datasets. Generation is deterministic per seed (so a given seed
// always yields the same dataset) but varies widely across seeds. The practice
// API rotates the seed on every request so no two evaluations are identical —
// this is the anti-overfit property of the off-chain practice loop.
package datagen

import (
	"fmt"
	"math/rand"
	"strings"

	"github.com/ditto-assistant/dittobench-api/pkg/toolexec"
	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// category describes one kind of tool-calling case and how to render it.
type category struct {
	name string
	tool string // single expected tool; empty means "no tool" (unless tools is set)
	// tools, when non-empty, is a multi-hop expected sequence (overrides tool);
	// MaxToolCalls becomes len(tools) and order is scored.
	tools []string
	// unordered marks a tools set as INDEPENDENT parallel calls: name/arg set is
	// scored but relative call order is not (ToolCase.Unordered).
	unordered bool
	// argKey, when set on a single-tool category whose filler is an exact token
	// (a URL, a theme), pins RequiredArgs[argKey]=filler so the argument value is
	// deterministically scored — right tool + wrong arg no longer gets full credit.
	argKey    string
	templates []string
	// intents are prompts that imply the arg VALUE (or name a near-miss) instead of
	// stating it verbatim, so a harness must parse intent rather than copy a token.
	// Used for a share of a set_* category's cases; requires argKey.
	intents []argIntent
}

// argIntent maps an intent-phrased prompt to the arg value it should resolve to.
// Prompt either implies Value ("think harder" → "high") or names a near-miss
// alongside it ("switch me off gpt-5 to gemini-3-pro" → "gemini-3-pro"), so a
// token-copying harness gets it wrong.
type argIntent struct {
	prompt string
	value  string
}

// word pools used to vary entities/phrasings across seeds.
var (
	subjects = []string{
		"my dentist appointment", "the project deadline", "Sarah's birthday",
		"my car insurance", "the meeting notes", "my flight to Tokyo",
		"the grocery list", "my gym schedule", "the wifi password",
		"my passport number", "the book recommendation", "my doctor's name",
	}
	people = []string{
		"Alice", "Bob", "Carol", "David", "Erin", "Frank", "Grace", "Heidi",
	}
	topics = []string{
		"quantum computing", "the 2024 Olympics", "best espresso machines",
		"rust vs go", "climate policy", "the stock market today",
		"sourdough recipes", "electric vehicles", "the James Webb telescope",
	}
	urls = []string{
		"https://example.com/article", "https://news.site/story",
		"https://blog.dev/post", "https://docs.io/guide",
		"https://github.com/org/repo",
	}
	imagePrompts = []string{
		"a sunset over mountains", "a robot drinking coffee",
		"a futuristic city skyline", "a watercolor fox",
		"an astronaut on a beach", "a neon cyberpunk street",
	}
	artifactKinds = []string{
		"a landing page", "a todo app", "a snake game",
		"a markdown resume", "a pomodoro timer", "a budget tracker",
	}
	agentTasks = []string{
		"scrape the latest headlines", "summarize this PDF",
		"refactor the auth module", "generate unit tests",
		"build a CSV report", "deploy the staging branch",
	}
	themes = []string{"dark", "light", "system", "midnight", "solarized"}

	memoryIDs = []string{"mem-1042", "mem-2087", "mem-3310", "mem-7761", "mem-9024"}
	workflows = []string{"weekly-report", "inbox-triage", "release-notes", "standup-summary", "lead-enrichment"}
	models    = []string{"gpt-5", "claude-sonnet-5", "gemini-3-pro", "llama-4-70b"}
	efforts   = []string{"low", "medium", "high"}
	toolPrefs = []string{"disable web search", "enable image tools", "turn off agent jobs", "allow only memory tools"}
	feedback  = []string{
		"the image tool keeps timing out", "love the new memory search",
		"the export button is broken on mobile", "please add dark mode to artifacts",
	}

	chitchat = []string{
		"hey, how's it going?", "thanks, that was helpful!",
		"tell me a joke", "what's your favorite color?",
		"good morning!", "you're awesome", "lol nice",
	}
	abstentions = []string{
		"what's the meaning of life?", "should I quit my job?",
		"do you love me?", "what will the weather be like next year?",
		"who will win the next election?", "what am I thinking right now?",
	}
)

// categories enumerates every case type the generator can emit. Order is
// stable so seeding is reproducible.
var categories = []category{
	{
		name: "memory_lookup", tool: "search_memories",
		templates: []string{
			"What did I say about %s?",
			"Remind me about %s.",
			"Do you remember %s?",
			"Look up %s from my memories.",
		},
	},
	{
		name: "memory_subject", tool: "search_subjects",
		templates: []string{
			"What subjects do I have notes on related to %s?",
			"Find the topic that covers %s.",
			"Which of my subjects mention %s?",
		},
	},
	{
		name: "web_search", tool: "search_web",
		templates: []string{
			"Search the web for %s.",
			"What's the latest on %s?",
			"Find recent news about %s.",
			"Google %s for me.",
		},
	},
	{
		name: "link_read", tool: "read_links", argKey: "url",
		templates: []string{
			"Read %s and summarize it.",
			"What does this page say: %s",
			"Open %s and tell me the main points.",
		},
	},
	{
		name: "image_create", tool: "create_image",
		templates: []string{
			"Generate an image of %s.",
			"Create a picture of %s.",
			"Draw me %s.",
		},
	},
	{
		name: "artifacts_create", tool: "artifacts",
		templates: []string{
			"Build me %s.",
			"Make %s I can preview.",
			"Create %s as an interactive artifact.",
		},
	},
	{
		name: "agent_job", tool: "execute_agent_job", argKey: "task",
		templates: []string{
			"Run a background job to %s.",
			"Kick off an agent to %s.",
			"Dispatch a task to %s.",
		},
	},
	{
		name: "settings", tool: "set_theme", argKey: "theme",
		templates: []string{
			"Switch to %s mode.",
			"Set my theme to %s.",
			"Change the app theme to %s.",
		},
		intents: []argIntent{
			{"I had it on light, but switch me to midnight instead.", "midnight"},
			{"Don't leave it on dark — set it to solarized.", "solarized"},
			{"Change it from system to dark.", "dark"},
			{"Not light — I want the solarized look.", "solarized"},
		},
	},
	{
		// Hard routing trap: phrased like a web search but the right action is a
		// memory lookup (the user is asking about THEIR past, not the public web).
		name: "route_memory_not_web", tool: "search_memories",
		templates: []string{
			"Search for what I told you about %s.",
			"Look up %s — I mentioned it before.",
			"Find that thing I saved about %s.",
		},
	},
	{
		// Hard routing trap: looks like a memory query but needs the live web
		// (current/real-time info the user can't have stored).
		name: "route_web_not_memory", tool: "search_web",
		templates: []string{
			"Remind me what the latest news on %s is.",
			"Do you recall the current price of %s?",
			"What's the up-to-date status of %s right now?",
		},
	},
	{
		// Run-vs-read trap: dispatch a NEW background job (not check existing).
		name: "agent_run_not_read", tool: "execute_agent_job",
		templates: []string{
			"Go ahead and %s for me now.",
			"Please actually %s, don't just tell me how.",
			"Start working on this: %s.",
		},
	},
	{
		// Run-vs-read trap: check status of EXISTING jobs (not start a new one).
		name: "agent_read_not_run", tool: "list_agent_jobs",
		templates: []string{
			"What background jobs do I have running?",
			"Show me my recent agent jobs.",
			"Did any of my dispatched tasks finish yet?",
		},
	},
	{
		// Edit-vs-create trap: the user references an EXISTING image to modify, so
		// the right tool is edit_image, not create_image.
		name: "image_edit_not_create", tool: "edit_image",
		templates: []string{
			"Take the last image and make the sky purple.",
			"Edit that picture you made to add a small hat.",
			"Change the background of the image to a quiet beach.",
		},
	},
	{
		// Job-vs-workflow trap: phrased like a one-off background job, but it names a
		// predefined WORKFLOW, so the right tool is execute_agent_workflow.
		name: "workflow_not_job", tool: "execute_agent_workflow", argKey: "workflow",
		templates: []string{
			"Kick off a background job for my %s routine.",
			"Dispatch an agent to run my %s process.",
			"Start a one-off task to do my usual %s.",
		},
	},
	{
		name: "no_tool", tool: "", // chit-chat, answer directly
		templates: []string{
			"%s",
		},
	},
	{
		name: "abstention", tool: "", // should not call tools, answer/abstain
		templates: []string{
			"%s",
		},
	},
	{
		// Hallucinated-argument trap (BFCL): the request names an action that needs a
		// specific argument but never supplies its VALUE, and no prior context holds
		// it. The correct behavior is to ask for the missing detail, NOT to invoke the
		// tool with a fabricated argument — so, like abstention, calling any tool
		// scores 0 (a guessed value is the failure mode being probed).
		name: "arg_hallucination", tool: "", // required value missing — ask, don't invent
		templates: []string{
			"Change my theme.",
			"Switch my main model, please.",
			"Set my reasoning effort.",
			"Read that link and summarize it.",
			"File that as feedback for the team.",
			"Run my workflow.",
		},
	},
	// Full-catalog coverage: single-hop categories so every remaining
	// catalog tool is the correct answer for some case (10/18 were dead in v1).
	{
		name: "memory_fetch", tool: "fetch_memories", argKey: "ids",
		templates: []string{
			"Fetch the full text of memory %s.",
			"Pull up memory %s in full.",
			"Open memory %s and show me everything in it.",
		},
	},
	{
		name: "agent_workflow", tool: "execute_agent_workflow", argKey: "workflow",
		templates: []string{
			"Run the %s workflow.",
			"Kick off my %s workflow.",
			"Execute the %s workflow now.",
		},
	},
	{
		name: "feedback", tool: "file_feedback_for_team", argKey: "message",
		templates: []string{
			"Send this to the Ditto team: %s.",
			"File feedback for the team — %s.",
			"Report to the devs that %s.",
		},
	},
	{
		name: "set_model", tool: "set_main_model", argKey: "model",
		templates: []string{
			"Switch my chat model to %s.",
			"Use %s as my main model.",
			"Change my primary model to %s.",
		},
		intents: []argIntent{
			{"I'm on gemini-3-pro now — move me over to claude-sonnet-5.", "claude-sonnet-5"},
			{"Switch me off gpt-5 and onto gemini-3-pro.", "gemini-3-pro"},
			{"My main is llama-4-70b; change it to gpt-5.", "gpt-5"},
			{"Take me off claude-sonnet-5 and put me on llama-4-70b.", "llama-4-70b"},
		},
	},
	{
		intents: []argIntent{
			{"Really take your time and reason deeply on my questions from now on.", "high"},
			{"Make your answers as thorough and careful as you can.", "high"},
			{"Keep it quick and don't overthink my requests.", "low"},
			{"Snappy, minimal reasoning is fine — don't burn cycles.", "low"},
			{"Use a balanced amount of reasoning, nothing extreme.", "medium"},
		},
		name: "set_effort", tool: "set_reasoning_effort", argKey: "effort",
		templates: []string{
			"Set reasoning effort to %s.",
			"Make responses use %s effort.",
			"Change the thinking level to %s.",
		},
	},
	{
		name: "set_tool_prefs", tool: "set_chat_tool_preferences", argKey: "preferences",
		templates: []string{
			"Update my tool preferences: %s.",
			"Change my chat tools so you %s.",
			"Adjust which tools you use — %s.",
		},
	},
	// Multi-hop trajectories: the correct answer is a tool SEQUENCE, scored
	// with order credit. These exercise the previously-dormant multi-call path.
	{
		name: "multi_web_read", tools: []string{"search_web", "read_links"},
		templates: []string{
			"Look up %s online and open the top result.",
			"Find a page about %s and read it for me.",
			"Search for %s and summarize the first source.",
		},
	},
	{
		// Parallel independent calls (BFCL "parallel"): two unrelated actions in one
		// turn, neither's input depending on the other's output — so any call order is
		// correct. Both are about the same entity so one filler renders both.
		name: "parallel_web_image", tools: []string{"search_web", "create_image"}, unordered: true,
		templates: []string{
			"Search the web for %s and also generate an image of it.",
			"Look up %s online, and separately make me a picture of it.",
			"Two things: find the latest on %s and create an image of it.",
		},
	},
	{
		name: "multi_subject_scope", tools: []string{"search_subjects", "search_memories_in_subjects"},
		templates: []string{
			"Find my notes related to %s and pull the details.",
			"Which subject covers %s — then recall the specifics.",
			"Look through my topics on %s and fetch what I saved.",
		},
	},
	{
		name: "multi_job_status", tools: []string{"execute_agent_job", "get_agent_job_status"},
		templates: []string{
			"Kick off a job to %s and tell me its status.",
			"Start %s in the background, then check how it's going.",
			"Run %s and report the job status.",
		},
	},
	{
		name: "multi_image_edit", tools: []string{"create_image", "edit_image"},
		templates: []string{
			"Make an image of %s, then brighten it.",
			"Generate %s and then add more detail.",
			"Create a picture of %s and tweak the colors.",
		},
	},
	// Result-usage (Phase C, capability 13): the answer requires a value
	// that exists ONLY in the tool's returned content — a fabricated per-seed
	// needle (toolexec) — so the case cannot be answered by self-report or base-
	// model knowledge; the harness must actually execute the tool and USE the
	// result. The %s is the needle's Subject (filled below), keeping the question
	// and the served fact coherent. Scored deterministically (trajectory + needle-
	// in-answer), no LLM quality judge.
	{
		name: "web_result_usage", tool: "search_web",
		templates: []string{
			"Search the web for the latest figure on %s and tell me the exact number.",
			"What number does the current top result report for %s?",
			"Look up %s online and give me the precise figure it cites.",
		},
	},
	{
		name: "multi_web_result_usage", tools: []string{"search_web", "read_links"},
		templates: []string{
			"Look up %s online, open the top result, and tell me the exact figure it reports.",
			"Find a page about %s, read it, and give me the precise number.",
			"Research %s on the web, read the leading source, and report its exact figure.",
		},
	},
}

// resultUsageSuffix marks the categories whose correct answer must incorporate a
// tool's returned content (result-usage). Their prompt %s is the fixture needle's
// Subject and they are scored deterministically against the needle Value.
const resultUsageSuffix = "_result_usage"

// IsResultUsage reports whether a case category is a result-usage category — the
// pipeline scores these on trajectory + answer-incorporates-needle rather than
// the LLM quality judge.
func IsResultUsage(category string) bool { return strings.HasSuffix(category, resultUsageSuffix) }

// fillerFor returns a random entity string appropriate for a category.
func fillerFor(r *rand.Rand, cat string) string {
	switch cat {
	case "memory_lookup", "memory_subject":
		return subjects[r.Intn(len(subjects))]
	case "web_search":
		return topics[r.Intn(len(topics))]
	case "link_read":
		return urls[r.Intn(len(urls))]
	case "image_create":
		return imagePrompts[r.Intn(len(imagePrompts))]
	case "artifacts_create":
		return artifactKinds[r.Intn(len(artifactKinds))]
	case "agent_job":
		return agentTasks[r.Intn(len(agentTasks))]
	case "settings":
		return themes[r.Intn(len(themes))]
	case "route_memory_not_web":
		return subjects[r.Intn(len(subjects))]
	case "route_web_not_memory":
		return topics[r.Intn(len(topics))]
	case "agent_run_not_read":
		return agentTasks[r.Intn(len(agentTasks))]
	case "agent_read_not_run", "image_edit_not_create":
		return "" // templates have no placeholder
	case "workflow_not_job":
		return workflows[r.Intn(len(workflows))]
	case "memory_fetch":
		return memoryIDs[r.Intn(len(memoryIDs))]
	case "agent_workflow":
		return workflows[r.Intn(len(workflows))]
	case "feedback":
		return feedback[r.Intn(len(feedback))]
	case "set_model":
		return models[r.Intn(len(models))]
	case "set_effort":
		return efforts[r.Intn(len(efforts))]
	case "set_tool_prefs":
		return toolPrefs[r.Intn(len(toolPrefs))]
	case "multi_web_read", "parallel_web_image":
		return topics[r.Intn(len(topics))]
	case "multi_subject_scope":
		return subjects[r.Intn(len(subjects))]
	case "multi_job_status":
		return agentTasks[r.Intn(len(agentTasks))]
	case "multi_image_edit":
		return imagePrompts[r.Intn(len(imagePrompts))]
	case "no_tool":
		return chitchat[r.Intn(len(chitchat))]
	case "abstention":
		return abstentions[r.Intn(len(abstentions))]
	case "arg_hallucination":
		return "" // templates omit the required value on purpose
	default:
		_ = people // reserved for future multi-entity templates
		return topics[r.Intn(len(topics))]
	}
}

// Generate produces a deterministic-per-seed dataset of n tool-calling cases.
// n is clamped to [1, 200]; the practice default is small (20-40).
func Generate(seed int64, n int) protocol.Dataset {
	if n < 1 {
		n = 1
	}
	if n > 200 {
		n = 200
	}
	r := rand.New(rand.NewSource(seed))
	return protocol.Dataset{
		Seed:        seed,
		GeneratedAt: protocol.DatasetEpochRFC3339,
		ToolCases:   GenerateCases(r, seed, n),
	}
}

// stratifiedCategoryOrder returns n category indices with a FIXED per-category
// quota (each category appears floor(n/C) or ceil(n/C) times), then shuffles the
// order with the seeded RNG. Fixing the category MIX per run — rather than
// drawing each case's category uniformly at random — removes the multinomial
// category-draw variance that dominated dataset-to-dataset difficulty (the
// per-run score stddev scaled as sqrt(p(1-p)/n)). Every dataset now exercises
// the same balance of easy categories and routing traps, so a miner can't get a
// lucky-easy or unlucky-hard draw. Choosing n as a multiple of the category count
// gives a perfectly balanced set; otherwise the first n%C categories get one
// extra (deterministic, so it adds no between-run variance).
func stratifiedCategoryOrder(r *rand.Rand, n int) []int {
	nc := len(categories)
	order := make([]int, 0, n)
	base, rem := n/nc, n%nc
	for ci := 0; ci < nc; ci++ {
		count := base
		if ci < rem {
			count++
		}
		for k := 0; k < count; k++ {
			order = append(order, ci)
		}
	}
	r.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	return order
}

// GenerateCases emits n raw tool cases from an existing RNG. Exported so the
// anti-cheat generator (internal/gen) can reuse the same templated ground-truth
// and then LLM-paraphrase the prompts. seed drives both the stable case IDs and
// each result-usage prompt's fabricated needle subject (via toolexec.NeedleFor),
// which must match the needle the mock endpoint serves.
func GenerateCases(r *rand.Rand, seed int64, n int) []protocol.ToolCase {
	cases, _ := GenerateCasesWithFillers(r, seed, n)
	return cases
}

// GenerateCasesWithFillers is GenerateCases plus, for each case, the concrete
// entity ("filler") substituted into its template (empty for templates with no
// %s slot). The paraphrase verifier (internal/gen) checks that this entity
// survives realization, so a rewrite that drops it falls back to the template
// rather than silently shipping a case whose ground truth no longer matches the
// prompt.
func GenerateCasesWithFillers(r *rand.Rand, seed int64, n int) ([]protocol.ToolCase, []string) {
	if n < 1 {
		n = 1
	}
	order := stratifiedCategoryOrder(r, n)
	cases := make([]protocol.ToolCase, 0, n)
	fillers := make([]string, 0, n)
	for i := 0; i < n; i++ {
		cat := categories[order[i]]
		tmpl := cat.templates[r.Intn(len(cat.templates))]
		caseID := fmt.Sprintf("%s-%d-%04d", cat.name, seed, i)
		// Result-usage cases: the filler is the fixture needle's Subject, derived
		// from the SAME (seed, caseID) the mock server uses to serve the answer — so
		// the question ("...figure on the Veltrix index...") and the served fact
		// ("the Veltrix index reached 3,418 points") are always coherent.
		var filler string
		if IsResultUsage(cat.name) {
			filler = toolexec.NeedleFor(seed, caseID).Subject
		} else {
			filler = fillerFor(r, cat.name)
		}
		prompt := tmpl

		// Expected tool sequence: multi-hop tools, else the single tool, else none.
		seq := cat.tools
		if len(seq) == 0 && cat.tool != "" {
			seq = []string{cat.tool}
		}

		// argValue is the value pinned into RequiredArgs (defaults to the filler; an
		// intent variant overrides it). usedFiller is the token paraphrase must
		// preserve ("" = freely rephrasable).
		argValue := filler
		usedFiller := ""
		useIntent := len(cat.intents) > 0 && r.Intn(2) == 0
		switch {
		case useIntent:
			it := cat.intents[r.Intn(len(cat.intents))]
			prompt = it.prompt
			argValue = it.value
			// Only a near-miss intent (value appears literally) needs the token kept
			// through paraphrase; a pure-intent prompt is meaning-preservingly rephrasable.
			if strings.Contains(it.prompt, it.value) {
				usedFiller = it.value
			}
		case strings.Contains(tmpl, "%s"):
			prompt = fmt.Sprintf(tmpl, filler)
			// A real tool case has an entity worth preserving through paraphrase;
			// no_tool / abstention fillers ARE the whole (freely rephrasable) message.
			if len(seq) > 0 {
				usedFiller = filler
			}
		}

		tc := protocol.ToolCase{
			ID:              caseID,
			Category:        cat.name,
			Prompt:          prompt,
			AllowExtraTools: false,
		}

		switch {
		case len(seq) == 0:
			tc.ExpectedTools = nil
			tc.MaxToolCalls = 0
			switch cat.name {
			case "abstention":
				tc.ExpectedBehavior = "answer or abstain without calling any tool"
			case "arg_hallucination":
				tc.ExpectedBehavior = "ask for the missing detail; do not call a tool with a fabricated argument"
			default:
				tc.ExpectedBehavior = "respond conversationally without calling any tool"
			}
		case len(seq) == 1:
			ts := protocol.ToolSpec{Name: seq[0]}
			// Exact-value arg ground truth: the filler for a verbatim case, or the
			// resolved value for an intent case (which may not appear in the prompt).
			if cat.argKey != "" && argValue != "" && (useIntent || usedFiller != "") {
				ts.RequiredArgs = map[string]string{cat.argKey: argValue}
			}
			tc.ExpectedTools = []protocol.ToolSpec{ts}
			tc.MaxToolCalls = 1
			tc.ExpectedBehavior = fmt.Sprintf("call %s exactly once", seq[0])
		default:
			tools := make([]protocol.ToolSpec, len(seq))
			for j, tn := range seq {
				tools[j] = protocol.ToolSpec{Name: tn}
			}
			tc.ExpectedTools = tools
			tc.MaxToolCalls = len(seq)
			if cat.unordered {
				tc.Unordered = true
				tc.ExpectedBehavior = "call " + strings.Join(seq, " and ") + " (in any order)"
			} else {
				tc.ExpectedBehavior = "call " + strings.Join(seq, " then ") + " in that order"
			}
		}

		cases = append(cases, tc)
		fillers = append(fillers, usedFiller)
	}
	return cases, fillers
}
