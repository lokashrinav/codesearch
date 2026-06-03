"""Experiment 9: Python codebase indexer.
Tests whether the same architecture (raw parser + SQLite + LLM walker)
works for Python codebases.

Uses Python's ast module (no type checker, no build system).
Extracts: classes, functions, methods, attributes, calls, imports.
"""

import ast
import hashlib
import json
import os
import sqlite3
import sys
import time
from pathlib import Path


def hash_id(s: str) -> int:
    return int(hashlib.sha256(s.encode()).hexdigest()[:15], 16)


def index_python_repo(root_dir: str, db_path: str) -> dict:
    """Index a Python repository into SQLite."""
    os.remove(db_path) if os.path.exists(db_path) else None

    conn = sqlite3.connect(db_path)
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA synchronous=NORMAL")

    conn.execute("""CREATE TABLE idents (
        id INTEGER PRIMARY KEY, name TEXT, qualname TEXT,
        pkg TEXT, kind TEXT, file TEXT, line INTEGER
    )""")
    conn.execute("""CREATE TABLE edges (
        src INTEGER, dst INTEGER, kind TEXT
    )""")
    conn.execute("""CREATE TABLE annotations (
        file TEXT, line INTEGER, text TEXT, near_type TEXT
    )""")

    stats = {"files": 0, "idents": 0, "edges": 0}
    t0 = time.time()

    for dirpath, dirnames, filenames in os.walk(root_dir):
        # Skip common non-source directories
        dirnames[:] = [d for d in dirnames if d not in {
            '.git', '__pycache__', 'node_modules', '.venv', 'venv',
            '.tox', '.eggs', 'dist', 'build', '.mypy_cache'
        }]

        for fname in filenames:
            if not fname.endswith('.py') or fname.startswith('test_'):
                continue

            fpath = os.path.join(dirpath, fname)
            rel_path = os.path.relpath(fpath, root_dir)

            try:
                with open(fpath, 'r', encoding='utf-8', errors='replace') as f:
                    source = f.read()
                tree = ast.parse(source, filename=rel_path)
            except SyntaxError:
                continue

            stats["files"] += 1
            module = rel_path.replace(os.sep, '.').replace('.py', '')

            for node in ast.walk(tree):
                # Classes
                if isinstance(node, ast.ClassDef):
                    qn = f"{module}.{node.name}"
                    cid = hash_id(qn)
                    conn.execute("INSERT OR IGNORE INTO idents VALUES (?,?,?,?,?,?,?)",
                                (cid, node.name, qn, module, "class", rel_path, node.lineno))
                    stats["idents"] += 1

                    # Methods and attributes
                    for item in node.body:
                        if isinstance(item, ast.FunctionDef) or isinstance(item, ast.AsyncFunctionDef):
                            mqn = f"{qn}.{item.name}"
                            mid = hash_id(mqn)
                            conn.execute("INSERT OR IGNORE INTO idents VALUES (?,?,?,?,?,?,?)",
                                        (mid, item.name, mqn, module, "method", rel_path, item.lineno))
                            conn.execute("INSERT INTO edges VALUES (?,?,?)", (cid, mid, "has_method"))
                            stats["idents"] += 1
                            stats["edges"] += 1

                            # Extract calls and attribute accesses from method body
                            _extract_body(conn, mid, item, module, qn, stats)

                        elif isinstance(item, ast.Assign):
                            for target in item.targets:
                                if isinstance(target, ast.Name):
                                    aqn = f"{qn}.{target.id}"
                                    aid = hash_id(aqn)
                                    conn.execute("INSERT OR IGNORE INTO idents VALUES (?,?,?,?,?,?,?)",
                                                (aid, target.id, aqn, module, "attr", rel_path, item.lineno))
                                    conn.execute("INSERT INTO edges VALUES (?,?,?)", (cid, aid, "has_attr"))
                                    stats["idents"] += 1
                                    stats["edges"] += 1

                    # Base classes
                    for base in node.bases:
                        if isinstance(base, ast.Name):
                            base_id = hash_id(f"{module}.{base.id}")
                            conn.execute("INSERT INTO edges VALUES (?,?,?)", (cid, base_id, "inherits"))
                            stats["edges"] += 1
                        elif isinstance(base, ast.Attribute):
                            base_name = _get_attr_name(base)
                            if base_name:
                                base_id = hash_id(base_name)
                                conn.execute("INSERT INTO edges VALUES (?,?,?)", (cid, base_id, "inherits"))
                                stats["edges"] += 1

                    # Decorators
                    for dec in node.decorator_list:
                        dec_name = _get_decorator_name(dec)
                        if dec_name:
                            conn.execute("INSERT INTO annotations VALUES (?,?,?,?)",
                                        (rel_path, node.lineno, f"@{dec_name}", node.name))

                # Module-level functions
                elif isinstance(node, ast.FunctionDef) or isinstance(node, ast.AsyncFunctionDef):
                    if not _is_nested(node, tree):
                        qn = f"{module}.{node.name}"
                        fid = hash_id(qn)
                        conn.execute("INSERT OR IGNORE INTO idents VALUES (?,?,?,?,?,?,?)",
                                    (fid, node.name, qn, module, "func", rel_path, node.lineno))
                        stats["idents"] += 1
                        _extract_body(conn, fid, node, module, "", stats)

                # Imports
                elif isinstance(node, ast.Import):
                    for alias in node.names:
                        imp_id = hash_id(alias.name)
                        mod_id = hash_id(module)
                        conn.execute("INSERT OR IGNORE INTO idents VALUES (?,?,?,?,?,?,?)",
                                    (imp_id, alias.name.split('.')[-1], alias.name, alias.name, "module", "", 0))
                        conn.execute("INSERT INTO edges VALUES (?,?,?)", (mod_id, imp_id, "imports"))
                        stats["edges"] += 1

                elif isinstance(node, ast.ImportFrom):
                    if node.module:
                        for alias in node.names:
                            full_name = f"{node.module}.{alias.name}"
                            imp_id = hash_id(full_name)
                            mod_id = hash_id(module)
                            conn.execute("INSERT OR IGNORE INTO idents VALUES (?,?,?,?,?,?,?)",
                                        (imp_id, alias.name, full_name, node.module, "import", rel_path, node.lineno))
                            conn.execute("INSERT INTO edges VALUES (?,?,?)", (mod_id, imp_id, "imports"))
                            stats["edges"] += 1

    # Create indexes after bulk insert
    conn.execute("CREATE INDEX idx_name ON idents(name)")
    conn.execute("CREATE INDEX idx_qn ON idents(qualname)")
    conn.execute("CREATE INDEX idx_esrc ON edges(src)")
    conn.execute("CREATE INDEX idx_edst ON edges(dst)")
    conn.commit()

    stats["time"] = time.time() - t0
    return stats


