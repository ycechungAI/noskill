"""Second-labeller pass over router-200 (labeller "b").

Shows each request with labeller "a"'s guess. Press Enter to agree, or type only
what's wrong, e.g. `type=research difficulty=3 web=yes`. `back` re-asks the previous
question. `q` quits; every answer is saved as you go, so running it again carries on
where you stopped. Question numbers match the request ids (question 13 = r013).
Run it a day after the first pass if you are both labellers (PHASE0_TESTS.md).
Usage: python3 spikes/data/review.py
"""
import json
import os
import time
from pathlib import Path

P = Path(__file__).parent / "router-200.jsonl"
TYPES = ["chat", "code", "reasoning", "math", "research", "summarise", "extract", "translate", "creative", "vision", "plan"]
LEVELS = ["trivial", "easy", "moderate", "hard", "expert"]
# what you type → field in the data file (short forms from the first version still work)
KEYS = {"type": "task_type", "t": "task_type",
        "difficulty": "difficulty", "d": "difficulty",
        "web": "needs_web", "w": "needs_web",
        "image": "needs_vision", "v": "needs_vision", "i": "needs_vision",
        "tools": "needs_tools", "x": "needs_tools"}
YES, NO = {"yes", "y", "1", "true"}, {"no", "n", "0", "false"}
TOO_FAST = 0.5  # seconds; an Enter faster than this after a question appears is ignored (held-down or repeated Enter)

HELP = f"""
How to answer:
  Enter                    agree with the guess
  type=research            what kind of task: {", ".join(TYPES)}
  difficulty=3             how hard for a small (4B) model: 1 trivial, 2 easy, 3 moderate, 4 hard, 5 expert
  web=yes                  needs up-to-date information from the internet?
  image=yes                is there an attached image that must be looked at?
  tools=yes                needs something outside the model: web, your files, the clock, or running code for exact maths?
  Combine them:            type=research difficulty=3 web=yes tools=yes
  back                     go back and re-answer the previous question
  ?                        show this help      q   quit (answers are already saved)
"""

items = [json.loads(l) for l in P.read_text().splitlines() if l.strip()]


def save():
    tmp = P.with_suffix(".jsonl.tmp")
    tmp.write_text("".join(json.dumps(i, ensure_ascii=False) + "\n" for i in items))
    os.replace(tmp, P)  # atomic, so S3 never reads a half-written file


def yn(v):
    return "yes" if v else "no"


def show(lab):
    return (f"type={lab['task_type']}  difficulty={lab['difficulty']} ({LEVELS[lab['difficulty'] - 1]})  "
            f"web={yn(lab['needs_web'])}  image={yn(lab['needs_vision'])}  tools={yn(lab['needs_tools'])}")


def parse(s, base):
    """Apply `name=value` corrections to a copy of base. Returns (labels, error)."""
    b = dict(base)
    for part in s.split():
        k, eq, v = part.partition("=")
        k, v = k.lower(), v.lower()
        if not eq or k not in KEYS:
            return None, f"didn't understand '{part}'. Use name=value, e.g. type=research or web=yes (? for help)"
        key = KEYS[k]
        if key == "task_type":
            if v not in TYPES:
                return None, f"'{v}' isn't a task type. Pick one of: {', '.join(TYPES)}"
            b[key] = v
        elif key == "difficulty":
            if v.isdigit() and 1 <= int(v) <= 5:
                b[key] = int(v)
            elif v in LEVELS:
                b[key] = LEVELS.index(v) + 1
            else:
                return None, "difficulty is 1–5 (or trivial, easy, moderate, hard, expert)"
        else:
            if v not in YES | NO:
                return None, f"{k} is yes or no"
            b[key] = v in YES
    return b, None


def next_open(i):
    while i < len(items) and "b" in items[i]["labels"]:
        i += 1
    return i


left = sum(1 for i in items if "b" not in i["labels"])
print(f"{left} of {len(items)} requests left.")
print(HELP)
i = next_open(0)
while i < len(items):
    it = items[i]
    left = sum(1 for x in items if "b" not in x["labels"])
    a = it["labels"]["a"]
    att = f"\n   attached: {', '.join(it['attachments'])}" if it["attachments"] else ""
    print(f"[question {i + 1} of {len(items)} · {left} left] {it['id']}  {it['prompt'][:300]}{att}")
    print(f"   guess: {show(a)}")
    action, b = None, None
    while True:
        t0 = time.monotonic()
        s = input("   your answer (Enter = agree)> ").strip()
        if not s and time.monotonic() - t0 < TOO_FAST:
            print("   (Enter came too fast, so it was ignored. Press Enter again to agree.)")
            continue
        if s.lower() in ("q", "quit"):
            action = "quit"
            break
        if s.lower() == "back":
            action = "back"
            break
        if s == "?":
            print(HELP)
            continue
        b, err = parse(s, a)
        if err:
            print("   " + err)
            continue
        break
    if action == "quit":
        break
    if action == "back":
        if i == 0:
            print("   This is the first question.\n")
            continue
        i -= 1
        items[i]["labels"].pop("b", None)
        save()
        print(f"   Going back to question {i + 1}.\n")
        continue
    it["labels"]["b"] = b
    save()
    if b != a:
        print(f"   saved: {show(b)}")
    print()
    i = next_open(i + 1)
save()
done = [i for i in items if "b" in i["labels"]]
agree = sum(i["labels"]["a"]["task_type"] == i["labels"]["b"]["task_type"] for i in done)
print(f"\nReviewed {len(done)} of {len(items)}; same task type as the guess: {agree}/{len(done)}")
