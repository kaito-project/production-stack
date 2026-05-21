#!/usr/bin/env bash
# Copyright 2026 The KAITO Authors.
#
# Generates E2E test-coverage reports from the Ginkgo test source files
# under test/e2e/. Produces two outputs:
#   1. Markdown report  — rendered inline via $GITHUB_STEP_SUMMARY
#   2. HTML report       — uploaded as a downloadable artifact
#
# Usage:
#   hack/e2e/generate-e2e-report.sh [OPTIONS]
#
# Options:
#   --label-filter  Ginkgo label filter used for this run (shown in header).
#   --output-md     Path to Markdown output. Default: e2e-coverage-report.md
#   --output-html   Path to HTML output.     Default: e2e-coverage-report.html
#   --workflow      Workflow name (displayed in the header).

set -euo pipefail

LABEL_FILTER="(all)"
OUTPUT_MD="e2e-coverage-report.md"
OUTPUT_HTML="e2e-coverage-report.html"
WORKFLOW=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --label-filter)  LABEL_FILTER="$2"; shift 2 ;;
    --output-md)     OUTPUT_MD="$2"; shift 2 ;;
    --output-html)   OUTPUT_HTML="$2"; shift 2 ;;
    --workflow)      WORKFLOW="$2"; shift 2 ;;
    *) echo "Unknown option: $1" >&2; exit 1 ;;
  esac
done

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
E2E_DIR="${REPO_ROOT}/test/e2e"
TIMESTAMP="$(date -u '+%Y-%m-%d %H:%M:%S UTC')"

