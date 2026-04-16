#!/usr/bin/env python3
"""Aggregate per-(SDK × OP × PHASE) pressure JSON reports into a single
Markdown + JSON report for the DNS-cutting workflow.

Input layout (produced by the workflow):
    <results-dir>/
        phase-A/
            go-v2-get.json
            go-v2-put.json
        phase-C/
            go-v2-get.json
            go-v2-put.json
        dns_event.env   # optional: DNS_CUT_START=... etc

Writes:
    <out-dir>/report.md
    <out-dir>/report.json
    <out-dir>/summary.md     # smaller, suitable for GITHUB_STEP_SUMMARY

Usage:
    aggregate_pressure_report.py <results-dir> <out-dir>
"""
from __future__ import annotations

import json
import os
import sys
from pathlib import Path


def load_reports(results_dir: Path):
    """Walk phase-*/ dirs and load every *.json as a pressure report."""
    by_phase = {}
    for phase_dir in sorted(results_dir.glob("phase-*")):
        phase_label = phase_dir.name.removeprefix("phase-").upper()
        by_phase.setdefault(phase_label, [])
        for f in sorted(phase_dir.glob("*.json")):
            try:
                data = json.loads(f.read_text())
                data["_source_file"] = f.name
                by_phase[phase_label].append(data)
            except Exception as e:  # noqa: BLE001
                print(f"WARN: failed to load {f}: {e}", file=sys.stderr)
    return by_phase


def load_dns_event(results_dir: Path) -> dict:
    env_file = results_dir / "dns_event.env"
    event = {}
    if env_file.exists():
        for line in env_file.read_text().splitlines():
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            if "=" in line:
                k, v = line.split("=", 1)
                event[k.strip()] = v.strip()
    return event


def index_by_key(reports):
    """(sdk, op) -> report"""
    out = {}
    for r in reports:
        out[(r.get("sdk", "?"), r.get("op", "?"))] = r
    return out


def diff_pct(after: float, before: float) -> str:
    if before == 0:
        return "—" if after == 0 else "N/A (baseline=0)"
    pct = (after - before) / before * 100.0
    sign = "+" if pct >= 0 else ""
    return f"{sign}{pct:.1f}%"


def abs_diff(after: float, before: float, fmt: str = "{:+.1f}") -> str:
    return fmt.format(after - before)


def verdict_row(a_val: float, c_val: float, metric: str) -> str:
    if a_val == 0 and c_val == 0:
        return "no-data"
    # Heuristic thresholds for "notable difference"
    if metric == "error_rate":
        if abs(c_val - a_val) >= 0.01:  # >=1pp
            return "notable"
        return "comparable"
    if metric == "throughput":
        if a_val == 0:
            return "N/A"
        pct = abs(c_val - a_val) / a_val
        return "notable" if pct >= 0.2 else "comparable"
    if metric.startswith("latency"):
        if a_val == 0:
            return "N/A"
        pct = abs(c_val - a_val) / a_val
        return "notable" if pct >= 0.3 else "comparable"
    return ""


