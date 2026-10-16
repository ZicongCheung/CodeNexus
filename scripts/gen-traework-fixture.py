"""Generate the TraeWork contract fixture from the reference implementation.

Run once from a checkout of autumnsentiment/Trae2api-cn (with httpx installed):

    python tools/gen_traework_fixture.py > traework_reference.json

The committed fixture is what internal/api/traework_reference_test.go compares
the Go port against, so the comparison does not need the reference at test
time.
"""

import asyncio
import json
import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from src import traework_compat as tc  # noqa: E402


DETECTION = [
    {"path": "/v1/traework/chat", "headers": {}, "body": {}},
    {"path": "/api/ide/v2/llm_raw_chat", "headers": {}, "body": {}},
    {"path": "/api/agent/v3/custom_model_proxy/chat", "headers": {}, "body": {}},
    {"path": "/api/agent/v3/custom_model_connectivity_check", "headers": {}, "body": {}},
    {"path": "/v1/chat/completions", "headers": {"user-agent": "TraeWork"}, "body": {}},
    {"path": "/v1/chat/completions", "headers": {"x-bridge-transport": "aha"}, "body": {}},
    {"path": "/v1/chat/completions", "headers": {"extra": "e", "x-app-id": "a", "x-ide-function": "f"}, "body": {}},
    {"path": "/v1/chat/completions", "headers": {}, "body": {"is_custom_model": True}},
    {"path": "/v1/chat/completions", "headers": {}, "body": {"data": {"custom_model": {"config_name": "glm-5.3"}}}},
    {"path": "/v1/chat/completions", "headers": {}, "body": {"model_info": {"custom_model_id": "x"}}},
    {"path": "/v1/chat/completions", "headers": {}, "body": {"model": "gpt-4o"}},
    {"path": "/v1/chat/completions", "headers": {}, "body": {"agent_type": "solo_agent_remote", "messages": []}},
]

NORMALIZE = [
    {
        "body": {
            "data": {
                "is_custom_model": True,
                "config_name": "glm-5.3",
                "stream": True,
                "messages": [{"role": "user", "content": "hi"}],
                "tools": [{"type": "function", "function": {"name": "f", "parameters": {"type": "object"}}}],
            }
        },
        "headers": {"user-agent": "TraeWork"},
        "path": "/v1/traework/chat",
    },
    {
        "body": {
            "params": {
                "payload": {
                    "messages": [{"role": 1, "content": [{"type": "text", "text": "deep"}]}],
                    "config_name": "deepseek-v4-pro",
                    "session_id": "s-1",
                    "request_id": "r-1",
                    "stream": False,
                }
            }
        },
        "headers": {},
        "path": "/v1/chat/completions",
    },
    {
        "body": {"query": [{"type": "text", "data": {"content": "from query"}}], "model": "glm-5.2"},
        "headers": {},
        "path": "/v1/chat/completions",
    },
    {
        "body": {"messages": "plain string", "model_name": "glm-5.2", "thinking": {"budget_tokens": 20000}},
        "headers": {},
        "path": "/v1/chat/completions",
    },
]

CONNECTIVITY = [
    {"data": {"config_name": "glm-5.3"}},
    {"data": {"custom_model": {"config_name": "deepseek-v4-pro", "provider": "acme"}}},
    {"data": {}},
]

COMPLETION = [
    {"choices": [{"message": {"content": "ok"}, "finish_reason": "stop"}]},
    {
        "choices": [
            {
                "message": {
                    "content": "let me look",
                    "tool_calls": [
                        {"id": "call_1", "function": {"name": "read_file", "arguments": '{"path":"a"}'}}
                    ],
                },
                "finish_reason": "tool_calls",
            }
        ]
    },
]

def sse(*payloads):
    """Frame OpenAI payloads the way a transport would deliver them."""
    out = []
    for item in payloads:
        if isinstance(item, str):
            out.append("data: " + item + "\n\n")
            continue
        out.append("data: " + json.dumps(item, ensure_ascii=False) + "\n\n")
    return out


STREAM = [
    sse(
        {"choices": [{"delta": {"content": "He"}}]},
        {"choices": [{"delta": {"content": "llo"}}]},
        {"choices": [{"delta": {}, "finish_reason": "stop"}],
         "usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}},
        "[DONE]",
    ),
    sse(
        {"choices": [{"delta": {"tool_calls": [{"index": 0, "id": "call_1", "function": {"name": "f"}}]}}]},
        {"choices": [{"delta": {"tool_calls": [{"index": 0, "function": {"arguments": "{\\\"a\\\""}}]}}]},
        {"choices": [{"delta": {"tool_calls": [{"index": 0, "function": {"arguments": ":1}"}}]}}]},
        {"choices": [{"delta": {}, "finish_reason": "tool_calls"}]},
        "[DONE]",
    ),
    sse({"choices": [{"delta": {}}]}, "[DONE]"),
]


async def collect_stream(payloads):
    async def source():
        for item in payloads:
            yield item

    frames = []
    async for frame in tc.translate_openai_stream_to_traework(source(), model="glm-5.3", request_id="req-1"):
        frames.append(frame)
    return frames


def main():
    out = {"detection": [], "normalize": [], "connectivity": [], "completion": [], "stream": []}

    for case in DETECTION:
        out["detection"].append(
            {
                "case": case,
                "want": bool(tc.is_traework_request(case["path"], case["headers"], case["body"])),
            }
        )

    for case in NORMALIZE:
        d = tc.normalize_inbound_request(case["body"], headers=case["headers"], path=case["path"])
        out["normalize"].append(
            {
                "case": case,
                "want": {
                    "model": d.model,
                    "stream": d.stream,
                    "protocol": d.protocol,
                    "messages": d.messages,
                    "session_id_given": bool(d.session_id),
                    "request_id_given": bool(d.request_id),
                    "tool_protocol_requested": bool(d.options.get("_tool_protocol_requested")),
                },
            }
        )

    for case in CONNECTIVITY:
        r = tc.connectivity_response(case)
        item = r["model"]
        out["connectivity"].append(
            {
                "case": case,
                "want": {
                    "is_preset": item.get("is_preset"),
                    "is_custom_base_url": item.get("is_custom_base_url"),
                    "config_name": r.get("config_name"),
                    "model_name": r.get("model_name"),
                    "provider": r.get("provider"),
                    "success": r.get("success"),
                    "code": r.get("code"),
                    "model_id": item.get("id"),
                    "prompt_max_tokens": item.get("prompt_max_tokens"),
                    "max_tokens": item.get("max_tokens"),
                },
            }
        )

    for case in COMPLETION:
        out["completion"].append(
            {"case": case, "want": tc.openai_completion_to_traework(case, model="glm-5.3")}
        )

    for payloads in STREAM:
        out["stream"].append({"case": payloads, "want": asyncio.run(collect_stream(payloads))})

    json.dump(out, sys.stdout, ensure_ascii=False, indent=1, sort_keys=True)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
