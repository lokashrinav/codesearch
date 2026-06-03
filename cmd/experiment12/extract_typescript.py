"""Experiment 12: TypeScript/JavaScript codebase indexer.
Tests whether the architecture generalizes to TypeScript.
Uses regex-based extraction (no AST parser dependency).
Extracts: classes, functions, methods, imports, type annotations, interfaces.
"""

import hashlib
import json
import os
import re
import sqlite3
import sys
import time
from pathlib import Path


def hash_id(s: str) -> int:
    return int(hashlib.sha256(s.encode()).hexdigest()[:15], 16)


# Regex patterns for TypeScript/JavaScript extraction
PATTERNS = {
    'class': re.compile(r'(?:export\s+)?(?:abstract\s+)?class\s+(\w+)(?:\s+extends\s+(\w+))?(?:\s+implements\s+([\w,\s]+))?'),
    'interface': re.compile(r'(?:export\s+)?interface\s+(\w+)(?:\s+extends\s+([\w,\s]+))?'),
    'function': re.compile(r'(?:export\s+)?(?:async\s+)?function\s+(\w+)'),
    'arrow_const': re.compile(r'(?:export\s+)?const\s+(\w+)\s*=\s*(?:async\s+)?\('),
    'method': re.compile(r'^\s+(?:async\s+)?(?:static\s+)?(?:get\s+|set\s+)?(\w+)\s*\(', re.MULTILINE),
    'property': re.compile(r'^\s+(?:readonly\s+)?(?:private\s+|protected\s+|public\s+)?(\w+)\s*[?!]?\s*:', re.MULTILINE),
    'import_from': re.compile(r'import\s+\{([^}]+)\}\s+from\s+[\'"]([^\'"]+)[\'"]'),
    'import_default': re.compile(r'import\s+(\w+)\s+from\s+[\'"]([^\'"]+)[\'"]'),
    'type_alias': re.compile(r'(?:export\s+)?type\s+(\w+)\s*='),
    'enum': re.compile(r'(?:export\s+)?(?:const\s+)?enum\s+(\w+)'),
    'decorator': re.compile(r'@(\w+)(?:\(([^)]*)\))?'),
}