def build_markdown(by_phase, dns_event) -> tuple[str, dict]:
    lines = []
    lines.append("# S3Proxy DNS Cut-over Test Report")
    lines.append("")

    # ---- DNS event block ----
    lines.append("## DNS Cut Event")
    lines.append("")
    if dns_event:
        lines.append("| Field | Value |")
        lines.append("|---|---|")
        for k in (
            "DNS_NAME", "DNS_ZONE", "CUTOVER_TARGET", "BASELINE_IP",
            "DNS_CUT_START_UTC", "DNS_CUT_START_CST",
            "DNS_CUT_COMPLETE_UTC", "DNS_CUT_COMPLETE_CST",
            "DNS_CUT_ELAPSED_SEC", "DNS_CHANGE_ID",
        ):
            if k in dns_event:
                lines.append(f"| {k} | `{dns_event[k]}` |")
    else:
        lines.append("_DNS event metadata not provided._")
    lines.append("")

    # ---- Per-phase raw metrics ----
    for phase in sorted(by_phase.keys()):
        reports = by_phase[phase]
        if not reports:
            continue
        lines.append(f"## Phase {phase} — Raw Metrics")
        lines.append("")
        lines.append("| SDK | Op | Duration(s) | Total | Success | Error | ErrRate | RPS | p50(ms) | p95(ms) | p99(ms) | Max(ms) |")
        lines.append("|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
        for r in sorted(reports, key=lambda x: (x.get("sdk", ""), x.get("op", ""))):
            lat = r.get("latency_ms", {})
            lines.append(
                f"| {r.get('sdk')} | {r.get('op')} | "
                f"{r.get('duration_sec', 0):.1f} | "
                f"{r.get('total_requests', 0)} | {r.get('success', 0)} | {r.get('error', 0)} | "
                f"{r.get('error_rate', 0) * 100:.2f}% | "
                f"{r.get('throughput_rps', 0):.1f} | "
                f"{lat.get('p50', 0):.1f} | {lat.get('p95', 0):.1f} | "
                f"{lat.get('p99', 0):.1f} | {lat.get('max', 0):.1f} |"
            )
        lines.append("")

    # ---- Phase A vs Phase C comparison ----
    a_reports = index_by_key(by_phase.get("A", []))
    c_reports = index_by_key(by_phase.get("C", []))
    comparison_rows = []
    notable_hits = []

    if a_reports and c_reports:
        lines.append("## Phase A vs Phase C Comparison")
        lines.append("")
        lines.append("| SDK | Op | RPS Δ | Err Rate Δ (pp) | p50 Δ | p95 Δ | p99 Δ | Verdict |")
        lines.append("|---|---|---|---|---|---|---|---|")
        all_keys = sorted(set(a_reports.keys()) | set(c_reports.keys()))
        for key in all_keys:
            sdk, op = key
            a = a_reports.get(key)
            c = c_reports.get(key)
            if not a or not c:
                lines.append(f"| {sdk} | {op} | _missing_ |  |  |  |  | incomplete |")
                continue
            a_rps = a.get("throughput_rps", 0)
            c_rps = c.get("throughput_rps", 0)
            a_err = a.get("error_rate", 0)
            c_err = c.get("error_rate", 0)
            a_p50 = a.get("latency_ms", {}).get("p50", 0)
            c_p50 = c.get("latency_ms", {}).get("p50", 0)
            a_p95 = a.get("latency_ms", {}).get("p95", 0)
            c_p95 = c.get("latency_ms", {}).get("p95", 0)
            a_p99 = a.get("latency_ms", {}).get("p99", 0)
            c_p99 = c.get("latency_ms", {}).get("p99", 0)

            v_rps = verdict_row(a_rps, c_rps, "throughput")
            v_err = verdict_row(a_err, c_err, "error_rate")
            v_p95 = verdict_row(a_p95, c_p95, "latency_p95")
            row_verdict = "comparable"
            if "notable" in (v_rps, v_err, v_p95):
                row_verdict = "notable"
                notable_hits.append((sdk, op, v_rps, v_err, v_p95))

            lines.append(
                f"| {sdk} | {op} | "
                f"{abs_diff(c_rps, a_rps)} ({diff_pct(c_rps, a_rps)}) | "
                f"{(c_err - a_err) * 100:+.2f} | "
                f"{abs_diff(c_p50, a_p50)}ms ({diff_pct(c_p50, a_p50)}) | "
                f"{abs_diff(c_p95, a_p95)}ms ({diff_pct(c_p95, a_p95)}) | "
                f"{abs_diff(c_p99, a_p99)}ms ({diff_pct(c_p99, a_p99)}) | "
                f"{row_verdict} |"
            )
            comparison_rows.append({
                "sdk": sdk, "op": op,
                "rps_a": a_rps, "rps_c": c_rps,
                "error_rate_a": a_err, "error_rate_c": c_err,
                "p50_a": a_p50, "p50_c": c_p50,
                "p95_a": a_p95, "p95_c": c_p95,
                "p99_a": a_p99, "p99_c": c_p99,
                "verdict": row_verdict,
            })
        lines.append("")

    # ---- Conclusion ----
    lines.append("## Conclusion")
    lines.append("")
    if not a_reports or not c_reports:
        lines.append("- Incomplete Phase A / Phase C data; conclusion not available.")
    elif notable_hits:
        lines.append(f"- ⚠️ **Notable differences detected** on {len(notable_hits)} (SDK, Op) combinations:")
        for sdk, op, v_rps, v_err, v_p95 in notable_hits:
            flags = []
            if v_rps == "notable":
                flags.append("throughput")
            if v_err == "notable":
                flags.append("error-rate")
            if v_p95 == "notable":
                flags.append("p95-latency")
            lines.append(f"  - `{sdk}/{op}`: {', '.join(flags)}")
        lines.append("")
        lines.append("- Thresholds: throughput Δ ≥ 20%, p95 Δ ≥ 30%, error-rate Δ ≥ 1pp.")
    else:
        lines.append("- ✅ All (SDK, Op) combinations are within configured comparability thresholds.")
        lines.append("- Thresholds: throughput Δ < 20%, p95 Δ < 30%, error-rate Δ < 1pp.")
    lines.append("")

    structured = {
        "dns_event": dns_event,
        "phases": {k: v for k, v in by_phase.items()},
        "comparison": comparison_rows,
        "notable_count": len(notable_hits),
    }
    return "\n".join(lines), structured


def build_summary(md_report: str, max_lines: int = 300) -> str:
    # For GITHUB_STEP_SUMMARY we just cap lines so the 1 MiB limit is safe.
    lines = md_report.splitlines()
    if len(lines) <= max_lines:
        return md_report
    return "\n".join(lines[:max_lines] + ["", "_(truncated — see uploaded report.md artifact for full detail)_"])


def main():
    if len(sys.argv) != 3:
        print("usage: aggregate_pressure_report.py <results-dir> <out-dir>", file=sys.stderr)
        sys.exit(2)
    results_dir = Path(sys.argv[1])
    out_dir = Path(sys.argv[2])
    out_dir.mkdir(parents=True, exist_ok=True)

    by_phase = load_reports(results_dir)
    dns_event = load_dns_event(results_dir)

    md, structured = build_markdown(by_phase, dns_event)
    (out_dir / "report.md").write_text(md)
    (out_dir / "report.json").write_text(json.dumps(structured, indent=2))
    (out_dir / "summary.md").write_text(build_summary(md))
    print(f"Wrote: {out_dir/'report.md'}, {out_dir/'report.json'}, {out_dir/'summary.md'}")


if __name__ == "__main__":
    main()
