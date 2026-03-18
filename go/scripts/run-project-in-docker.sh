#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  run-project-in-docker.sh [options] /absolute/or/relative/project/path

Options:
  --linear-project-slug SLUG   Linear project name/slug input. Defaults to sanitized project directory name and is resolved to Linear slugId.
  --port PORT                  HTTP port to expose. Defaults to a deterministic port per project.
  --container-name NAME        Docker container name. Defaults to symphony-<project>.
  --image NAME                 Docker image tag. Defaults to symphony-go.
  --require-gh-token           Fail if GH_TOKEN is not set.
  --force                      Overwrite an existing generated .symphony/WORKFLOW.md.
  --build                      Force a docker rebuild even if the image already exists.
  -h, --help                   Show this help.
EOF
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

load_dotenv_file() {
  local path="$1"
  [[ -f "${path}" ]] || return 0
  while IFS= read -r line || [[ -n "${line}" ]]; do
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line%"${line##*[![:space:]]}"}"
    [[ -z "${line}" || "${line}" == \#* ]] && continue
    [[ "${line}" == export\ * ]] && line="${line#export }"
    if [[ "${line}" != *=* ]]; then
      continue
    fi
    local key="${line%%=*}"
    local value="${line#*=}"
    key="${key%"${key##*[![:space:]]}"}"
    key="${key#"${key%%[![:space:]]*}"}"
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    if [[ "${value}" =~ ^\".*\"$ ]] || [[ "${value}" =~ ^\'.*\'$ ]]; then
      value="${value:1:${#value}-2}"
    fi
    if [[ -z "${!key:-}" ]]; then
      export "${key}=${value}"
    fi
  done < "${path}"
}

normalize_env_aliases() {
  if [[ -z "${LINEAR_API_KEY:-}" && -n "${LINEAR_API_TOKEN:-}" ]]; then
    export LINEAR_API_KEY="${LINEAR_API_TOKEN}"
  fi
  if [[ -z "${GH_TOKEN:-}" && -n "${GITHUB_TOKEN:-}" ]]; then
    export GH_TOKEN="${GITHUB_TOKEN}"
  fi
  if [[ -z "${GITHUB_TOKEN:-}" && -n "${GH_TOKEN:-}" ]]; then
    export GITHUB_TOKEN="${GH_TOKEN}"
  fi
}

resolve_linear_project_slug_id() {
  local requested="$1"
  LINEAR_PROJECT_INPUT="${requested}" LINEAR_API_KEY="${LINEAR_API_KEY}" node <<'NODE'
const input = (process.env.LINEAR_PROJECT_INPUT || '').trim();
const token = (process.env.LINEAR_API_KEY || '').trim();

function normalize(value) {
  return value.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
}

if (!input) {
  console.error("missing Linear project input");
  process.exit(1);
}
if (!token) {
  console.error("missing LINEAR_API_KEY");
  process.exit(1);
}

const query = `query {
  projects(first: 250) {
    nodes {
      name
      slugId
    }
  }
}`;

const res = await fetch("https://api.linear.app/graphql", {
  method: "POST",
  headers: {
    "content-type": "application/json",
    authorization: token,
  },
  body: JSON.stringify({ query }),
});

if (!res.ok) {
  console.error(`linear project lookup failed: ${res.status}`);
  process.exit(1);
}

const payload = await res.json();
const nodes = payload?.data?.projects?.nodes || [];
const exactSlug = nodes.find((node) => (node.slugId || "") === input);
if (exactSlug?.slugId) {
  process.stdout.write(exactSlug.slugId);
  process.exit(0);
}

const wanted = normalize(input);
const byName = nodes.find((node) => normalize(node.name || "") === wanted);
if (byName?.slugId) {
  process.stdout.write(byName.slugId);
  process.exit(0);
}

console.error(`unable to resolve Linear project: ${input}`);
process.exit(1);
NODE
}

PROJECT_PATH=""
LINEAR_PROJECT_SLUG=""
PORT=""
CONTAINER_NAME=""
IMAGE_NAME="symphony-go"
REQUIRE_GH_TOKEN=0
FORCE=0
FORCE_BUILD=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --linear-project-slug)
      LINEAR_PROJECT_SLUG="${2:-}"
      shift 2
      ;;
    --port)
      PORT="${2:-}"
      shift 2
      ;;
    --container-name)
      CONTAINER_NAME="${2:-}"
      shift 2
      ;;
    --image)
      IMAGE_NAME="${2:-}"
      shift 2
      ;;
    --require-gh-token)
      REQUIRE_GH_TOKEN=1
      shift
      ;;
    --force)
      FORCE=1
      shift
      ;;
    --build)
      FORCE_BUILD=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    -*)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 1
      ;;
    *)
      if [[ -n "${PROJECT_PATH}" ]]; then
        echo "Only one project path is supported." >&2
        exit 1
      fi
      PROJECT_PATH="$1"
      shift
      ;;
  esac
