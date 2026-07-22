"""Run one DittoBench case through the unmodified Hermes agent loop."""

from __future__ import annotations

import hashlib
import json
import os
import threading
import time
import urllib.request
from contextlib import nullcontext
from dataclasses import dataclass
from typing import Any, Callable

from .protocol import RunRequest, ToolDefinition
from .state import HermesStateStore


MEMORY_READ_TOOLS = frozenset(
    {
        "search_memories",
        "fetch_memories",
        "search_subjects",
        "search_memories_in_subjects",
    }
)


@dataclass
class _RunContext:
    request: RunRequest
    db: Any


def _default_post_json(url: str, payload: dict[str, Any], timeout: float) -> dict[str, Any]:
    body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
    request = urllib.request.Request(
        url,
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=timeout) as response:
        raw = response.read(1 << 20)
    value = json.loads(raw)
    if not isinstance(value, dict):
        raise ValueError("tool endpoint returned a non-object")
    return value


class EvalAgentMixin:
    """Keep benchmark questions out of the seeded recall corpus."""

    def _ensure_db_session(self) -> None:  # pragma: no cover - exercised with Hermes
        return None

    def _flush_messages_to_session_db(self, *args: Any, **kwargs: Any) -> None:  # pragma: no cover
        return None

    def _persist_session(self, *args: Any, **kwargs: Any) -> None:  # pragma: no cover
        return None


