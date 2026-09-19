#!/usr/bin/env python3
"""Cross-check every webui API call against the registered route tables.

Extracts (version, method, path) triples from the webui request call sites and
(METHOD, path) pairs from the Go route tables, then fails on either drift:

  * no route registered for the path            -> 404 at runtime
  * path registered, but not for that verb      -> 405 at runtime

The verb check is deliberately strict. An earlier revision accepted a
path-only match as "covered", which is exactly how a frontend PUT against a
PATCH-only route shipped green and broke the page: the path existed, the verb
did not.

Template interpolations are normalised to :param so they match route patterns,
and a template with two literal alternatives (`${on ? "enable" : "disable"}`)
expands into both concrete paths.
"""

import pathlib
import re
import sys

ROOT = pathlib.Path(".")
WEBUI = ROOT / "webui/src"
ROUTE_FILES = sorted((ROOT / "internal/api").glob("routes*.go"))

METHOD_CONST = {
    "MethodGet": "GET",
    "MethodPost": "POST",
    "MethodPut": "PUT",
    "MethodPatch": "PATCH",
    "MethodDelete": "DELETE",
    "MethodHead": "HEAD",
    "MethodOptions": "OPTIONS",
}

ROUTE_RE = re.compile(r'a\.add\(http\.Method(\w+),\s*"([^"]+)"')
METHOD_RE = re.compile(r"""method:\s*['"]([A-Z]+)['"]""")
VERSION_RE = re.compile(r"""apiVersion:\s*['"](\w+)['"]""")
FORM_VERB_RE = re.compile(r"""[,(]\s*['"](POST|PUT)['"]\s*[,)]""")

CALL_RE = re.compile(r"\b(?:this\.request|this\.requestForm|apiRequest|apiRequestForm)\s*(?:<[^>]*>)?\s*\(")
FETCH_RE = re.compile(r"\bfetch\s*\(\s*([`\"'])")
WS_RE = re.compile(r"\bnew\s+(?:WebSocket|EventSource)\s*\(\s*([`\"'])")


def normalise(path: str) -> str:
    # Route params carry descriptive names (:uid vs :log_id); collapse them.
    resolved = re.sub(r":\w+", ":param", path)
    resolved = re.sub(r":param[^/]*", ":param", resolved)
    return resolved.split("?", 1)[0].rstrip("/") or "/"


def interpolations(path: str) -> list[tuple[int, int, str]]:
    """Locate `${...}` spans, brace-aware so nested `${}` inside is kept whole."""
    spans: list[tuple[int, int, str]] = []
    i, n = 0, len(path)
    while i < n:
        if path[i] == "$" and i + 1 < n and path[i + 1] == "{":
            depth = 1
            j = i + 2
            while j < n and depth:
                if path[j] == "{":
                    depth += 1
                elif path[j] == "}":
                    depth -= 1
                j += 1
            spans.append((i, j, path[i:j]))
            i = j
        else:
            i += 1
    return spans


def expand(path: str) -> list[str]:
    """Expand a template literal into the concrete paths it can produce.

    Three shapes occur in the client:
      * `${cond ? "enable" : "disable"}` -> two registered routes
      * `${suffix ? `?${q}` : ""}`       -> an optional query string, no path text
      * `${uid}`                         -> one route parameter
    A ternary is detected by its ` ? ` / ` : ` shape rather than a regex, because
    the query-string form embeds a nested template that no flat pattern matches.
    """
    slots: list[tuple[str, list[str]]] = []
    for start, end, token in interpolations(path):
        if " ? " in token and " : " in token:
            branches = [lit for lit in string_literals(token)]
            # Both branches are a query suffix or empty: the slot contributes no
            # path text, it only decides whether a query string is appended.
            if branches and all(branch == "" or branch.startswith("?") for branch in branches):
                options = [""]
            elif branches:
                options = [branch for branch in branches if branch and not branch.startswith("?")] or [":param"]
            else:
                options = [":param"]
        elif start == 0 or path[start - 1] != "/":
            # Glued to the end of a segment (`/tickets${suffix}`); it can only
            # append a query string, never introduce a path segment.
            options = [""]
        else:
            options = [":param"]
        slots.append((token, options))

    variants = [path]
    for token, options in slots:
        nxt = []
        for variant in variants:
            for option in options:
                nxt.append(variant.replace(token, option, 1))
        variants = nxt
    return [normalise(v) for v in variants]


def balanced(text: str, start: int) -> str:
    """Return the argument list starting at the open paren at `start`."""
    depth = 0
    i, n = start, len(text)
    while i < n:
        ch = text[i]
        if ch in "\"'`":
            quote = ch
            i += 1
            while i < n:
                if text[i] == "\\":
                    i += 2
                    continue
                if quote == "`" and text[i] == "$" and i + 1 < n and text[i + 1] == "{":
                    brace = 1
                    i += 2
                    while i < n and brace:
                        if text[i] == "{":
                            brace += 1
                        elif text[i] == "}":
                            brace -= 1
                        i += 1
                    continue
                if text[i] == quote:
                    i += 1
                    break
                i += 1
            continue
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
            if depth == 0:
                return text[start : i + 1]
        i += 1
    return text[start:]