done

if [[ -z "${PROJECT_PATH}" ]]; then
  usage >&2
  exit 1
fi

PROJECT_PATH="$(cd "${PROJECT_PATH}" && pwd)"
if [[ ! -d "${PROJECT_PATH}" ]]; then
  echo "Project path does not exist: ${PROJECT_PATH}" >&2
  exit 1
fi

load_dotenv_file "${PROJECT_PATH}/.env"
load_dotenv_file "${GO_DIR}/../.env"
normalize_env_aliases

if [[ -z "${LINEAR_API_KEY:-}" ]]; then
  echo "LINEAR_API_KEY must be set in the environment." >&2
  exit 1
fi

if [[ "${REQUIRE_GH_TOKEN}" -eq 1 ]] && [[ -z "${GH_TOKEN:-}" ]]; then
  echo "GH_TOKEN must be set in the environment when --require-gh-token is used." >&2
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required." >&2
  exit 1
fi

if ! command -v codex >/dev/null 2>&1; then
  echo "codex is required on the host so its installed package can be mounted into the container." >&2
  exit 1
fi

if ! command -v node >/dev/null 2>&1; then
  echo "node is required on the host to resolve the Linear project to its slugId." >&2
  exit 1
fi

PROJECT_NAME="$(basename "${PROJECT_PATH}")"
SANITIZED_NAME="$(printf '%s' "${PROJECT_NAME}" | tr '[:upper:]' '[:lower:]' | tr -cs 'a-z0-9' '-')"
SANITIZED_NAME="${SANITIZED_NAME#-}"
SANITIZED_NAME="${SANITIZED_NAME%-}"
SOURCE_BRANCH="$(git -C "${PROJECT_PATH}" symbolic-ref --short HEAD 2>/dev/null || printf 'main')"
ORIGIN_URL="$(git -C "${PROJECT_PATH}" remote get-url origin 2>/dev/null || true)"

if [[ -z "${LINEAR_PROJECT_SLUG}" ]]; then
  LINEAR_PROJECT_SLUG="${SANITIZED_NAME}"
fi

LINEAR_PROJECT_SLUG="$(resolve_linear_project_slug_id "${LINEAR_PROJECT_SLUG}")"

if [[ -z "${CONTAINER_NAME}" ]]; then
  CONTAINER_NAME="symphony-${SANITIZED_NAME}"
fi

if [[ -z "${PORT}" ]]; then
  CHECKSUM="$(printf '%s' "${PROJECT_PATH}" | cksum | awk '{print $1}')"
  PORT="$((4100 + (CHECKSUM % 700)))"
fi

CODEX_BIN="$(command -v codex)"
CODEX_JS="$(readlink -f "${CODEX_BIN}")"
CODEX_PACKAGE_ROOT="$(cd "$(dirname "${CODEX_JS}")/.." && pwd)"
CODEX_AUTH_DIR="${HOME}/.codex"
CODEX_AGENTS_DIR="${HOME}/.agents"

