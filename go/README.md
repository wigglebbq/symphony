# Symphony Go

This directory contains a Go implementation of Symphony based on [`SPEC.md`](../SPEC.md).

## Run

```bash
cd go
go run ./cmd/symphony ../WORKFLOW.md
```

If no workflow path is passed, the binary defaults to `WORKFLOW.md` in the current working
directory.

## Current scope

Implemented:

- `WORKFLOW.md` loading with YAML front matter
- Typed config defaults and environment indirection
- Linear polling for active issues
- Workspace creation, reuse, cleanup, and shell hooks
- Local Codex app-server sessions over stdio
- Optional SSH worker extension
- Multi-turn worker continuation
- Orchestrator polling, reconciliation, stall detection, and retry scheduling
- Optional HTTP dashboard and JSON API
- Filesystem watch plus poll-time defensive reload
- Structured stderr logging

Not implemented yet:

- Rich dashboard UI

## HTTP API

When `server.port` is set in `WORKFLOW.md` or `--port` is passed, Symphony serves:

- `GET /`
- `GET /api/v1/state`
- `GET /api/v1/<issue_identifier>`
- `POST /api/v1/refresh`

## Docker

Build:

```bash
cd go
docker build -t symphony-go .
```

Run:

```bash
docker run --rm \
  -e LINEAR_API_KEY \
  -v "$PWD/../WORKFLOW.md:/work/WORKFLOW.md:ro" \
  -v "$HOME/.codex:/root/.codex" \
  symphony-go /work/WORKFLOW.md
```

Important:

- The provided image contains the Symphony binary, `bash`, `git`, and `ssh`, but not the Codex CLI.
- The recommended local-dev path is the launcher script below, which mounts your host-installed
  Codex package into the container and runs it with the container's Node runtime.
- If you do not want the container itself to host Codex, use `worker.ssh_hosts` so the container
  orchestrates work while remote workers run `codex app-server`.

## Project Launcher

Use [run-project-in-docker.sh](/home/davidcky/workspace/symphony/go/scripts/run-project-in-docker.sh)
to point Symphony at a local project path, generate `.symphony/WORKFLOW.md`, build the image if
needed, and start a container:

```bash
export LINEAR_API_KEY=...
./go/scripts/run-project-in-docker.sh /path/to/your/project
```

Optional flags:

- `--linear-project-slug your-linear-slug`
- `--port 4110`
- `--container-name symphony-my-project`
- `--force`
- `--build`
