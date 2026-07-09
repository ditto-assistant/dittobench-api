// Package catalog defines the Ditto tool catalog presented to harnesses. Each
// entry has a name, a short description, and a minimal JSON-schema parameter
// definition. This is the tool menu a harness sees on every RunRequest.
package catalog

import (
	"encoding/json"

	"github.com/ditto-assistant/dittobench-api/pkg/protocol"
)

// params is a tiny helper to build a JSON-schema object for parameters.
func params(props map[string]string, required ...string) json.RawMessage {
	properties := map[string]any{}
	for name, desc := range props {
		properties[name] = map[string]any{
			"type":        "string",
			"description": desc,
		}
	}
	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	b, _ := json.Marshal(schema)
	return json.RawMessage(b)
}

// Catalog returns the full Ditto tool catalog.
func Catalog() []protocol.ToolDefinition {
	return []protocol.ToolDefinition{
		{
			Name:        "create_image",
			Description: "Generate a new image from a text prompt.",
			Parameters:  params(map[string]string{"prompt": "what to draw"}, "prompt"),
		},
		{
			Name:        "edit_image",
			Description: "Edit an existing image given an instruction.",
			Parameters:  params(map[string]string{"image_url": "image to edit", "instruction": "how to change it"}, "image_url", "instruction"),
		},
		{
			Name:        "read_links",
			Description: "Fetch and read the contents of one or more URLs.",
			Parameters:  params(map[string]string{"url": "the URL to read"}, "url"),
		},
		{
			Name:        "search_web",
			Description: "Search the public web for current information.",
			Parameters:  params(map[string]string{"query": "the search query"}, "query"),
		},
		{
			Name:        "search_memories",
			Description: "Search the user's long-term memories by semantic query.",
			Parameters:  params(map[string]string{"query": "what to recall"}, "query"),
		},
		{
			Name:        "search_subjects",
			Description: "Find subject/topic clusters in the user's memories.",
			Parameters:  params(map[string]string{"query": "topic to find"}, "query"),
		},
		{
			Name:        "fetch_memories",
			Description: "Fetch specific memories by ID or outline.",
			Parameters:  params(map[string]string{"ids": "comma-separated memory IDs"}),
		},
		{
			Name:        "search_memories_in_subjects",
			Description: "Search memories scoped to specific subjects.",
			Parameters:  params(map[string]string{"query": "what to recall", "subjects": "subjects to scope to"}, "query"),
		},
		{
			Name:        "artifacts",
			Description: "Create an interactive, previewable artifact (web app, doc, game).",
			Parameters:  params(map[string]string{"spec": "what to build"}, "spec"),
		},
		{
			Name:        "execute_agent_job",
			Description: "Dispatch a one-off background agent job.",
			Parameters:  params(map[string]string{"task": "the task to run"}, "task"),
		},
		{
			Name:        "execute_agent_workflow",
			Description: "Run a predefined multi-step agent workflow.",
			Parameters:  params(map[string]string{"workflow": "workflow name", "input": "workflow input"}, "workflow"),
		},
		{
			Name:        "get_agent_job_status",
			Description: "Check the status of a running or finished agent job.",
			Parameters:  params(map[string]string{"job_id": "the job ID"}, "job_id"),
		},
		{
			Name:        "list_agent_jobs",
			Description: "List the user's recent agent jobs.",
			Parameters:  params(map[string]string{"limit": "max number to return"}),
		},
		{
			Name:        "file_feedback_for_team",
			Description: "Send feedback or a bug report to the Ditto team.",
			Parameters:  params(map[string]string{"message": "the feedback message"}, "message"),
		},
		{
			Name:        "set_theme",
			Description: "Change the app's color theme.",
			Parameters:  params(map[string]string{"theme": "theme name, e.g. dark or light"}, "theme"),
		},
		{
			Name:        "set_main_model",
			Description: "Change the primary chat model.",
			Parameters:  params(map[string]string{"model": "model identifier"}, "model"),
		},
		{
			Name:        "set_reasoning_effort",
			Description: "Set the reasoning effort level for responses.",
			Parameters:  params(map[string]string{"effort": "low, medium, or high"}, "effort"),
		},
		{
			Name:        "set_chat_tool_preferences",
			Description: "Enable or disable specific tools for chat.",
			Parameters:  params(map[string]string{"preferences": "tool preference settings"}, "preferences"),
		},
	}
}
