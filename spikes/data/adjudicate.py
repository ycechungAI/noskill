"""Settle the requests where your labels (b) and the AI's (c) disagree.

Shows each disagreement side by side with the definitions of the fields in question.
Type 1 to keep yours, 2 to take the AI's, or type corrections (e.g. `tools=yes`) on top
of yours. `back` re-asks the previous one, `q` quits; answers are saved as you go.
The settled label is stored as labels.final, and spikes/s3/report.py scores against it.
Usage: python3 spikes/data/adjudicate.py
"""
import json
import os
from pathlib import Path

D = Path(__file__).parent
P, PC = D / "router-200.jsonl", D / "router-200.labels-c.jsonl"
TYPES = ["chat", "code", "reasoning", "math", "research", "summarise", "extract", "translate", "creative", "vision", "plan"]
LEVELS = ["trivial", "easy", "moderate", "hard", "expert"]
FIELDS = ["task_type", "difficulty", "needs_web", "needs_vision", "needs_tools"]
NAMES = {"task_type": "type", "difficulty": "difficulty", "needs_web": "web", "needs_vision": "image", "needs_tools": "tools"}
KEYS = {"type": "task_type", "difficulty": "difficulty", "web": "needs_web", "image": "needs_vision", "tools": "needs_tools"}
MEANING = {
    "task_type": "type: the main job. chat (talk, advice, general knowledge) · code · reasoning (logic, trade-offs, diagnosis) · "
                 "math · research (needs current info from the web) · summarise · extract (pull fields/data out) · translate · "
                 "creative · vision (about an attached image) · plan",
    "difficulty": "difficulty: how hard for a small (4B) model to do well. 1 trivial … 5 expert",
    "needs_web": "web: answering needs up-to-date or outside information from the internet. Text you paste in, or URLs that "
                 "are only being copied out, don't count",
    "needs_vision": "image: there's an attached image that must be looked at",
    "needs_tools": "tools: needs something outside the model: the web, your files, the current time, or running code for an "
                   "exact answer. So web=yes means tools=yes too",
}
YES, NO = {"yes", "y", "1", "true"}, {"no", "n", "0", "false"}

items = [json.loads(l) for l in P.read_text().splitlines() if l.strip()]
ai = {json.loads(l)["id"]: json.loads(l) for l in PC.read_text().splitlines() if l.strip()}


def save():
    tmp = P.with_suffix(".jsonl.tmp")
    tmp.write_text("".join(json.dumps(i, ensure_ascii=False) + "\n" for i in items))
    os.replace(tmp, P)


def fmt(lab, mark=()):
    def f(k):
        v = lab[k]
        v = f"{v} ({LEVELS[v - 1]})" if k == "difficulty" else ("yes" if v is True else "no" if v is False else v)
        s = f"{NAMES[k]}={v}"
        return f"[{s}]" if k in mark else s
    return "  ".join(f(k) for k in FIELDS)


def differs(b, c):
    return [k for k in FIELDS if (abs(b[k] - c[k]) >= 2 if k == "difficulty" else b[k] != c[k])]


def contradictions(lab):
    return ["web=yes but tools=no (web access is a tool)"] if lab["needs_web"] and not lab["needs_tools"] else []


def parse(s, base):
    out = dict(base)
    for part in s.split():
        k, eq, v = part.partition("=")
        k, v = k.lower(), v.lower()
        if not eq or k not in KEYS:
            return None, f"didn't understand '{part}'. Type 1, 2, or name=value like tools=yes"
        key = KEYS[k]
        if key == "task_type":
            if v not in TYPES:
                return None, f"'{v}' isn't a type: {', '.join(TYPES)}"
            out[key] = v
        elif key == "difficulty":
            if v.isdigit() and 1 <= int(v) <= 5:
                out[key] = int(v)
            elif v in LEVELS:
                out[key] = LEVELS.index(v) + 1
            else:
                return None, "difficulty is 1–5"
        else:
            if v not in YES | NO:
                return None, f"{k} is yes or no"
            out[key] = v in YES
    return out, None


todo = [it for it in items if "b" in it["labels"] and differs(it["labels"]["b"], ai[it["id"]]["labels"])]
left = [it for it in todo if "final" not in it["labels"]]
print(f"{len(todo)} requests where you and the AI disagree; {len(left)} still to settle.\n")
print("1 = keep yours · 2 = take the AI's · or fix yours, e.g. tools=yes · back · q\n")
k = 0
while k < len(todo):
    it = todo[k]
    if "final" in it["labels"]:
        k += 1
        continue
    b, c = it["labels"]["b"], ai[it["id"]]["labels"]
    diff = differs(b, c)
    print(f"[{it['id']} · {sum(1 for x in todo if 'final' not in x['labels'])} left]  {it['prompt'][:400]}"
          + (f"\n   attached: {', '.join(it['attachments'])}" if it["attachments"] else ""))
    print(f"   1 you: {fmt(b, diff)}" + "".join(f"   ⚠ {w}" for w in contradictions(b)))
    print(f"   2 AI:  {fmt(c, diff)}" + (f"   (AI was unsure: {ai[it['id']]['unsure']})" if ai[it["id"]]["unsure"] else ""))
    for f in diff:
        print(f"     · {MEANING[f]}")
    while True:
        s = input("   settle> ").strip()
        if s.lower() in ("q", "quit"):
            k = len(todo) + 1
            break
        if s.lower() == "back":
            prev = [j for j in range(k) if "final" in todo[j]["labels"]]
            if prev:
                k = prev[-1]
                todo[k]["labels"].pop("final", None)
                todo[k]["labels"].pop("final_via", None)
                save()
                print()
            else:
                print("   nothing to go back to")
                continue
            break
        if s == "1":
            final, via = dict(b), "kept yours"
        elif s == "2":
            final, via = dict(c), "took AI's"
        elif s:
            final, err = parse(s, b)
            if err:
                print("   " + err)
                continue
            via = "edited"
        else:
            print("   type 1, 2, or a correction")
            continue
        if final["needs_web"] and not final["needs_tools"]:
            final["needs_tools"] = True  # by definition: web access is a tool
            print("   (tools set to yes, because web=yes needs a tool)")
        it["labels"]["final"], it["labels"]["final_via"] = final, via
        save()
        print(f"   saved: {fmt(final)}\n")
        k += 1
        break
done = [x for x in todo if "final" in x["labels"]]
print(f"\nSettled {len(done)} of {len(todo)}: " + ", ".join(f"{v} {n}" for v, n in
      sorted({(v, sum(1 for x in done if x['labels']['final_via'] == v)) for v in {x['labels']['final_via'] for x in done}})))
