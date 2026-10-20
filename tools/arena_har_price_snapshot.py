"""Extract public Arena price tables from a HAR without copying credentials.

Only complete JSON objects are extracted, including from truncated RSC bodies.
These are published reference prices/statistics, not charges for the captured chat.
"""

import argparse
import csv
import hashlib
import json
import re
from pathlib import Path
from urllib.parse import urlsplit


def extract(source):
    raw = source.read_bytes()
    entries = json.loads(raw.decode("utf-8-sig"))["log"]["entries"]
    decoder = json.JSONDecoder()
    prices, task_costs, sources = {}, {}, []
    for index, entry in enumerate(entries):
        url = urlsplit(entry["request"]["url"])
        if url.hostname != "arena.ai" or not url.path.startswith("/leaderboard"):
            continue
        body = (entry["response"].get("content") or {}).get("text", "")
        found = False
        for match in re.finditer(r"\{", body):
            try:
                obj, _ = decoder.raw_decode(body, match.start())
            except ValueError:
                continue
            if not isinstance(obj, dict):
                continue
            key = obj.get("modelKey") or obj.get("contenderName")
            if not isinstance(key, str):
                continue
            key = key.removeprefix("contenders/")
            if "inputPricePerMillion" in obj and "outputPricePerMillion" in obj:
                record = {
                    "model_key": key,
                    "display_name": obj.get("modelDisplayName") or obj.get("model") or key,
                    "organization": obj.get("modelOrganization"),
                    "input_price_per_million": obj["inputPricePerMillion"],
                    "output_price_per_million": obj["outputPricePerMillion"],
                }
                # Keep conflicting prices as separate observations, never overwrite.
                identity = json.dumps(record, sort_keys=True)
                saved = prices.setdefault(identity, dict(record, sources=[]))
            elif "medianUsd" in obj:
                record = {"model_key": key}
                for field in ("medianUsd", "p25Usd", "p95Usd", "meanUsd", "totalSampleCount", "pricedSampleCount"):
                    record[field] = obj.get(field)
                identity = json.dumps(record, sort_keys=True)
                saved = task_costs.setdefault(identity, dict(record, sources=[]))
            else:
                continue
            found = True
            ref = {"har_entry_index": index, "path": url.path, "captured_at": entry.get("startedDateTime")}
            if ref not in saved["sources"]:
                saved["sources"].append(ref)
        if found:
            sources.append({
                "har_entry_index": index,
                "path": url.path,
                "captured_at": entry.get("startedDateTime"),
                "response_body_note": entry.get("_responseBodyNote"),
            })
    records = sorted(prices.values(), key=lambda r: (r["model_key"], str(r["input_price_per_million"])))
    return {
        "schema_version": 1,
        "source_har_sha256": hashlib.sha256(raw).hexdigest(),
        "kind": "published_reference_prices_not_observed_chat_charges",
        "limitations": [
            "HAR leaderboard bodies are truncated; this is a partial snapshot.",
            "Null means unavailable, not zero.",
            "Prices shared by several models cannot uniquely identify a model.",
            "Captured chat streams lack response bodies and per-request token/cost usage.",
            "Per-task costs are leaderboard aggregate statistics, not this chat's bill.",
        ],
        "sources": sources,
        "model_prices": records,
        "task_cost_statistics": sorted(task_costs.values(), key=lambda r: r["model_key"]),
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("har", type=Path)
    parser.add_argument("output_dir", type=Path)
    args = parser.parse_args()
    result = extract(args.har)
    args.output_dir.mkdir(parents=True, exist_ok=True)
    (args.output_dir / "prices.json").write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    fields = ["model_key", "display_name", "organization", "input_price_per_million", "output_price_per_million", "source_paths", "captured_at"]
    with (args.output_dir / "prices.csv").open("w", encoding="utf-8-sig", newline="") as stream:
        writer = csv.DictWriter(stream, fieldnames=fields, extrasaction="ignore")
        writer.writeheader()
        for row in result["model_prices"]:
            writer.writerow(dict(row,
                source_paths=";".join(sorted({s["path"] for s in row["sources"]})),
                captured_at=";".join(sorted({s["captured_at"] for s in row["sources"] if s["captured_at"]})),
            ))
    print(json.dumps({"model_price_rows": len(result["model_prices"]), "task_cost_rows": len(result["task_cost_statistics"]), "source_responses": len(result["sources"])}, ensure_ascii=True))


if __name__ == "__main__":
    main()
