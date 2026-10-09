#!/usr/bin/env python3
"""Safe bridge between generated Xray JSON and the 3x-ui SQLite model."""
import argparse
import hashlib
import json
import os
import shutil
import sqlite3
import sys
import tempfile
import time

DB = os.environ.get("XUI_DB_PATH", "/etc/x-ui/x-ui.db")
RUNTIME = os.environ.get("XUI_CONFIG_PATH", "/usr/local/x-ui/bin/config.json")


def canonical(value):
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def revision(value):
    return hashlib.sha256(canonical(value).encode()).hexdigest()


def load_runtime():
    with open(RUNTIME, encoding="utf-8") as handle:
        value = json.load(handle)
    if not isinstance(value, dict) or not isinstance(value.get("inbounds"), list):
        raise ValueError("Xray config must be an object with an inbounds array")
    return value


def read():
    value = load_runtime()
    print(json.dumps({"config": value, "revision": revision(value)}, ensure_ascii=False))


def write(expected):
    value = json.load(sys.stdin)
    if not isinstance(value, dict) or not isinstance(value.get("inbounds"), list):
        raise ValueError("Xray config must be an object with an inbounds array")
    current = load_runtime()
    actual = revision(current)
    if actual != expected:
        raise RuntimeError("revision conflict: config changed on node")
    incoming = value["inbounds"]
    tags = [str(item.get("tag") or "") for item in incoming if isinstance(item, dict)]
    if not tags or len(tags) != len(set(tags)):
        raise ValueError("every inbound must have a unique non-empty tag")

    stamp = time.strftime("%Y%m%d-%H%M%S", time.gmtime())
    backup_dir = "/var/lib/palm-agent/backups"
    os.makedirs(backup_dir, mode=0o700, exist_ok=True)
    db_backup = os.path.join(backup_dir, "x-ui-%s.db" % stamp)
    runtime_backup = os.path.join(backup_dir, "config-%s.json" % stamp)
    source = sqlite3.connect("file:%s?mode=ro" % DB, uri=True)
    backup = sqlite3.connect(db_backup)
    try:
        source.backup(backup)
    finally:
        backup.close()
        source.close()
    shutil.copy2(RUNTIME, runtime_backup)

    conn = sqlite3.connect(DB, timeout=15)
    conn.row_factory = sqlite3.Row
    try:
        conn.execute("BEGIN IMMEDIATE")
        rows = {row["tag"]: row for row in conn.execute("SELECT * FROM inbounds")}
        missing = set(rows) - set(tags)
        if missing:
            raise ValueError("managed 3x-ui inbounds may not be removed: %s" % sorted(missing))
        for item in incoming:
            tag = item["tag"]
            if tag not in rows:
                continue
            settings = item.get("settings", {})
            stream = item.get("streamSettings", {})
            sniffing = item.get("sniffing", {})
            # Clients live inside settings; editing the JSON intentionally updates them,
            # while traffic counters remain in their dedicated tables.
            conn.execute(
                "UPDATE inbounds SET listen=?,port=?,protocol=?,settings=?,stream_settings=?,sniffing=? WHERE tag=?",
                (item.get("listen", ""), int(item.get("port", 0)), item.get("protocol", ""),
                 canonical(settings), canonical(stream), canonical(sniffing), tag),
            )
        template = dict(value)
        template["inbounds"] = [item for item in incoming if item["tag"] not in rows]
        changed = conn.execute(
            "UPDATE settings SET value=? WHERE key='xrayTemplateConfig'", (canonical(template),)
        ).rowcount
        if not changed:
            raise RuntimeError("3x-ui setting xrayTemplateConfig is missing")
        conn.commit()
    except Exception:
        conn.rollback()
        raise
    finally:
        conn.close()
    print(json.dumps({"revision": revision(value), "backup": os.path.basename(db_backup)}))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=("read", "write"))
    parser.add_argument("--expected-revision", default="")
    args = parser.parse_args()
    if args.command == "read":
        read()
    else:
        if not args.expected_revision:
            raise ValueError("expected revision is required")
        write(args.expected_revision)


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print(str(exc), file=sys.stderr)
        raise SystemExit(1)