class HermesRunner:
    def __init__(
        self,
        state: HermesStateStore,
        *,
        agent_factory: Callable[..., Any] | None = None,
        registry: Any | None = None,
        session_search: Callable[..., str] | None = None,
        post_json: Callable[[str, dict[str, Any], float], dict[str, Any]] = _default_post_json,
    ) -> None:
        self.state = state
        self._agent_factory = agent_factory
        self._registry = registry
        self._session_search = session_search
        self._post_json = post_json
        self._local = threading.local()

    def _hermes(self) -> tuple[Callable[..., Any], Any, Callable[..., str]]:
        if self._agent_factory is None:
            from run_agent import AIAgent

            class EvalAgent(EvalAgentMixin, AIAgent):
                pass

            self._agent_factory = EvalAgent
        if self._registry is None:
            from tools.registry import registry

            self._registry = registry
        if self._session_search is None:
            from tools.session_search_tool import session_search

            self._session_search = session_search
        return self._agent_factory, self._registry, self._session_search

    @staticmethod
    def _queries(args: dict[str, Any]) -> str:
        raw = args.get("queries") or args.get("query") or args.get("pairIds") or args.get("subject_id") or ""
        if isinstance(raw, list):
            return " OR ".join(str(item) for item in raw if str(item).strip())
        return str(raw)

    def _memory_read(self, name: str, args: dict[str, Any]) -> str:
        _, _, session_search = self._hermes()
        context: _RunContext = self._local.context
        query = self._queries(args)
        if not query:
            return session_search(limit=5, db=context.db)
        # These aliases deliberately retain Hermes' native FTS5 behavior. The
        # adapter translates the catalog name, not the retrieval algorithm.
        return session_search(query=query, limit=5, db=context.db)

    def _dispatch_wire_tool(self, name: str, args: dict[str, Any]) -> str:
        context: _RunContext = self._local.context
        if name in MEMORY_READ_TOOLS:
            return self._memory_read(name, args)
        endpoint = context.request.tool_endpoint
        if not endpoint:
            return json.dumps({"error": "no validator tool_endpoint was provided"})
        result = self._post_json(
            endpoint,
            {
                "case_id": context.request.case_id,
                "user_id": context.request.user_id,
                "name": name,
                "args": args,
                "hop": int(getattr(self._local, "hop", 0)),
            },
            15.0,
        )
        if result.get("error"):
            return json.dumps({"error": result["error"]})
        return str(result.get("result", ""))

    def _register_tools(self, tools: tuple[ToolDefinition, ...]) -> None:
        _, registry, _ = self._hermes()
        for tool in tools:
            def handler(args: dict[str, Any], _name: str = tool.name, **_: Any) -> str:
                return self._dispatch_wire_tool(_name, args)

            registry.register(
                name=tool.name,
                toolset="dittobench-wire",
                schema={
                    "name": tool.name,
                    "description": tool.description,
                    "parameters": tool.parameters,
                },
                handler=handler,
                override=True,
            )

    def _preflight(self, request: RunRequest) -> dict[str, Any]:
        started = time.monotonic()
        if not request.tool_endpoint:
            raise ValueError("preflight requires tool_endpoint")
        self._post_json(
            request.tool_endpoint,
            {
                "case_id": request.case_id,
                "user_id": request.user_id,
                "name": "search_web",
                "args": {"queries": ["dittobench reachability preflight"]},
                "hop": 0,
            },
            15.0,
        )
        return {
            "final_text": "DittoBench tool endpoint is reachable.",
            "tool_calls": [
                {
                    "name": "search_web",
                    "args": {"queries": ["dittobench reachability preflight"]},
                    "hop": 0,
                }
            ],
            "prompt_tokens": 0,
            "output_tokens": 0,
            "latency_ms": int((time.monotonic() - started) * 1000),
        }

    def run(self, request: RunRequest) -> dict[str, Any]:
        if request.case_id.startswith("preflight:"):
            return self._preflight(request)

        # Hermes SessionDB explicitly supports multiple reader threads and the
        # benchmark never seeds while cases are running. Serializing every case
        # behind the user lock would defeat the scorer's bounded tool-case
        # concurrency without improving isolation; physical per-user databases
        # remain the isolation boundary.
        with nullcontext():
            db = self.state.db_for(request.user_id)
            self._register_tools(request.tools)
            agent_factory, _, _ = self._hermes()
            observed: list[dict[str, Any]] = []

            def on_tool_start(_call_id: str, name: str, args: dict[str, Any]) -> None:
                hop = len(observed)
                self._local.hop = hop
                observed.append({"name": name, "args": args or {}, "hop": hop})

            agent = agent_factory(
                base_url=os.environ.get("CHUTES_BASE_URL") or os.environ.get("OPENAI_BASE_URL"),
                api_key=os.environ.get("CHUTES_API_KEY") or os.environ.get("OPENAI_API_KEY") or "relay",
                provider="custom",
                api_mode="chat_completions",
                model=os.environ.get("DITTOBENCH_MODEL", "qwen/qwen3-32b"),
                max_iterations=int(os.environ.get("HERMES_DITTOBENCH_MAX_ITERATIONS", "8")),
                tool_delay=0,
                enabled_toolsets=[],
                quiet_mode=True,
                ephemeral_system_prompt=request.system_prompt,
                session_id="dittobench_eval_"
                + hashlib.sha256(request.case_id.encode("utf-8")).hexdigest()[:32],
                session_db=db,
                skip_context_files=True,
                load_soul_identity=False,
                skip_memory=True,
                tool_start_callback=on_tool_start,
                reasoning_config={"enabled": False},
            )
            # The validator-style model relay deliberately returns ordinary
            # Chat Completions JSON (streaming is disabled for deterministic
            # accounting). Tell Hermes to request that shape; otherwise its
            # streaming consumer treats the valid non-stream response as an
            # empty SSE stream and never executes the returned tool call.
            agent._disable_streaming = True
            # AIAgent was intentionally initialized with no Hermes tools. Add
            # exactly the validator-supplied catalog; registered handlers above
            # route each call to the observed endpoint or native FTS memory.
            agent.tools = [tool.openai_schema() for tool in request.tools]
            agent.valid_tool_names = {tool.name for tool in request.tools}
            agent._end_session_on_close = False

            started = time.monotonic()
            self._local.context = _RunContext(request=request, db=db)
            self._local.hop = 0
            try:
                result = agent.run_conversation(request.user_input)
            finally:
                self._local.context = None
                try:
                    agent.shutdown_memory_provider()
                except Exception:
                    pass
                try:
                    agent.release_clients()
                except Exception:
                    pass

            return {
                "final_text": str(result.get("final_response") or ""),
                "tool_calls": observed,
                "prompt_tokens": int(result.get("input_tokens") or result.get("prompt_tokens") or 0),
                "output_tokens": int(result.get("output_tokens") or result.get("completion_tokens") or 0),
                "latency_ms": int((time.monotonic() - started) * 1000),
            }