if [[ ! -d "${CODEX_AUTH_DIR}" ]]; then
  echo "Expected Codex auth directory at ${CODEX_AUTH_DIR}." >&2
  exit 1
fi

echo "Using auth inputs: linear_api_key_present=$([[ -n "${LINEAR_API_KEY:-}" ]] && echo true || echo false) gh_token_present=$([[ -n "${GH_TOKEN:-}" ]] && echo true || echo false)"

STATE_DIR="${PROJECT_PATH}/.symphony"
WORKFLOW_PATH="${STATE_DIR}/WORKFLOW.md"
CODEX_CONTAINER_HOME="${STATE_DIR}/codex-home"
AGENTS_CONTAINER_HOME="${STATE_DIR}/agents-home"
mkdir -p "${STATE_DIR}/workspaces"

prepare_codex_home() {
  rm -rf "${CODEX_CONTAINER_HOME}"
  rm -rf "${AGENTS_CONTAINER_HOME}"
  mkdir -p "${CODEX_CONTAINER_HOME}/skills"
  mkdir -p "${AGENTS_CONTAINER_HOME}/skills"

  for file in auth.json config.toml version.json .personality_migration; do
    if [[ -f "${CODEX_AUTH_DIR}/${file}" ]]; then
      cp "${CODEX_AUTH_DIR}/${file}" "${CODEX_CONTAINER_HOME}/${file}"
    fi
  done

  if [[ -d "${CODEX_AUTH_DIR}/rules" ]]; then
    cp -R "${CODEX_AUTH_DIR}/rules" "${CODEX_CONTAINER_HOME}/rules"
  fi

  if [[ -d "${CODEX_AUTH_DIR}/skills/.system" ]]; then
    mkdir -p "${CODEX_CONTAINER_HOME}/skills"
    cp -R "${CODEX_AUTH_DIR}/skills/.system" "${CODEX_CONTAINER_HOME}/skills/.system"
  fi

  for entry in find-skills linear-cli; do
    if [[ -L "${CODEX_AUTH_DIR}/skills/${entry}" ]]; then
      cp -a "${CODEX_AUTH_DIR}/skills/${entry}" "${CODEX_CONTAINER_HOME}/skills/${entry}"
    fi
    if [[ -d "${CODEX_AGENTS_DIR}/skills/${entry}" ]]; then
      cp -R "${CODEX_AGENTS_DIR}/skills/${entry}" "${AGENTS_CONTAINER_HOME}/skills/${entry}"
    fi
  done
}

if [[ -f "${WORKFLOW_PATH}" ]] && [[ "${FORCE}" -ne 1 ]]; then
  if ! grep -q 'Generated by go/scripts/run-project-in-docker.sh' "${WORKFLOW_PATH}"; then
    echo "Refusing to overwrite existing non-generated workflow: ${WORKFLOW_PATH}" >&2
    echo "Re-run with --force if you want this script to replace it." >&2
    exit 1
  fi
fi

prepare_codex_home

cat > "${WORKFLOW_PATH}" <<EOF
---
# Generated by go/scripts/run-project-in-docker.sh
tracker:
  kind: linear
  api_key: \$LINEAR_API_KEY
  project_slug: ${LINEAR_PROJECT_SLUG}
polling:
  interval_ms: 30000
workspace:
  root: /project/.symphony/workspaces
