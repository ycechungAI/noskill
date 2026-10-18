"""Directories a binary's dylib closure lives in (macOS, otool -L), for the sandbox's read-only list."""
import os, re, subprocess, sys

def closure(binary):
    seen, todo, dirs = set(), [os.path.realpath(binary)], set()
    while todo:
        f = todo.pop()
        if f in seen:
            continue
        seen.add(f)
        out = subprocess.run(["otool", "-L", f], capture_output=True, text=True).stdout.splitlines()[1:]
        for line in out:
            p = line.strip().split(" (")[0]
            if p.startswith(("/usr/lib", "/System")) or p.startswith("@"):
                continue
            if os.path.exists(p):
                dirs.add(_root(p))
                dirs.add(_root(os.path.realpath(p)))
                todo.append(os.path.realpath(p))
    dirs.add(_root(os.path.realpath(binary)))
    return sorted(dirs)

def _root(p):
    # /opt/homebrew/Cellar/<pkg>/<ver> or /opt/homebrew/opt/<pkg>, else the file's parent's parent
    m = re.match(r"(/opt/homebrew/(?:Cellar/[^/]+/[^/]+|opt/[^/]+))", p)
    return m.group(1) if m else os.path.dirname(os.path.dirname(p))

if __name__ == "__main__":
    print(",".join(closure(sys.argv[1])))