def string_literals(text: str) -> list[str]:
    """Extract every string/template literal, tolerating quotes inside ${...}."""
    out: list[str] = []
    i, n = 0, len(text)
    while i < n:
        ch = text[i]
        if ch not in "\"'`":
            i += 1
            continue
        quote = ch
        buf: list[str] = []
        i += 1
        while i < n:
            c = text[i]
            if c == "\\":
                buf.append(text[i : i + 2])
                i += 2
                continue
            if quote == "`" and c == "$" and i + 1 < n and text[i + 1] == "{":
                buf.append("${")
                brace = 1
                i += 2
                while i < n and brace:
                    inner = text[i]
                    if inner == "{":
                        brace += 1
                    elif inner == "}":
                        brace -= 1
                        if brace == 0:
                            buf.append("}")
                            i += 1
                            break
                    buf.append(inner)
                    i += 1
                continue
            if c == quote:
                i += 1
                break
            buf.append(c)
            i += 1
        out.append("".join(buf))
    return out


def load_routes() -> dict[str, dict[str, set[str]]]:
    routes: dict[str, dict[str, set[str]]] = {"v1": {}, "v2": {}}
    for file in ROUTE_FILES:
        text = file.read_text(encoding="utf-8")
        for const, raw in ROUTE_RE.findall(text):
            method = METHOD_CONST.get("Method" + const)
            if not method:
                continue
            for version in ("v1", "v2"):
                prefix = f"/api/{version}"
                if raw.startswith(prefix):
                    routes[version].setdefault(normalise(raw[len(prefix) :]), set()).add(method)
    return routes


def load_calls() -> tuple[list[tuple[str, str, str, str]], list[str]]:
    calls: list[tuple[str, str, str, str]] = []
    unresolved: list[str] = []
    for file in sorted(WEBUI.rglob("*.ts")) + sorted(WEBUI.rglob("*.tsx")):
        text = file.read_text(encoding="utf-8")
        rel = file.relative_to(ROOT)

        for match in CALL_RE.finditer(text):
            arglist = balanced(text, text.index("(", match.end() - 1))
            line = text.count("\n", 0, match.start()) + 1
            version_match = VERSION_RE.search(arglist)
            version = version_match.group(1) if version_match else "v2"
            is_form = "requestForm" in match.group(0)
            if method_match := METHOD_RE.search(arglist):
                method = method_match.group(1)
            elif is_form and (verb := FORM_VERB_RE.search(arglist)):
                method = verb.group(1)
            elif is_form:
                method = "POST"
            else:
                # fetch() and friends default to GET when no verb is given.
                method = "GET"

            for raw in string_literals(arglist):
                if not raw.startswith("/"):
                    continue
                if raw.startswith("/api/"):
                    if not raw.startswith(f"/api/{version}"):
                        continue
                    body = raw[len(f"/api/{version}") :]
                else:
                    body = raw
                for variant in expand(body):
                    calls.append((version, method, variant, f"{rel}:{line}"))
                break
            else:
                # Delegating wrappers (`request(endpoint, options, extra)`) have
                # no literal; they are not call sites.
                unresolved.append(f"{rel}:{line}  {method}  {arglist[:80]!r}")

        for regex in (FETCH_RE, WS_RE):
            for match in regex.finditer(text):
                quote = match.group(1)
                rest = text[match.end() - 1 : match.end() + 400]
                lit = re.match(re.escape(quote) + r"(/[^" + quote + r"]*)", rest)
                if not lit:
                    continue
                raw = lit.group(1)
                if not raw.startswith("/api/v2"):
                    continue
                line = text.count("\n", 0, match.start()) + 1
                window = text[match.start() : match.end() + 300]
                method_match = METHOD_RE.search(window)
                for variant in expand(raw[len("/api/v2") :]):
                    calls.append(("v2", method_match.group(1) if method_match else "GET", variant, f"{rel}:{line}"))
    return calls, unresolved


def main() -> int:
    routes = load_routes()
    calls, unresolved = load_calls()
    print(
        f"v2 routes: {sum(len(m) for m in routes['v2'].values())}   "
        f"v1 routes: {sum(len(m) for m in routes['v1'].values())}   "
        f"call sites: {len(calls)}"
    )
    if unresolved:
        print(f"({len(unresolved)} delegating wrappers without a literal path)")

    missing: list[tuple[str, str, str, str]] = []
    mismatch: list[tuple[str, str, str, list[str], str]] = []
    for version, method, path, where in calls:
        methods = routes[version].get(path)
        if methods is None:
            missing.append((version, method, path, where))
        elif method not in methods:
            mismatch.append((version, method, path, sorted(methods), where))

    if mismatch:
        print(f"\n{len(mismatch)} call sites whose verb is not registered for the path (405 at runtime):")
        for version, method, path, allowed, where in mismatch:
            print(f"  {where}  [{version}] {method} {path}   (registered: {','.join(allowed)})")
    if missing:
        print(f"\n{len(missing)} call sites with no route for the path (404 at runtime):")
        for version, method, path, where in missing:
            print(f"  {where}  [{version}] {method} {path}")

    if not mismatch and not missing:
        print("all extracted frontend calls resolve to a registered route with the same verb")
        return 0
    return 1


if __name__ == "__main__":
    sys.exit(main())