def index_ts_repo(root_dir: str, db_path: str) -> dict:
    if os.path.exists(db_path):
        os.remove(db_path)

    conn = sqlite3.connect(db_path)
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA synchronous=NORMAL")
    conn.execute("CREATE TABLE idents (id INTEGER PRIMARY KEY, name TEXT, qualname TEXT, pkg TEXT, kind TEXT, file TEXT, line INTEGER)")
    conn.execute("CREATE TABLE edges (src INTEGER, dst INTEGER, kind TEXT)")
    conn.execute("CREATE TABLE annotations (file TEXT, line INTEGER, text TEXT, near_type TEXT)")

    stats = {"files": 0, "idents": 0, "edges": 0}
    t0 = time.time()

    skip_dirs = {'.git', 'node_modules', 'dist', 'build', '.next', 'coverage', '__tests__', '.turbo'}

    for dirpath, dirnames, filenames in os.walk(root_dir):
        dirnames[:] = [d for d in dirnames if d not in skip_dirs]

        for fname in filenames:
            if not (fname.endswith('.ts') or fname.endswith('.tsx') or fname.endswith('.js') or fname.endswith('.jsx')):
                continue
            if fname.endswith('.d.ts') or fname.endswith('.test.ts') or fname.endswith('.spec.ts'):
                continue

            fpath = os.path.join(dirpath, fname)
            rel_path = os.path.relpath(fpath, root_dir)

            try:
                with open(fpath, 'r', encoding='utf-8', errors='replace') as f:
                    content = f.read()
                    lines = content.split('\n')
            except Exception:
                continue

            stats["files"] += 1
            module = rel_path.replace(os.sep, '/').rsplit('.', 1)[0]

            current_class = None
            current_class_id = None

            for lineno, line in enumerate(lines, 1):
                # Classes
                m = PATTERNS['class'].search(line)
                if m:
                    name = m.group(1)
                    extends = m.group(2)
                    qn = f"{module}.{name}"
                    cid = hash_id(qn)
                    conn.execute("INSERT OR IGNORE INTO idents VALUES (?,?,?,?,?,?,?)",
                                (cid, name, qn, module, "class", rel_path, lineno))
                    stats["idents"] += 1
                    current_class = name
                    current_class_id = cid

                    if extends:
                        eid = hash_id(extends)
                        conn.execute("INSERT INTO edges VALUES (?,?,?)", (cid, eid, "extends"))
                        stats["edges"] += 1

                # Interfaces
                m = PATTERNS['interface'].search(line)
                if m:
                    name = m.group(1)
                    qn = f"{module}.{name}"
                    iid = hash_id(qn)
                    conn.execute("INSERT OR IGNORE INTO idents VALUES (?,?,?,?,?,?,?)",
                                (iid, name, qn, module, "interface", rel_path, lineno))
                    stats["idents"] += 1
                    current_class = name
                    current_class_id = iid

                # Functions
                m = PATTERNS['function'].search(line)
                if m:
                    name = m.group(1)
                    qn = f"{module}.{name}"
                    fid = hash_id(qn)
                    conn.execute("INSERT OR IGNORE INTO idents VALUES (?,?,?,?,?,?,?)",
                                (fid, name, qn, module, "func", rel_path, lineno))
                    stats["idents"] += 1
                    current_class = None

                # Arrow function consts
                m = PATTERNS['arrow_const'].search(line)
                if m and not current_class:
                    name = m.group(1)
                    qn = f"{module}.{name}"
                    fid = hash_id(qn)
                    conn.execute("INSERT OR IGNORE INTO idents VALUES (?,?,?,?,?,?,?)",
                                (fid, name, qn, module, "func", rel_path, lineno))
                    stats["idents"] += 1

                # Methods (inside class)
                if current_class:
                    m = PATTERNS['method'].search(line)
                    if m and m.group(1) not in ('if', 'for', 'while', 'switch', 'return', 'new', 'throw', 'catch', 'try'):
                        name = m.group(1)
                        qn = f"{module}.{current_class}.{name}"
                        mid = hash_id(qn)
                        conn.execute("INSERT OR IGNORE INTO idents VALUES (?,?,?,?,?,?,?)",
                                    (mid, name, qn, module, "method", rel_path, lineno))
                        if current_class_id:
                            conn.execute("INSERT INTO edges VALUES (?,?,?)", (current_class_id, mid, "has_method"))
                        stats["idents"] += 1
                        stats["edges"] += 1

                # Imports
                m = PATTERNS['import_from'].search(line)
                if m:
                    names = [n.strip().split(' as ')[0].strip() for n in m.group(1).split(',')]
                    source = m.group(2)
                    mod_id = hash_id(module)
                    for name in names:
                        if name:
                            imp_qn = f"{source}.{name}"
                            imp_id = hash_id(imp_qn)
                            conn.execute("INSERT OR IGNORE INTO idents VALUES (?,?,?,?,?,?,?)",
                                        (imp_id, name, imp_qn, source, "import", rel_path, lineno))
                            conn.execute("INSERT INTO edges VALUES (?,?,?)", (mod_id, imp_id, "imports"))
                            stats["idents"] += 1
                            stats["edges"] += 1

                # Type aliases
                m = PATTERNS['type_alias'].search(line)
                if m:
                    name = m.group(1)
                    qn = f"{module}.{name}"
                    tid = hash_id(qn)
                    conn.execute("INSERT OR IGNORE INTO idents VALUES (?,?,?,?,?,?,?)",
                                (tid, name, qn, module, "type", rel_path, lineno))
                    stats["idents"] += 1

                # Enums
                m = PATTERNS['enum'].search(line)
                if m:
                    name = m.group(1)
                    qn = f"{module}.{name}"
                    eid = hash_id(qn)
                    conn.execute("INSERT OR IGNORE INTO idents VALUES (?,?,?,?,?,?,?)",
                                (eid, name, qn, module, "enum", rel_path, lineno))
                    stats["idents"] += 1

                # Decorators
                m = PATTERNS['decorator'].search(line)
                if m:
                    conn.execute("INSERT INTO annotations VALUES (?,?,?,?)",
                                (rel_path, lineno, f"@{m.group(1)}", current_class or ""))

                # Track class end (simple heuristic: unindented closing brace)
                if line.strip() == '}' and not line.startswith(' ') and not line.startswith('\t'):
                    current_class = None
                    current_class_id = None

    conn.execute("CREATE INDEX idx_name ON idents(name)")
    conn.execute("CREATE INDEX idx_qn ON idents(qualname)")
    conn.execute("CREATE INDEX idx_esrc ON edges(src)")
    conn.execute("CREATE INDEX idx_edst ON edges(dst)")
    conn.commit()

    stats["time"] = time.time() - t0
    return stats