# --- Parse Ginkgo label constants from utils/ginkgo.go --------------------
declare -A LABEL_MAP
while IFS= read -r line; do
  if [[ "$line" =~ ([A-Za-z_][A-Za-z0-9_]*)[[:space:]]*=[[:space:]]*.*Label\(\"([^\"]+)\"\) ]]; then
    LABEL_MAP["${BASH_REMATCH[1]}"]="${BASH_REMATCH[2]}"
  fi
done < "${E2E_DIR}/utils/ginkgo.go"

# --- Helpers --------------------------------------------------------------
extract_labels() {
  local line="$1" labels="" tmp
  tmp="$line"
  while [[ "$tmp" =~ utils\.(GinkgoLabel[A-Za-z0-9_]+) ]]; do
    local var="${BASH_REMATCH[1]}"
    local display="${LABEL_MAP[$var]:-$var}"
    labels="${labels:+$labels,}${display}"
    tmp="${tmp/${BASH_REMATCH[0]}/}"
  done
  tmp="$line"
  while [[ "$tmp" =~ Label\(\"([^\"]+)\"\) ]]; do
    local lbl="${BASH_REMATCH[1]}"
    if [[ ! ",$labels," == *",${lbl},"* ]]; then
      labels="${labels:+$labels,}${lbl}"
    fi
    tmp="${tmp/${BASH_REMATCH[0]}/}"
  done
  echo "$labels"
}

# --- Parse all test files -------------------------------------------------
declare -a FILE_NAMES=()
declare -a FILE_BLOCKS=()

total_its=0
total_describes=0
total_contexts=0

for gofile in "${E2E_DIR}"/*_test.go; do
  fname="$(basename "$gofile")"
  [[ "$fname" == "e2e_test.go" ]] && continue
  FILE_NAMES+=("$fname")
  blocks=""
  while IFS= read -r line; do
    [[ -z "${line// /}" ]] && continue
    if [[ "$line" =~ (Describe|Context|It)\(\"([^\"]+)\" ]]; then
      btype="${BASH_REMATCH[1]}"
      title="${BASH_REMATCH[2]}"
      labels="$(extract_labels "$line")"
      blocks+="${btype}|${title}|${labels}"$'\n'
      case "$btype" in
        It)       (( total_its++ )) || true ;;
        Describe) (( total_describes++ )) || true ;;
        Context)  (( total_contexts++ )) || true ;;
      esac
    fi
  done < "$gofile"
  FILE_BLOCKS+=("$blocks")
done

total_files=${#FILE_NAMES[@]}

# --- Compute chart data ---------------------------------------------------
CHART_COLORS=('#58a6ff' '#3fb950' '#f85149' '#bc8cff' '#f0883e' '#d29922' '#39d353' '#8b949e' '#79c0ff' '#56d364')
declare -a FILE_ITS_ARR=()
max_file_its=0
for i in "${!FILE_NAMES[@]}"; do
  n=$(echo "${FILE_BLOCKS[$i]}" | grep -c '^It|' || true)
  FILE_ITS_ARR+=("$n")
  (( n > max_file_its )) && max_file_its=$n || true
done

declare -A LABEL_BLOCK_COUNTS
# Pre-initialize all known labels to avoid unbound variable errors with set -u
for _lbl in Smoke Infra Routing Auth Scaling ScaleUp ScaleDown AntiFlapping FilterOrder Nightly NetworkPolicy PrefixCache InferenceSet; do
  LABEL_BLOCK_COUNTS["$_lbl"]=0
done
for i in "${!FILE_NAMES[@]}"; do
  while IFS='|' read -r btype title labels; do
    [[ -z "$btype" || -z "$labels" ]] && continue
    IFS=',' read -ra parts <<< "$labels"
    for lbl in "${parts[@]}"; do
      [[ -n "$lbl" ]] && (( LABEL_BLOCK_COUNTS["$lbl"]++ )) || true
    done
  done <<< "${FILE_BLOCKS[$i]}"
done

# =========================================================================
# 1. Markdown report (for GITHUB_STEP_SUMMARY)
# =========================================================================
{
  echo "# ✅ E2E Test Coverage Report"
  echo ""
  [[ -n "$WORKFLOW" ]] && echo "**Workflow:** ${WORKFLOW}  "
  echo "**Label filter:** \`${LABEL_FILTER}\`  "
  echo "**Generated:** ${TIMESTAMP}"
  echo ""
  echo "---"
  echo ""
  echo "| Metric | Count |"
  echo "|--------|------:|"
  echo "| 📄 Test Files | **${total_files}** |"
  echo "| 📋 Suites | **${total_describes}** |"
  echo "| 📂 Contexts | **${total_contexts}** |"
  echo "| 🧪 Test Cases | **${total_its}** |"
  echo ""
  echo "---"
  echo ""

  for i in "${!FILE_NAMES[@]}"; do
    fname="${FILE_NAMES[$i]}"
    blocks="${FILE_BLOCKS[$i]}"
    file_its=$(echo "$blocks" | grep -c '^It|' || true)
    file_descs=$(echo "$blocks" | grep -c '^Describe|' || true)

    echo "<details>"
    echo "<summary><strong>📄 ${fname}</strong> &mdash; ${file_its} tests, ${file_descs} suite(s)</summary>"
    echo ""

    while IFS='|' read -r btype title labels; do
      [[ -z "$btype" ]] && continue
      badges=""
      if [[ -n "$labels" ]]; then
        IFS=',' read -ra parts <<< "$labels"
        for lbl in "${parts[@]}"; do
          [[ -n "$lbl" ]] && badges="${badges} \`${lbl}\`"
        done
      fi
      case "$btype" in
        Describe) echo ""; echo "#### ▸ ${title}${badges}"; echo "" ;;
        Context)  echo "**◦ ${title}**${badges}"; echo "" ;;
        It)       echo "- 🟢 ${title}${badges}" ;;
      esac
    done <<< "$blocks"

    echo ""
    echo "</details>"
    echo ""
  done

  echo "---"
  echo ""
  echo "<details>"
  echo "<summary><strong>🏷️ Label Legend</strong></summary>"
  echo ""
  echo "| Label | Description |"
  echo "|-------|-------------|"
  echo "| \`Smoke\` | Basic sanity checks — every PR |"
  echo "| \`Infra\` | Infrastructure lifecycle (nodes, pods, GC) |"
  echo "| \`Routing\` | Gateway / model routing correctness |"
  echo "| \`Auth\` | API key authentication |"
  echo "| \`Scaling\` | KEDA-driven scale-up / scale-down |"
  echo "| \`ScaleUp\` | Scale-up specific assertions |"
  echo "| \`ScaleDown\` | Scale-down specific assertions |"
  echo "| \`AntiFlapping\` | Prevents premature re-scaling |"
  echo "| \`Nightly\` | Long-running tests (nightly only) |"
  echo "| \`NetworkPolicy\` | Kubernetes NetworkPolicy enforcement |"
  echo "| \`PrefixCache\` | Prefix / KV-cache aware routing |"
  echo "| \`InferenceSet\` | InferenceSet chart lifecycle |"
  echo "| \`FilterOrder\` | Envoy HTTP filter chain execution order |"
  echo ""
  echo "</details>"
  echo ""
  echo "_Generated by \`hack/e2e/generate-e2e-report.sh\` · KAITO Production Stack_"
} > "$OUTPUT_MD"

echo "✅ Markdown report → ${OUTPUT_MD}"

# =========================================================================
# 2. HTML report (self-contained, for artifact download)
# =========================================================================
generate_html() {
  cat <<'HTMLHEAD'
<!DOCTYPE html>
<html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>E2E Test Coverage Report</title>
<style>
:root{--bg:#0d1117;--sf:#161b22;--bd:#30363d;--tx:#e6edf3;--mt:#8b949e;--ac:#58a6ff;--gn:#3fb950;--yl:#d29922;--rd:#f85149;--pp:#bc8cff;--or:#f0883e;--cy:#39d353}
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif;background:var(--bg);color:var(--tx);line-height:1.6;padding:24px 24px 40px}
.w{max-width:1200px;margin:0 auto}
.hd-bar{height:3px;background:linear-gradient(90deg,var(--ac),var(--gn),var(--pp),var(--or));border-radius:2px;margin-bottom:20px}
.hd{display:flex;align-items:center;gap:14px;margin-bottom:28px;padding-bottom:16px;border-bottom:1px solid var(--bd)}
.hd h1{font-size:24px;font-weight:700}.hd .sb{color:var(--mt);font-size:13px;margin-top:2px}
.cr{display:grid;grid-template-columns:repeat(4,1fr);gap:14px;margin-bottom:24px}
@media(max-width:640px){.cr{grid-template-columns:repeat(2,1fr)}}
.cd{background:linear-gradient(135deg,var(--sf) 0%,rgba(22,27,34,.7) 100%);border:1px solid var(--bd);border-radius:12px;padding:18px 20px;text-align:center;position:relative;overflow:hidden}
.cd::before{content:'';position:absolute;top:0;left:0;right:0;height:2px}
.cd:nth-child(1)::before{background:linear-gradient(90deg,var(--ac),#79c0ff)}
.cd:nth-child(2)::before{background:linear-gradient(90deg,var(--pp),#d2a8ff)}
.cd:nth-child(3)::before{background:linear-gradient(90deg,var(--or),var(--yl))}
.cd:nth-child(4)::before{background:linear-gradient(90deg,var(--gn),var(--cy))}
.cd .ic{font-size:22px;margin-bottom:6px}
.cd .v{font-size:36px;font-weight:800;line-height:1}
.cd:nth-child(1) .v{color:var(--ac)}.cd:nth-child(2) .v{color:var(--pp)}
.cd:nth-child(3) .v{color:var(--or)}.cd:nth-child(4) .v{color:var(--gn)}
.cd .l{font-size:11px;color:var(--mt);text-transform:uppercase;letter-spacing:.8px;margin-top:6px;font-weight:600}
.br{background:var(--sf);border:1px solid var(--bd);border-radius:8px;padding:10px 16px;margin-bottom:28px;font-size:13px;color:var(--mt);display:flex;gap:20px;flex-wrap:wrap}.br b{color:var(--tx)}
.charts{display:grid;grid-template-columns:1fr 260px;gap:16px;margin-bottom:28px}
@media(max-width:768px){.charts{grid-template-columns:1fr}}
.panel{background:var(--sf);border:1px solid var(--bd);border-radius:12px;padding:20px}
.panel h3{font-size:12px;text-transform:uppercase;letter-spacing:1px;color:var(--mt);margin-bottom:16px;font-weight:700}
.bar-row{display:flex;align-items:center;gap:10px;margin-bottom:8px;font-size:12px}
.bar-label{width:190px;text-overflow:ellipsis;overflow:hidden;white-space:nowrap;color:var(--tx);font-weight:500;font-size:12px}
.bar-track{flex:1;height:20px;background:rgba(48,54,61,.5);border-radius:4px;overflow:hidden}
.bar-fill{height:100%;border-radius:4px;display:flex;align-items:center;padding-left:8px;font-size:10px;font-weight:700;color:var(--bg);min-width:24px}
.bar-val{width:28px;text-align:right;font-weight:700;color:var(--ac);font-size:13px;font-variant-numeric:tabular-nums}
.donut-wrap{display:flex;flex-direction:column;align-items:center;gap:14px}
.donut{width:150px;height:150px;border-radius:50%;position:relative}
.donut-hole{position:absolute;top:50%;left:50%;transform:translate(-50%,-50%);width:86px;height:86px;border-radius:50%;background:var(--sf);display:flex;flex-direction:column;align-items:center;justify-content:center}
.donut-hole .dv{font-size:28px;font-weight:800;color:var(--ac);line-height:1}
.donut-hole .dl{font-size:9px;color:var(--mt);text-transform:uppercase;letter-spacing:.5px;margin-top:2px}
.donut-legend{display:flex;flex-wrap:wrap;gap:4px 10px;justify-content:center}
.donut-legend-item{display:flex;align-items:center;gap:4px;font-size:10px;color:var(--mt)}
.donut-legend-item .swatch{width:8px;height:8px;border-radius:2px;flex-shrink:0}
.lbl-section{margin-bottom:28px}
.lbl-section h3{font-size:12px;text-transform:uppercase;letter-spacing:1px;color:var(--mt);margin-bottom:14px;font-weight:700}
.lbl-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(120px,1fr));gap:10px}
.lbl-card{background:var(--sf);border:1px solid var(--bd);border-radius:10px;padding:14px 10px 12px;text-align:center;position:relative;overflow:hidden;transition:border-color .2s}
.lbl-card:hover{border-color:var(--ac)}
.lbl-card::after{content:'';position:absolute;bottom:0;left:0;right:0;height:3px}
.lbl-card .lc-n{font-size:24px;font-weight:800;line-height:1}
.lbl-card .lc-l{font-size:10px;text-transform:uppercase;letter-spacing:.5px;margin-top:4px;font-weight:600}
.tree-hd{font-size:12px;text-transform:uppercase;letter-spacing:1px;color:var(--mt);margin-bottom:14px;font-weight:700}
.fs{background:var(--sf);border:1px solid var(--bd);border-radius:8px;margin-bottom:8px;overflow:hidden}
.fh{padding:10px 14px;background:rgba(56,139,253,.04);border-bottom:1px solid var(--bd);cursor:pointer;display:flex;align-items:center;gap:8px;user-select:none;font-size:14px}
.fh:hover{background:rgba(56,139,253,.08)}
.fh .cv{transition:transform .2s;font-size:11px;color:var(--mt)}.fh.cl .cv{transform:rotate(-90deg)}
.fh .fn{font-weight:600}.fh .st{margin-left:auto;font-size:12px;color:var(--mt)}
.fb{padding:0}.fb.hd2{display:none}
.dh{padding:10px 14px 8px 20px;font-weight:600;font-size:14px;color:var(--pp);border-bottom:1px solid rgba(48,54,61,.5)}
.cx{padding:8px 14px 6px 36px;font-size:13px;font-weight:600;color:var(--or);border-top:1px solid rgba(48,54,61,.4)}
.ti{padding:5px 14px 5px 52px;font-size:13px;display:flex;align-items:center;gap:8px;border-top:1px solid rgba(48,54,61,.25)}
.ti:hover{background:rgba(56,139,253,.04)}
.ti .dt{width:6px;height:6px;border-radius:50%;background:var(--gn);flex-shrink:0}
.tg{display:inline-block;font-size:10px;font-weight:500;padding:1px 7px;border-radius:10px;margin-left:3px;vertical-align:middle}
.t-smoke{background:rgba(63,185,80,.15);border:1px solid rgba(63,185,80,.4);color:var(--gn)}
.t-infra,.t-networkpolicy,.t-inferenceset{background:rgba(188,140,255,.15);border:1px solid rgba(188,140,255,.4);color:var(--pp)}
.t-routing,.t-prefixcache{background:rgba(240,136,62,.15);border:1px solid rgba(240,136,62,.4);color:var(--or)}
.t-auth{background:rgba(248,81,73,.15);border:1px solid rgba(248,81,73,.4);color:var(--rd)}
.t-nightly{background:rgba(210,153,34,.15);border:1px solid rgba(210,153,34,.4);color:var(--yl)}
.t-scaling,.t-scaleup,.t-scaledown,.t-antiflapping{background:rgba(57,211,83,.15);border:1px solid rgba(57,211,83,.4);color:var(--cy)}
.t-shadowpod,.t-filterorder{background:rgba(56,139,253,.15);border:1px solid rgba(56,139,253,.4);color:var(--ac)}
.lg{margin-top:28px;padding:16px;background:var(--sf);border:1px solid var(--bd);border-radius:10px}
.lg h3{font-size:12px;margin-bottom:10px;text-transform:uppercase;letter-spacing:1px;color:var(--mt);font-weight:700}
.lg .gd{display:flex;flex-wrap:wrap;gap:6px}
.ft{margin-top:24px;text-align:center;font-size:11px;color:var(--mt)}
</style></head><body><div class="w">
HTMLHEAD

  # --- Header ---
  echo "<div class=\"hd-bar\"></div>"
  echo "<div class=\"hd\">"
  echo "<svg width=\"32\" height=\"32\" viewBox=\"0 0 32 32\" fill=\"none\"><circle cx=\"16\" cy=\"16\" r=\"16\" fill=\"#58a6ff\"/><path d=\"M10 16l4 4 8-8\" stroke=\"#0d1117\" stroke-width=\"2.5\" stroke-linecap=\"round\" stroke-linejoin=\"round\"/></svg>"
  echo "<div><h1>E2E Test Coverage Report</h1>"
  echo "<div class=\"sb\">${WORKFLOW:+${WORKFLOW} — }${TIMESTAMP}</div></div></div>"

  # --- Stat cards ---
  echo "<div class=\"cr\">"
  echo "<div class=\"cd\"><div class=\"ic\">📄</div><div class=\"v\">${total_files}</div><div class=\"l\">Test Files</div></div>"
  echo "<div class=\"cd\"><div class=\"ic\">📋</div><div class=\"v\">${total_describes}</div><div class=\"l\">Suites</div></div>"
  echo "<div class=\"cd\"><div class=\"ic\">📂</div><div class=\"v\">${total_contexts}</div><div class=\"l\">Contexts</div></div>"
  echo "<div class=\"cd\"><div class=\"ic\">🧪</div><div class=\"v\">${total_its}</div><div class=\"l\">Test Cases</div></div>"
  echo "</div>"

  # --- Info bar ---
  echo "<div class=\"br\"><div><b>Label filter:</b> ${LABEL_FILTER}</div><div><b>Generated:</b> ${TIMESTAMP}</div></div>"

  # --- Charts row ---
  echo "<div class=\"charts\">"

  # Bar chart panel
  echo "<div class=\"panel\"><h3>📊 Tests by File</h3>"
  for i in "${!FILE_NAMES[@]}"; do
    local fname="${FILE_NAMES[$i]}"
    local n="${FILE_ITS_ARR[$i]}"
    local pct=0
    (( max_file_its > 0 )) && pct=$(( n * 100 / max_file_its )) || true
    local color="${CHART_COLORS[$i]:-#8b949e}"
    echo "<div class=\"bar-row\">"
    echo "<span class=\"bar-label\">${fname%_test.go}</span>"
    echo "<div class=\"bar-track\"><div class=\"bar-fill\" style=\"width:${pct}%;background:${color}\"></div></div>"
    echo "<span class=\"bar-val\">${n}</span>"
    echo "</div>"
  done
  echo "</div>"

  # Donut chart panel
  local gradient="" cumulative=0
  if (( total_its > 0 )); then
    for i in "${!FILE_NAMES[@]}"; do
      local n="${FILE_ITS_ARR[$i]}"
      local deg_start=$(( cumulative * 360 / total_its ))
      cumulative=$(( cumulative + n ))
      local deg_end=$(( cumulative * 360 / total_its ))
      local color="${CHART_COLORS[$i]:-#8b949e}"
      gradient+="${color} ${deg_start}deg ${deg_end}deg,"
    done
    gradient="${gradient%,}"
  else
    gradient="var(--bd) 0deg 360deg"
  fi
  echo "<div class=\"panel\"><h3>📈 Distribution</h3>"
  echo "<div class=\"donut-wrap\">"
  echo "<div class=\"donut\" style=\"background:conic-gradient(${gradient})\">"
  echo "<div class=\"donut-hole\"><span class=\"dv\">${total_its}</span><span class=\"dl\">tests</span></div>"
  echo "</div>"
  echo "<div class=\"donut-legend\">"
  for i in "${!FILE_NAMES[@]}"; do
    local fname="${FILE_NAMES[$i]}"
    local color="${CHART_COLORS[$i]:-#8b949e}"
    echo "<div class=\"donut-legend-item\"><span class=\"swatch\" style=\"background:${color}\"></span>${fname%_test.go}</div>"
  done
  echo "</div></div></div>"
  echo "</div>"

  # --- Label coverage cards ---
  local ordered_labels=(Smoke Infra Routing Auth Scaling ScaleUp ScaleDown AntiFlapping FilterOrder Nightly NetworkPolicy PrefixCache InferenceSet)
  declare -A lbl_colors=(
    [Smoke]="#3fb950" [Infra]="#bc8cff" [Routing]="#f0883e" [Auth]="#f85149"
    [Scaling]="#39d353" [ScaleUp]="#39d353" [ScaleDown]="#39d353" [AntiFlapping]="#39d353"
    [FilterOrder]="#58a6ff" [Nightly]="#d29922" [NetworkPolicy]="#bc8cff"
    [PrefixCache]="#f0883e" [InferenceSet]="#bc8cff"
  )
  echo "<div class=\"lbl-section\"><h3>🏷️ Coverage by Label</h3>"
  echo "<div class=\"lbl-grid\">"
  for lbl in "${ordered_labels[@]}"; do
    local cnt="${LABEL_BLOCK_COUNTS[$lbl]:-0}"
    local lc="${lbl_colors[$lbl]:-#8b949e}"
    echo "<div class=\"lbl-card\"><div style=\"position:absolute;bottom:0;left:0;right:0;height:3px;background:${lc}\"></div><div class=\"lc-n\" style=\"color:${lc}\">${cnt}</div><div class=\"lc-l\" style=\"color:${lc}\">${lbl}</div></div>"
  done
  echo "</div></div>"

  # --- Detailed test tree (collapsed by default) ---
  echo "<h3 class=\"tree-hd\">🔍 Detailed Test Tree</h3>"
  for i in "${!FILE_NAMES[@]}"; do
    local fname="${FILE_NAMES[$i]}"
    local blocks="${FILE_BLOCKS[$i]}"
    local file_its file_descs fid
    file_its=$(echo "$blocks" | grep -c '^It|' || true)
    file_descs=$(echo "$blocks" | grep -c '^Describe|' || true)
    fid="f${i}"

    echo "<div class=\"fs\">"
    printf '<div class="fh cl" onclick="var e=document.getElementById('"'"'%s'"'"');e.classList.toggle('"'"'hd2'"'"');this.classList.toggle('"'"'cl'"'"')">' "$fid"
    echo "<span class=\"cv\">▼</span><span class=\"fn\">${fname}</span>"
    echo "<span class=\"st\">${file_its} tests · ${file_descs} suite(s)</span></div>"
    echo "<div class=\"fb hd2\" id=\"${fid}\">"

    while IFS='|' read -r btype title labels; do
      [[ -z "$btype" ]] && continue
      local tags=""
      if [[ -n "$labels" ]]; then
        IFS=',' read -ra parts <<< "$labels"
        for lbl in "${parts[@]}"; do
          [[ -n "$lbl" ]] && tags="${tags}<span class=\"tg t-$(echo "$lbl" | tr '[:upper:]' '[:lower:]')\">${lbl}</span>"
        done
      fi
      case "$btype" in
        Describe) echo "<div class=\"dh\">▸ ${title} ${tags}</div>" ;;
        Context)  echo "<div class=\"cx\">◦ ${title} ${tags}</div>" ;;
        It)       echo "<div class=\"ti\"><span class=\"dt\"></span>${title} ${tags}</div>" ;;
      esac
    done <<< "$blocks"

    echo "</div></div>"
  done

  # --- Legend ---
  echo "<div class=\"lg\"><h3>Label Legend</h3><div class=\"gd\">"
  for lbl in "${ordered_labels[@]}"; do
    printf '<span class="tg t-%s">%s</span>' "$(echo "$lbl" | tr '[:upper:]' '[:lower:]')" "$lbl"
  done
  echo "</div></div>"
  echo "<div class=\"ft\">Generated by hack/e2e/generate-e2e-report.sh · KAITO Production Stack</div>"
  echo "</div></body></html>"
}

generate_html > "$OUTPUT_HTML"
echo "✅ HTML report  → ${OUTPUT_HTML}"
