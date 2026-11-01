"""
Reference rules engine for spec/rules/README.md, and the conformance runner: `python conformance/reference.py` checks every case in
spec/conformance/ against it. Stdlib only. pkg/policy (Go) must pass the same cases; this one keeps the cases honest
while it's written, and shows the spec in ~150 lines.
"""
import json
import re
import sys
from datetime import datetime, timezone
from pathlib import Path
from zoneinfo import ZoneInfo

SUBJECTS = {"mcp", "http", "k8s"}
EFFECTS = {"allow", "deny", "approve"}
TEXT_OPS = {"is", "is_not", "contains", "not_contains", "matches", "in"}
NUM_OPS = {"gt": float.__gt__, "gte": float.__ge__, "lt": float.__lt__, "lte": float.__le__, "eq": float.__eq__}
DAYS = ["mon", "tue", "wed", "thu", "fri", "sat", "sun"]
RULE_ID = re.compile(r"[A-Za-z0-9_-]{1,64}")
DECIMAL = re.compile(r"-?[0-9]+(\.[0-9]+)?")
MISSING = object()


class PolicyError(ValueError):
    pass


def validate(policy: dict) -> None:
    if policy.get("default", "deny") not in ("allow", "deny"):
        raise PolicyError(f"bad default {policy.get('default')!r}")
    seen = set()
    for r in policy.get("rules", []):
        if not isinstance(r.get("id"), str) or not RULE_ID.fullmatch(r["id"]):
            raise PolicyError(f"missing or malformed id {r.get('id')!r}")
        if r["id"] in seen:
            raise PolicyError(f"duplicate id {r['id']!r}")
        seen.add(r["id"])
        if r.get("on") not in SUBJECTS or r.get("effect") not in EFFECTS:
            raise PolicyError(f"{r['id']}: bad on or effect")
        for c in r.get("where", []):
            if c.get("op") not in TEXT_OPS and c.get("op") not in NUM_OPS:
                raise PolicyError(f"{r['id']}: unknown op {c.get('op')!r}")


def lookup(request: dict, path: str):
    v = request
    for part in path.split("."):
        if not isinstance(v, dict) or part not in v:
            return MISSING
        v = v[part]
    return v


def number(v):
    if isinstance(v, bool):
        return None
    if isinstance(v, (int, float)):
        return float(v)
    if isinstance(v, str) and DECIMAL.fullmatch(v):
        return float(v)
    return None


def glob(pattern: str, s: str) -> bool:
    # Not fnmatch: no [...] classes and no case folding, so every engine agrees (spec/rules, Conditions)
    rx = "".join(".*" if ch == "*" else "." if ch == "?" else re.escape(ch) for ch in pattern)
    return re.fullmatch(rx, s, re.DOTALL) is not None


def holds(c: dict, request: dict) -> bool:
    v, want, op = lookup(request, c["field"]), c["value"], c["op"]
    if op in NUM_OPS:
        n = number(v)
        return n is not None and NUM_OPS[op](n, float(want))
    s = v if isinstance(v, str) else None
    positive = {"is": "is", "is_not": "is", "contains": "contains", "not_contains": "contains"}.get(op, op)
    hit = s is not None and {"is": lambda: s == want, "contains": lambda: want in s, "matches": lambda: glob(want, s),
                             "in": lambda: s in want}[positive]()
    return not hit if op in ("is_not", "not_contains") else hit


def in_window(when: dict, at: float) -> bool:
    t = datetime.fromtimestamp(at, timezone.utc).astimezone(ZoneInfo(when.get("tz", "UTC")))
    minute = t.hour * 60 + t.minute
    start = int(when["from"][:2]) * 60 + int(when["from"][3:]) if "from" in when else 0
    end = int(when["to"][:2]) * 60 + int(when["to"][3:]) if "to" in when else 24 * 60
    day = t.weekday()
    if end > start:
        inside = start <= minute < end
    elif minute >= start:  # overnight, before midnight: the window started today
        inside = True
    else:  # overnight, after midnight: it started yesterday
        inside, day = minute < end, (day - 1) % 7
    return inside and ("days" not in when or DAYS[day] in when["days"])


def matches(rule: dict, request: dict) -> bool:
    return (rule["on"] == request.get("on")
            and (not rule.get("agents") or request.get("agent") in rule["agents"])
            and all(holds(c, request) for c in rule.get("where", []))
            and ("when" not in rule or in_window(rule["when"], request["at"])))