def search_and_trace(conn, query, api_key=""):
    terms = [t for t in query.lower().split() if len(t) >= 3 and t not in {
        'the', 'and', 'for', 'not', 'was', 'did', 'with', 'from', 'that', 'this', 'when'
    }]

    seeds = []
    seen = set()
    for term in terms:
        cursor = conn.execute(
            "SELECT id, name, qualname, kind, file, line FROM idents WHERE LOWER(qualname) LIKE ? OR LOWER(name) LIKE ? LIMIT 15",
            (f"%{term}%", f"%{term}%"))
        for row in cursor:
            if row[0] not in seen:
                seen.add(row[0])
                seeds.append(row)

    lines = ["## Code Graph\n"]
    for sid, name, qn, kind, file, line in seeds[:25]:
        lines.append(f"### [{kind}] {qn}")
        lines.append(f"  File: {file}:{line}")
        cursor = conn.execute("SELECT i.qualname, i.kind, e.kind FROM edges e JOIN idents i ON e.dst = i.id WHERE e.src = ? LIMIT 8", (sid,))
        for tqn, tk, ek in cursor:
            lines.append(f"  -> [{tk}] {tqn} ({ek})")
        cursor = conn.execute("SELECT i.qualname, i.kind, e.kind FROM edges e JOIN idents i ON e.src = i.id WHERE e.dst = ? LIMIT 8", (sid,))
        for sqn, sk, ek in cursor:
            lines.append(f"  <- [{sk}] {sqn} ({ek})")
        lines.append("")

    subgraph = "\n".join(lines)
    print(f"Subgraph: {len(subgraph)} chars, {len(seeds)} seeds\n")

    if api_key:
        import urllib.request
        body = json.dumps({
            "model": "claude-sonnet-4-6", "max_tokens": 1500,
            "messages": [{"role": "user", "content": f"You are analyzing a TypeScript/JavaScript codebase.\n\nDeveloper's problem: {query!r}\n\n{subgraph}\n\nTrace the causal chain. Give symbol, file:line, why relevant, how it connects."}]
        }).encode()
        req = urllib.request.Request("https://api.anthropic.com/v1/messages", data=body)
        req.add_header("Content-Type", "application/json")
        req.add_header("x-api-key", api_key)
        req.add_header("anthropic-version", "2023-06-01")
        try:
            resp = urllib.request.urlopen(req, timeout=60)
            result = json.loads(resp.read())
            if result.get("content"):
                print("=== LLM TRACE ===\n")
                print(result["content"][0]["text"])
        except Exception as e:
            print(f"LLM error: {e}")
    else:
        print(subgraph[:3000])


if __name__ == "__main__":
    if len(sys.argv) < 3:
        print("Usage: extract_typescript.py <directory> <query>")
        sys.exit(1)

    root_dir = sys.argv[1]
    query = sys.argv[2]
    db_path = "experiment12_ts.db"
    api_key = os.environ.get("ANTHROPIC_API_KEY", "")

    print(f"=== Experiment 12: TypeScript Search ===")
    print(f"Directory: {root_dir}")
    print(f"Query: {query!r}\n")

    stats = index_ts_repo(root_dir, db_path)
    print(f"Indexed: {stats['files']} files, {stats['idents']} idents, {stats['edges']} edges in {stats['time']:.2f}s\n")

    conn = sqlite3.connect(db_path)
    search_and_trace(conn, query, api_key)
    conn.close()

    try:
        os.remove(db_path)
    except Exception:
        pass

    print(f"\n=== DONE ===")
