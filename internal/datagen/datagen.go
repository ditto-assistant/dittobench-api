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

	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// category describes one kind of tool-calling case and how to render it.
type category struct {
	name      string
	tool      string // expected tool name; empty means "no tool"
	templates []string
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
		name: "link_read", tool: "read_links",
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
		name: "agent_job", tool: "execute_agent_job",
		templates: []string{
			"Run a background job to %s.",
			"Kick off an agent to %s.",
			"Dispatch a task to %s.",
		},
	},
	{
		name: "settings", tool: "set_theme",
		templates: []string{
			"Switch to %s mode.",
			"Set my theme to %s.",
			"Change the app theme to %s.",
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
}

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
	case "agent_read_not_run":
		return "" // templates have no placeholder
	case "no_tool":
		return chitchat[r.Intn(len(chitchat))]
	case "abstention":
		return abstentions[r.Intn(len(abstentions))]
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
// and then LLM-paraphrase the prompts. seed is only used for stable case IDs.
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
		filler := fillerFor(r, cat.name)
		prompt := tmpl
		usedFiller := ""
		if strings.Contains(tmpl, "%s") {
			prompt = fmt.Sprintf(tmpl, filler)
			// Only a real tool case has an entity worth preserving; no_tool /
			// abstention fillers ARE the whole (freely rephrasable) message.
			if cat.tool != "" {
				usedFiller = filler
			}
		}

		tc := protocol.ToolCase{
			ID:              fmt.Sprintf("%s-%d-%04d", cat.name, seed, i),
			Category:        cat.name,
			Prompt:          prompt,
			MaxToolCalls:    1,
			AllowExtraTools: false,
		}

		if cat.tool != "" {
			tc.ExpectedTools = []protocol.ToolSpec{{Name: cat.tool}}
			tc.ExpectedBehavior = fmt.Sprintf("call %s exactly once", cat.tool)
		} else {
			tc.ExpectedTools = nil
			tc.MaxToolCalls = 0
			if cat.name == "abstention" {
				tc.ExpectedBehavior = "answer or abstain without calling any tool"
			} else {
				tc.ExpectedBehavior = "respond conversationally without calling any tool"
			}
		}

		cases = append(cases, tc)
		fillers = append(fillers, usedFiller)
	}
	return cases, fillers
}