hooks:
  after_create: |
    git init .
    git config user.name "\${SYMPHONY_GIT_NAME:-Symphony Bot}" >/dev/null
    git config user.email "\${SYMPHONY_GIT_EMAIL:-symphony@example.invalid}" >/dev/null
    if [ -n "${ORIGIN_URL}" ]; then
      git remote remove origin >/dev/null 2>&1 || true
      git remote add origin ${ORIGIN_URL}
    fi
    git remote remove source >/dev/null 2>&1 || true
    git remote add source /source
    git fetch source ${SOURCE_BRANCH} --depth=1
    git checkout -B ${SOURCE_BRANCH} source/${SOURCE_BRANCH}
    if command -v gh >/dev/null 2>&1 && [ -n "\${GH_TOKEN:-}" ]; then
      gh auth setup-git >/dev/null 2>&1 || true
    fi
  before_run: |
    if [ -d .git ]; then
      git config user.name "\${SYMPHONY_GIT_NAME:-Symphony Bot}" >/dev/null
      git config user.email "\${SYMPHONY_GIT_EMAIL:-symphony@example.invalid}" >/dev/null
      if [ -n "${ORIGIN_URL}" ]; then
        git remote add origin ${ORIGIN_URL} >/dev/null 2>&1 || git remote set-url origin ${ORIGIN_URL}
      fi
      git remote remove source >/dev/null 2>&1 || true
      git remote add source /source >/dev/null 2>&1 || git remote set-url source /source
      git fetch source ${SOURCE_BRANCH} --prune || true
      git checkout -B ${SOURCE_BRANCH} source/${SOURCE_BRANCH} >/dev/null 2>&1 || true
      if command -v gh >/dev/null 2>&1 && [ -n "\${GH_TOKEN:-}" ]; then
        gh auth setup-git >/dev/null 2>&1 || true
      fi
    fi
agent:
  max_concurrent_agents: 10
  max_turns: 20
codex:
  command: node /opt/codex/bin/codex.js app-server
  thread_sandbox: danger-full-access
  tmux_session_prefix: symphony
  turn_sandbox_policy:
    type: danger-full-access
server:
  port: ${PORT}
  host: 0.0.0.0
---
You are working on Linear issue {{ issue.identifier }} inside an isolated workspace cloned from the project mounted at /source.

Project: ${PROJECT_NAME}
Issue title: {{ issue.title }}

Description:
{% if issue.description %}
{{ issue.description }}
{% else %}
No description provided.
{% endif %}

Execution rules:
- Read the relevant project files first before acting, especially docs/ and any existing deliverables/ content from main.
- You can use the dynamic tool linear_graphql to read from and write to Linear.
- If GH_TOKEN is available in the environment, you may use git and gh non-interactively against the repo origin.
- {% if issue.is_review %}This is a REVIEW ticket for {{ issue.review_source_identifier }}. Review tickets should deliver their result in Linear, not in the repo.{% else %}Every non-review ticket must leave meaningful repo-tracked output. Write issue-scoped artifacts under deliverables/{{ issue.identifier }}/ when the work is design, research, planning, or other substantive non-code output.{% endif %}
- {% if issue.is_review %}Do not create a new branch or PR for the review ticket. Inspect the linked source branch/PR, review the relevant repo files, and post a single structured review result comment on this Linear issue using exactly this marker and JSON shape:
  SYMPHONY_REVIEW_RESULT
  {
    "decision": "approve | request_changes | comment_only | blocked",
    "summary": "short reviewer summary",
    "required_changes": ["specific follow-up needed"],
    "residual_risks": ["remaining risk"],
    "reviewed_sha": "optional commit sha"
  }
  Use 'approve' only when the source PR is ready to merge. Use 'request_changes' when the worker should continue. Use 'comment_only' only when you cannot make a final approval decision yet but still have substantive review guidance. Use 'blocked' for concrete external blockers.{% else %}For non-coding tickets, the repo deliverable is required. Examples include plan.md, research.md, backlog.md, summary.md, or decision-log.md as appropriate.{% endif %}
