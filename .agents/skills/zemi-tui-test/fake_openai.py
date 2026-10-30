# Fake OpenAI-compatible server for driving zemi without a real model.
# A user message "call NAME {json}" makes the model call that tool once; anything
# else gets a short streamed reply. Non-streaming requests (the object calls:
# classify, zemi.model.ask) get a plain chat.completion. Every accepted request
# body lands in OUTDIR/last_request.json (atomic) and OUTDIR/requests/NNN.json,
# so two consecutive bodies diff byte for byte (prompt cache); a refused
# request (a 429) records nothing.
# While OUTDIR/rate_fail holds a positive count, that many requests are refused
# with a 429 and a 300ms retry-after, the count decrementing per refusal.
# OUTDIR/rate_limit holds "REQUESTS[:TOKENS][:WINDOW]" (e.g. "40:52000:60"): the
# server enforces it as a sliding window over its own requests and refuses one
# that would exceed it with a 429 whose Retry-After names when the oldest
# entry leaves the window.
# While OUTDIR/drop holds a positive count, that many requests get a streamed
# reply cut off mid-stream (headers and a couple of events, then the socket),
# the count decrementing per drop. OUTDIR/delay_ms sleeps that long before a
# reply that is not a refusal (a 429 or a drop), holding the turn open;
# OUTDIR/pace_ms sleeps that long between streamed chunks, pacing the stream.
# usage: python3 fake_openai.py PORT [OUTDIR]
#   (OUTDIR default: the CWD)
import json, os, re, sys, time
from http.server import BaseHTTPRequestHandler, HTTPServer

OUT = sys.argv[2] if len(sys.argv) > 2 else "."
os.makedirs(os.path.join(OUT, "requests"), exist_ok=True)
REQS, TOKS = [], []  # sliding-window entries: monotonic times, (time, completion tokens)

def ctl(name):
    try:
        return int(open(os.path.join(OUT, name)).read().strip() or 0)
    except (OSError, ValueError):
        return 0

def ctl_text(name):
    try:
        return open(os.path.join(OUT, name)).read().strip()
    except OSError:
        return ""

def set_ctl(name, v):
    tmp = os.path.join(OUT, name + ".tmp")
    open(tmp, "w").write(str(v))
    os.replace(tmp, os.path.join(OUT, name))

class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        last = body["messages"][-1]
        c = last.get("content") or ""
        text = c if isinstance(c, str) else " ".join(p.get("text", "") for p in c)
        m = last["role"] == "user" and re.match(r"call (\w+) (\{.*\})", text)
        n = len(os.listdir(os.path.join(OUT, "requests"))) + 1
        print("REQ %d stream=%s %s" % (n, body.get("stream"), text[:60].replace("\n", " ")), flush=True)
        try:
            parts = (open(os.path.join(OUT, "rate_limit")).read().strip() or "0").split(":")
            reqs_cap = int(parts[0])
            tok_cap = int(parts[1]) if len(parts) > 1 and parts[1] else 0
            win = float(parts[2]) if len(parts) > 2 and parts[2] else 60.0
        except (OSError, ValueError):
            reqs_cap, tok_cap, win = 0, 0, 60.0
        if reqs_cap > 0 or tok_cap > 0:
            now = time.monotonic()
            while REQS and REQS[0] <= now - win: REQS.pop(0)
            while TOKS and TOKS[0][0] <= now - win: TOKS.pop(0)
            retry = None
            if reqs_cap > 0 and len(REQS) >= reqs_cap:
                retry = REQS[0] + win - now
            elif tok_cap > 0 and sum(t for _, t in TOKS) + 50 > tok_cap:
                retry = TOKS[0][0] + win - now
            if retry is not None:
                out = json.dumps({"error": {"message": "Rate limit exceeded", "type": "rate_limit_error"}}).encode()
                self.send_response(429); self.send_header("Content-Type", "application/json")
                self.send_header("Retry-After", "%.0f" % max(1, retry))
                self.send_header("Content-Length", str(len(out))); self.end_headers(); self.wfile.write(out)
                print("REQ %d -> 429 window full (%d reqs, %d toks in %.0fs)" % (n, len(REQS), sum(t for _, t in TOKS), win), flush=True)
                return
            REQS.append(now)
            TOKS.append((now, 50))
        failp = os.path.join(OUT, "rate_fail")
        fail = ctl("rate_fail")
        if fail > 0:
            set_ctl("rate_fail", fail - 1)
            out = json.dumps({"error": {"message": "Rate limit exceeded", "type": "rate_limit_error"}}).encode()
            self.send_response(429); self.send_header("Content-Type", "application/json")
            self.send_header("Retry-After-Ms", "300")
            self.send_header("Content-Length", str(len(out))); self.end_headers(); self.wfile.write(out)
            print("REQ %d -> 429 (%d left)" % (n, fail - 1), flush=True)
            return
        drop = ctl("drop")
        if drop > 0:
            set_ctl("drop", drop - 1)
            self.send_response(200); self.send_header("Content-Type", "text/event-stream"); self.end_headers()
            self.wfile.write(b'data: {"id":"c","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"half a"}}]}\n\n')
            self.wfile.flush()
            self.connection.close()
            print("REQ %d -> dropped mid-stream" % n, flush=True)
            return
        # the body is recorded only once the fake commits to answering it: a
        # refused request (a 429) never reaches a model, so it must not count
        # for the lab's request counter
        data = json.dumps(body, indent=1)
        tmp = os.path.join(OUT, "requests/.%d.json" % n)
        open(tmp, "w").write(data)
        os.replace(tmp, os.path.join(OUT, "requests/%03d.json" % n))
        tmp2 = os.path.join(OUT, ".last_request.tmp")
        open(tmp2, "w").write(data)
        os.replace(tmp2, os.path.join(OUT, "last_request.json"))
        # delay_ms holds the reply (turn) open, pace_ms spaces the chunks
        delay = ctl("delay_ms")
        if delay > 0:
            time.sleep(delay / 1000.0)
        pace = ctl("pace_ms") / 1000.0
        # while OUTDIR/judge holds text, the next goal check ("Judge whether")
        # answers with it, once: the goal's verdicts scripted per check
        reply = ctl_text("judge") if text.startswith("Judge whether") else ""
        if reply:
            set_ctl("judge", "")
        # a builder call (Compaction: compress message / merge lines) answers
        # with the one-lined head 400B of its <input> block: a
        # compression-shaped digest instead of the canned line
        if not reply and re.search(r"Compaction: (compress message|merge lines)", text):
            inp = re.search(r"<input>(.*)</input>", text, re.S)
            if inp:
                reply = " ".join(inp.group(1).split())[:400]
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
            if pace:
                time.sleep(pace)
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
            for w in (reply or "done, here is the reply.").split():
                ev({"content": w + " "})
            ev({}, "stop", usage)
        self.wfile.write(b"data: [DONE]\n\n")

HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
