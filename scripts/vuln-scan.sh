#!/usr/bin/env bash
#
# vuln-scan.sh — Semgrep SAST + secret scan, triaged by a local LLM (Ollama).
#
# Lives in <project>/scripts/ and scans its own project (the parent dir).
# Semgrep finds candidate vulns + leaked secrets; the local LLM then reviews
# each finding (real vs false positive, exploitability, and a concrete fix)
# and writes a markdown report under <project>/security-reports/.
#
# Usage:
#   ./scripts/vuln-scan.sh                 # scan the whole project
#   MODEL=qwen3.6:27b ./scripts/vuln-scan.sh   # override the LLM
#   OLLAMA_URL=http://10.0.0.5:11434 ./scripts/vuln-scan.sh
#   NO_LLM=1 ./scripts/vuln-scan.sh        # semgrep only, skip the LLM step
#
set -euo pipefail

# ---------- config (override via env) ----------
OLLAMA_URL="${OLLAMA_URL:-http://10.0.0.99:11434}"   # admin PC / RX 7900 XTX
MODEL="${MODEL:-qwen3-coder:30b}"
NUM_CTX="${NUM_CTX:-32768}"        # context window for the review request
BATCH="${BATCH:-8}"                # findings sent to the LLM per request
CTX_LINES="${CTX_LINES:-8}"        # source lines of context above/below each hit
NO_LLM="${NO_LLM:-0}"

export PATH="$HOME/.local/bin:$PATH"   # pipx-installed semgrep

# Semgrep rule packs. Registry packs need internet the first time (they cache).
SEMGREP_CONFIGS=(
  --config p/default    # curated cross-language security baseline
  --config p/secrets    # leaked API keys / tokens / credentials
  --config p/golang
  --config p/react
  --config p/csharp
)

# ---------- paths ----------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
PROJECT_NAME="$(basename "$PROJECT_ROOT")"
STAMP="$(date +%Y%m%d-%H%M%S)"
OUT_DIR="$PROJECT_ROOT/security-reports"
RAW_JSON="$OUT_DIR/semgrep-$STAMP.json"
REPORT="$OUT_DIR/review-$STAMP.md"
mkdir -p "$OUT_DIR"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

c_bold=$'\e[1m'; c_red=$'\e[31m'; c_grn=$'\e[32m'; c_ylw=$'\e[33m'; c_off=$'\e[0m'
say() { printf '%s\n' "$*"; }

# ---------- preflight ----------
command -v semgrep >/dev/null 2>&1 || { say "${c_red}semgrep not found on PATH.${c_off} Install: pipx install semgrep"; exit 1; }
command -v jq >/dev/null 2>&1      || { say "${c_red}jq not found.${c_off} Install jq."; exit 1; }

say "${c_bold}=== Vuln scan: $PROJECT_NAME ===${c_off}"
say "Project : $PROJECT_ROOT"
say "Semgrep : $(semgrep --version 2>/dev/null | head -1)"

# ---------- 1. Semgrep ----------
say "${c_bold}[1/3]${c_off} Running Semgrep (SAST + secrets)..."
# Semgrep exits 1 when it finds something; that is not an error for us.
set +e
semgrep scan "${SEMGREP_CONFIGS[@]}" \
  --json --quiet --metrics=off --error --timeout 120 \
  --exclude node_modules --exclude dist --exclude build --exclude .next \
  --exclude vendor --exclude security-reports --exclude '*.min.js' \
  --output "$RAW_JSON" "$PROJECT_ROOT"
sg_rc=$?
set -e
if [ ! -s "$RAW_JSON" ]; then
  say "${c_red}Semgrep produced no output (exit $sg_rc). Check network/rule access.${c_off}"
  exit 1
fi

# ---------- 2. Condense findings ----------
jq '[.results[] | {
  check_id,
  path,
  line: .start.line,
  end_line: .end.line,
  severity: (.extra.severity // "INFO"),
  is_secret: ((.check_id | ascii_downcase) | test("secret|api.?key|token|password|credential")),
  message: (.extra.message // "")
}] | sort_by((.is_secret | not), .severity)' "$RAW_JSON" > "$TMP/findings.json"