- {% if issue.is_review %}Review tickets should not create status-only commits, review-summary files, or operational repo bookkeeping. Leave the review deliverable in Linear comments only.{% else %}For coding tickets, make the code changes and also add supporting issue-scoped notes under deliverables/{{ issue.identifier }}/ when useful.{% endif %}
- {% if issue.is_review %}Focus on substantive review findings: correctness, completeness, regressions, missing deliverables, and clear next action.{% else %}Create and work from an issue-scoped feature branch, not main. Use a branch name derived from the issue identifier.{% endif %}
- {% if issue.is_review %}Symphony runtime owns PR creation/discovery, review-ticket creation, GitHub-to-Linear status sync, and merge-state bookkeeping when those actions are deterministic.{% else %}Before finishing, stage your repo changes, create a commit, and push the feature branch to origin. If push fails, leave the exact failure in a Linear comment.{% endif %}
- {% if issue.is_review %}Do not spend time on workflow bookkeeping beyond posting the structured review result comment and any concise supporting reviewer notes that are actually helpful.{% else %}If the issue asks for planning, backlog creation, research synthesis, or project management work, do that work in Linear and also persist the substantive results under deliverables/{{ issue.identifier }}/ in the repo. For planning/backlog tasks, create the necessary Linear issues directly, capture dependencies/priorities when possible, and leave a comment on the current issue summarizing what you created and where the repo deliverables live.{% endif %}
- Symphony runtime owns PR creation/discovery, review-ticket creation, GitHub-to-Linear status sync, and merge-state bookkeeping when those actions are deterministic.
- For normal work issues, focus on the substantive work. Push the branch when you have a real deliverable; you do not need to create or update the PR or the review ticket yourself unless the runtime cannot do it.
- Downstream tickets should treat main as the canonical source for prior deliverables, not unmerged feature branches.
- Do not end a turn having done only private analysis. Produce externally visible progress each turn: repo changes, Linear issue creation/updates, or a comment explaining a concrete blocker.
- If the docs are incomplete or ambiguous, create explicit follow-up issues in Linear for the missing decisions instead of silently stopping.
- Use files from the cloned issue workspace as the source of truth before assuming repo docs are inaccessible.
EOF

if [[ "${FORCE_BUILD}" -eq 1 ]] || ! docker image inspect "${IMAGE_NAME}" >/dev/null 2>&1; then
  docker build -t "${IMAGE_NAME}" "${GO_DIR}"
fi

if docker container inspect "${CONTAINER_NAME}" >/dev/null 2>&1; then
  docker rm -f "${CONTAINER_NAME}" >/dev/null
fi

docker run -d \
  --name "${CONTAINER_NAME}" \
  -p "${PORT}:${PORT}" \
  -e GH_TOKEN="${GH_TOKEN:-}" \
  -e GITHUB_TOKEN="${GITHUB_TOKEN:-}" \
  -e LINEAR_API_KEY="${LINEAR_API_KEY}" \
  -e SYMPHONY_GIT_NAME="${SYMPHONY_GIT_NAME:-Symphony Bot}" \
  -e SYMPHONY_GIT_EMAIL="${SYMPHONY_GIT_EMAIL:-symphony@example.invalid}" \
  -v "${PROJECT_PATH}:/project" \
  -v "${PROJECT_PATH}:/source:ro" \
  -v "${CODEX_PACKAGE_ROOT}:/opt/codex:ro" \
  -v "${CODEX_CONTAINER_HOME}:/root/.codex" \
  -v "${AGENTS_CONTAINER_HOME}:/root/.agents:ro" \
  "${IMAGE_NAME}" \
  --port "${PORT}" \
  /project/.symphony/WORKFLOW.md >/dev/null

cat <<EOF
Started ${CONTAINER_NAME}
Project:      ${PROJECT_PATH}
Workflow:     ${WORKFLOW_PATH}
Linear slug:  ${LINEAR_PROJECT_SLUG}
Dashboard:    http://127.0.0.1:${PORT}/
State API:    http://127.0.0.1:${PORT}/api/v1/state

Useful commands:
  docker logs -f ${CONTAINER_NAME}
  docker exec ${CONTAINER_NAME} tmux ls
  docker exec -it ${CONTAINER_NAME} tmux attach -t symphony-<lowercased-issue-identifier>
  docker exec ${CONTAINER_NAME} gh auth status
  docker rm -f ${CONTAINER_NAME}
EOF
