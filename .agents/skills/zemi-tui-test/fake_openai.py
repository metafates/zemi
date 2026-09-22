# Fake OpenAI-compatible server for driving zemi without a real model.
# A user message "call NAME {json}" makes the model call that tool once; anything
# else gets a short streamed reply. Non-streaming requests (titles, summaries)
# get a plain chat.completion. Every request body lands in last_request.json.
# usage: python3 fake_openai.py PORT
import json, re, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        open("last_request.json", "w").write(json.dumps(body, indent=1))
        last = body["messages"][-1]
        c = last.get("content") or ""
        text = c if isinstance(c, str) else " ".join(p.get("text", "") for p in c)
        m = last["role"] == "user" and re.search(r"call (\w+) (\{.*\})", text)
        usage = {"prompt_tokens": 1000, "completion_tokens": 50, "total_tokens": 1050}
        if not body.get("stream"):
            msg = {"role": "assistant", "content": "done"}
            out = {"id": "c", "object": "chat.completion", "model": "m", "usage": usage,
                   "choices": [{"index": 0, "message": msg, "finish_reason": "stop"}]}
            data = json.dumps(out).encode()
            self.send_response(200); self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data))); self.end_headers(); self.wfile.write(data); return
        self.send_response(200); self.send_header("Content-Type", "text/event-stream"); self.end_headers()
        def ev(delta, fin=None, u=None):
            d = {"id": "c", "object": "chat.completion.chunk", "model": "m",
                 "choices": [{"index": 0, "delta": delta, "finish_reason": fin}]}
            if u: d["usage"] = u
            self.wfile.write(("data: " + json.dumps(d) + "\n\n").encode()); self.wfile.flush()
        if m:
            ev({"role": "assistant", "tool_calls": [{"index": 0, "id": "t1", "type": "function",
                "function": {"name": m.group(1), "arguments": m.group(2)}}]})
            ev({}, "tool_calls", usage)
        else:
            ev({"role": "assistant", "reasoning_content": "thinking a moment"})
            for w in "done, here is the reply.".split():
                ev({"content": w + " "})
            ev({}, "stop", usage)
        self.wfile.write(b"data: [DONE]\n\n")

HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