def over_limit(rule: dict, request: dict, history: list) -> bool:
    lim = rule["limit"]
    key = [lookup(request, p) for p in lim.get("per", [])]
    past = [h for h in history if h.get("agent") == request.get("agent") and h.get("on") == rule["on"]
            and h["at"] > request["at"] - lim["window_s"] and all(holds(c, h) for c in rule.get("where", []))
            and [lookup(h, p) for p in lim.get("per", [])] == key]
    if "sum" not in lim:
        return len(past) + 1 > lim["max"]
    amount = number(lookup(request, lim["sum"]))
    if amount is None or amount < 0:  # can't be checked, or would lower the total
        return True
    return sum(number(lookup(h, lim["sum"])) or 0.0 for h in past) + amount > lim["max"]


def decide(policy: dict, request: dict, history: list = (), approved: bool = False) -> dict:
    validate(policy)
    if request.get("agent") in policy.get("quarantined", []):
        return {"decision": "deny", "rule": None, "reason": f"{request.get('agent')} is quarantined"}
    hits = sorted((r for r in policy.get("rules", []) if matches(r, request)), key=lambda r: r["id"])
    out = lambda decision, r: {"decision": decision, "rule": r["id"], "reason": r.get("reason")}
    for r in hits:
        if r["effect"] == "deny":
            return out("deny", r)
    passing = [r for r in hits if r["effect"] in ("allow", "approve")]
    for r in passing:
        if "limit" in r and over_limit(r, request, list(history)):
            return out("rate_limited", r)
    if not approved:
        for r in passing:
            if r["effect"] == "approve":
                return out("require_approval", r)
    if passing:
        return out("allow", passing[0])
    return {"decision": policy.get("default", "deny"), "rule": None, "reason": None}


def run_breach(run: dict, limits: dict):
    """spec/rules, Run limits: the checkpoint check."""
    if limits["budget_usd"] is not None and round(run["spent_usd"], 9) > limits["budget_usd"]:
        return "budget_exceeded"
    if run["iterations"] > limits["max_iterations"]:
        return "iteration_limit"
    if run["duration_ms"] > limits["max_duration_ms"]:
        return "time_limit"
    if limits["max_tokens"] > 0 and run["tokens_used"] > limits["max_tokens"]:
        return "token_limit"
    return None


def guard(run: dict, limits: dict, amount: float) -> str:
    """spec/rules, Run limits: 'allow' (and reserves the amount in `run`) or the breach that refuses it."""
    if run.get("stopped"):
        return "stopped_by_user"
    breach = run.get("breach") or run_breach(run, limits)
    if breach:
        return breach
    if limits["budget_usd"] is not None and round(run["spent_usd"] + run["reserved_usd"] + amount, 9) > limits["budget_usd"]:
        return "budget_exceeded"
    run["reserved_usd"] += amount
    return "allow"


def check_run_case(suite: dict, case: dict) -> dict:
    limits, state = case.get("limits", suite.get("limits")), dict(case["run"])
    if case["check"] == "checkpoint":
        return {"breach": run_breach(state, limits)}
    return {"results": [guard(state, limits, a) for a in case["amounts"]]}


def schema_errors(policy: dict) -> list:
    """Schema problems with a policy; None if jsonschema isn't installed (CI installs it)."""
    try:
        import jsonschema
    except ImportError:
        return None
    schema = json.loads((Path(__file__).parent.parent / "spec" / "rules" / "schema.json").read_text(encoding="utf-8"))
    return [e.message for e in jsonschema.Draft202012Validator(schema).iter_errors(policy)]


def run(cases_dir: Path) -> int:
    failed = total = 0
    for f in sorted(cases_dir.glob("*.json")):
        suite = json.loads(f.read_text(encoding="utf-8"))
        for case in suite["cases"]:
            total += 1
            want = case["expect"]
            if suite.get("kind") == "run":
                got = check_run_case(suite, case)
            else:
                policy = case.get("policy", suite.get("policy"))
                try:
                    got = decide(policy, case["request"], case.get("history", []), case.get("approved", False))
                except PolicyError as e:
                    got = {"error": "invalid_policy", "detail": str(e)}
                # The schema must agree with the engine: a valid policy passes it, an invalid one fails it, except a
                # duplicate id, which JSON Schema can't express
                errs = schema_errors(policy)
                if errs is not None and bool(errs) != ("error" in got and not got["detail"].startswith("duplicate")):
                    got, want = {**got, "schema": errs}, {**want, "schema": "agrees with the engine"}
            if any(got.get(k) != v for k, v in want.items()):
                failed += 1
                print(f"FAIL {f.name}: {case['name']}\n  want {want}\n  got  {got}")
    print(f"{total - failed}/{total} conformance cases pass")
    return 1 if failed or not total else 0


if __name__ == "__main__":
    sys.exit(run(Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).parent.parent / "spec" / "conformance"))