def _extract_body(conn, func_id, func_node, module, class_name, stats):
    """Extract calls and attribute accesses from a function body."""
    for node in ast.walk(func_node):
        # Method/function calls
        if isinstance(node, ast.Call):
            callee_name = None
            if isinstance(node.func, ast.Attribute):
                callee_name = node.func.attr
            elif isinstance(node.func, ast.Name):
                callee_name = node.func.id

            if callee_name:
                callee_id = hash_id(callee_name)
                conn.execute("INSERT INTO edges VALUES (?,?,?)", (func_id, callee_id, "calls"))
                stats["edges"] += 1

        # Attribute accesses (self.x, obj.method)
        elif isinstance(node, ast.Attribute):
            attr_name = node.attr
            if isinstance(node.value, ast.Name):
                if node.value.id == "self" and class_name:
                    target_qn = f"{class_name}.{attr_name}"
                else:
                    target_qn = f"{node.value.id}.{attr_name}"
                target_id = hash_id(target_qn)
                conn.execute("INSERT INTO edges VALUES (?,?,?)", (func_id, target_id, "accesses"))
                stats["edges"] += 1


def _get_attr_name(node):
    """Get dotted name from an Attribute node."""
    parts = []
    while isinstance(node, ast.Attribute):
        parts.append(node.attr)
        node = node.value
    if isinstance(node, ast.Name):
        parts.append(node.id)
    return '.'.join(reversed(parts)) if parts else None


def _get_decorator_name(node):
    if isinstance(node, ast.Name):
        return node.id
    elif isinstance(node, ast.Attribute):
        return _get_attr_name(node)
    elif isinstance(node, ast.Call):
        return _get_decorator_name(node.func)
    return None