# ---- enrich each finding with surrounding source so the LLM can trace data flow ----
say "      enriching findings with ±$CTX_LINES lines of source context..."
: > "$TMP/enriched.jsonl"
NFOUND=$(jq 'length' "$TMP/findings.json")
for idx in $(seq 0 $((NFOUND-1))); do
  obj=$(jq -c ".[$idx]" "$TMP/findings.json")
  fpath=$(printf '%s' "$obj" | jq -r '.path')
  fline=$(printf '%s' "$obj" | jq -r '.line')
  ctx=""
  if [[ "$fline" =~ ^[0-9]+$ ]]; then
    real="$fpath"; [ -f "$real" ] || real="$PROJECT_ROOT/$fpath"
    if [ -f "$real" ]; then
      s=$(( fline > CTX_LINES ? fline - CTX_LINES : 1 )); e=$(( fline + CTX_LINES ))
      ctx=$(awk -v s="$s" -v e="$e" 'NR>=s && NR<=e {printf "%6d| %s\n", NR, $0} NR>e {exit}' "$real" 2>/dev/null || true)
    fi
  fi
  printf '%s' "$obj" | jq -c --arg ctx "$ctx" '. + {context: $ctx}' >> "$TMP/enriched.jsonl"
done
jq -s '.' "$TMP/enriched.jsonl" > "$TMP/findings.json"

TOTAL=$(jq 'length' "$TMP/findings.json")
SECRETS=$(jq '[.[] | select(.is_secret)] | length' "$TMP/findings.json")
ERRORS=$(jq '[.[] | select(.severity=="ERROR")] | length' "$TMP/findings.json")
WARNS=$(jq '[.[] | select(.severity=="WARNING")] | length' "$TMP/findings.json")

say "${c_bold}[2/3]${c_off} Findings: ${c_red}$TOTAL total${c_off}  |  ${c_red}$SECRETS secrets${c_off}  |  $ERRORS error  |  $WARNS warning"

# ---------- report header ----------
{
  echo "# Security review — $PROJECT_NAME"
  echo
  echo "- **Scanned:** $(date '+%Y-%m-%d %H:%M:%S')"
  echo "- **Tool:** Semgrep $(semgrep --version 2>/dev/null | head -1) + LLM triage (\`$MODEL\`)"
  echo "- **Findings:** $TOTAL total — **$SECRETS possible secrets**, $ERRORS error, $WARNS warning"
  echo "- **Raw Semgrep JSON:** \`$(basename "$RAW_JSON")\`"
  echo
  echo "> LLM triage is advisory. Treat every **possible secret** as compromised until proven"
  echo "> otherwise — rotate the credential and purge it from git history if confirmed."
  echo
} > "$REPORT"

if [ "$TOTAL" -eq 0 ]; then
  echo "## ✅ No findings" >> "$REPORT"
  say "${c_grn}No findings. Report: $REPORT${c_off}"
  exit 0
fi

if [ "$NO_LLM" = "1" ]; then
  echo "## Raw findings (LLM triage skipped)" >> "$REPORT"
  echo '```json' >> "$REPORT"; cat "$TMP/findings.json" >> "$REPORT"; echo '```' >> "$REPORT"
  say "${c_ylw}NO_LLM=1 — wrote raw findings only.${c_off} Report: $REPORT"
  exit 0
fi

# ---------- 3. LLM triage ----------
# Reachability + model presence check
if ! curl -s -m 8 "$OLLAMA_URL/api/tags" >/dev/null 2>&1; then
  say "${c_red}Cannot reach Ollama at $OLLAMA_URL — writing raw findings instead.${c_off}"
  echo "## Raw findings (LLM unreachable)" >> "$REPORT"
  echo '```json' >> "$REPORT"; cat "$TMP/findings.json" >> "$REPORT"; echo '```' >> "$REPORT"
  exit 0