def _is_nested(func_node, tree):
    """Check if a function is nested inside a class."""
    for node in ast.walk(tree):
        if isinstance(node, ast.ClassDef):
            for item in node.body:
                if item is func_node:
                    return True
    return False


def search_and_build_subgraph(conn, query, max_seeds=25):
    """Search the index and build a subgraph for LLM consumption."""
    terms = [t for t in query.lower().split() if len(t) >= 3 and t not in {
        'the', 'and', 'for', 'not', 'was', 'did', 'with', 'from', 'that', 'this', 'when'
    }]

    seeds = []
    seen = set()

    for term in terms:
        cursor = conn.execute(
            "SELECT id, name, qualname, kind, file, line FROM idents WHERE LOWER(qualname) LIKE ? LIMIT 15",
            (f"%{term}%",))
        for row in cursor:
            if row[0] not in seen:
                seen.add(row[0])
                seeds.append(row)

    # Build context
    lines = ["## Code Graph\n"]
    for i, (sid, name, qn, kind, file, line) in enumerate(seeds[:max_seeds]):
        lines.append(f"### [{kind}] {qn}")
        lines.append(f"  File: {file}:{line}")

        cursor = conn.execute(
            "SELECT i.qualname, i.kind, e.kind FROM edges e JOIN idents i ON e.dst = i.id WHERE e.src = ? LIMIT 8",
            (sid,))
        for tqn, tk, ek in cursor:
            lines.append(f"  -> [{tk}] {tqn} ({ek})")

        cursor = conn.execute(
            "SELECT i.qualname, i.kind, e.kind FROM edges e JOIN idents i ON e.src = i.id WHERE e.dst = ? LIMIT 8",
            (sid,))
        for sqn, sk, ek in cursor:
            lines.append(f"  <- [{sk}] {sqn} ({ek})")

        lines.append("")

    # Annotations
    lines.append("### Decorators/Annotations")
    for sid, name, qn, kind, file, line in seeds[:max_seeds]:
        if kind == "class":
            cursor = conn.execute("SELECT file, line, text FROM annotations WHERE near_type = ?", (name,))
            for f, l, t in cursor:
                lines.append(f"  {name}: {t} at {f}:{l}")

    return "\n".join(lines)


if __name__ == "__main__":
    if len(sys.argv) < 3:
        print("Usage: extract_python.py <directory> <query>")
        sys.exit(1)

    root_dir = sys.argv[1]
    query = sys.argv[2]
    db_path = "experiment9_python.db"

    print(f"=== Experiment 9: Python Codebase Search ===")
    print(f"Directory: {root_dir}")
    print(f"Query: {query!r}\n")

    stats = index_python_repo(root_dir, db_path)
    print(f"Indexed: {stats['files']} files, {stats['idents']} idents, {stats['edges']} edges in {stats['time']:.2f}s\n")

    conn = sqlite3.connect(db_path)
    subgraph = search_and_build_subgraph(conn, query)
    print(f"Subgraph: {len(subgraph)} chars\n")
    print(subgraph[:3000])

    # LLM trace
    api_key = os.environ.get("ANTHROPIC_API_KEY", "")
    if api_key:
        import urllib.request
        body = json.dumps({
            "model": "claude-sonnet-4-6",
            "max_tokens": 1500,
            "messages": [{"role": "user", "content": f"""You are a code comprehension engine analyzing a Python codebase.

Developer's problem: {query!r}

Below is a subgraph of relevant code symbols:

{subgraph}

Trace the causal chain. For each step give the symbol, file:line, why it's relevant, and how it connects to the next step."""}]
        }).encode()

        req = urllib.request.Request("https://api.anthropic.com/v1/messages", data=body)
        req.add_header("Content-Type", "application/json")
        req.add_header("x-api-key", api_key)
        req.add_header("anthropic-version", "2023-06-01")

        try:
            resp = urllib.request.urlopen(req, timeout=60)
            result = json.loads(resp.read())
            if result.get("content"):
                print("\n=== LLM TRACE ===\n")
                print(result["content"][0]["text"])
        except Exception as e:
            print(f"LLM error: {e}")

    conn.close()
    os.remove(db_path)
    print(f"\n=== DONE ===")