fi
if ! curl -s -m 8 "$OLLAMA_URL/api/tags" | jq -e --arg m "$MODEL" '.models[]?.name | select(. == $m)' >/dev/null 2>&1; then
  say "${c_ylw}Warning: model '$MODEL' not listed on $OLLAMA_URL. Trying anyway.${c_off}"
fi

SYS_PROMPT="You are a senior application-security engineer reviewing static-analysis (Semgrep) findings for '$PROJECT_NAME', an application OWNED by the person requesting this authorized review.

Each finding includes a 'context' field: numbered source lines around the flagged line ('lineNo| code'). USE IT to judge — do not judge from the rule message alone.

Decision rules (be skeptical — Semgrep over-reports):
- Mark REAL only if the context shows a plausible path from UNTRUSTED/EXTERNAL input (HTTP request, CLI arg, env, file, network) to the dangerous sink.
- If the arguments to the sink are constants, internal/derived values, or you cannot see the input source in the context, use LIKELY FALSE POSITIVE or NEEDS HUMAN CHECK. Do NOT call something REAL just because Semgrep flagged it.
- For is_secret=true findings: identify the secret TYPE from the context. A bcrypt/argon2 HASH or an obvious test/example placeholder is usually NOT a live secret — say so. A real API key/token/private key IS live: state it must be rotated and scrubbed.

For EACH finding output exactly this markdown section:
### <path>:<line> — <short title>
- **Verdict:** REAL | LIKELY FALSE POSITIVE | NEEDS HUMAN CHECK
- **Severity:** Critical | High | Medium | Low (your judgment)
- **Why:** 1-2 sentences citing what you saw in the context (e.g. 'arg is a hardcoded constant', 'req.URL.Query() flows into exec')
- **Fix:** concrete, code-level remediation

Copy <path> and <line> VERBATIM from the JSON — never alter or reformat a path. Do not invent findings beyond the JSON provided. Be concise."

say "${c_bold}[3/3]${c_off} LLM triage via $MODEL @ $OLLAMA_URL (batches of $BATCH)..."
echo "## Triaged findings" >> "$REPORT"
echo >> "$REPORT"

i=0; batch_no=1
while [ "$i" -lt "$TOTAL" ]; do
  jq -c ".[$i:$((i+BATCH))]" "$TMP/findings.json" > "$TMP/batch.json"
  n_in_batch=$(jq 'length' "$TMP/batch.json")
  say "   batch $batch_no: findings $((i+1))-$((i+n_in_batch)) of $TOTAL"

  USER_PROMPT="Findings JSON:
$(cat "$TMP/batch.json")"

  jq -n --arg model "$MODEL" --arg sys "$SYS_PROMPT" --arg usr "$USER_PROMPT" --argjson ctx "$NUM_CTX" \
    '{model:$model, stream:false,
      options:{num_ctx:$ctx, temperature:0.15},
      messages:[{role:"system",content:$sys},{role:"user",content:$usr}]}' > "$TMP/payload.json"

  resp="$(curl -s -m 900 "$OLLAMA_URL/api/chat" --data-binary @"$TMP/payload.json" || true)"
  content="$(printf '%s' "$resp" | jq -r '.message.content // empty' 2>/dev/null)"
  if [ -z "$content" ]; then
    echo "> ⚠️ LLM returned no content for batch $batch_no. Raw findings:" >> "$REPORT"
    echo '```json' >> "$REPORT"; cat "$TMP/batch.json" >> "$REPORT"; echo '```' >> "$REPORT"
  else
    printf '%s\n\n' "$content" >> "$REPORT"
  fi

  i=$((i+BATCH)); batch_no=$((batch_no+1))
done

say "${c_grn}Done.${c_off} Report: ${c_bold}$REPORT${c_off}"
if [ "$SECRETS" -gt 0 ]; then
  say "${c_red}${c_bold}⚠  $SECRETS possible secret(s) found — review the report first.${c_off}"
fi
