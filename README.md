# knowledge-database
Data store to serve multiple services with bookmarks/favourites 
./
## Stack

- Go HTTP server
- Air (Go live reload)
- HTMX
- Tailwind CSS + DaisyUI
- Docker + Docker Compose only (no host `go build` and no host `npm install`)

## Run

1. Start Docker Desktop.
2. Set values in `.env`:
	- `GITHUB_PAT`: GitHub personal access token
	- `GITHUB_REPO_URL`: repository to clone on startup
3. Run `run.bat` from this repository root.
4. Open `http://127.0.0.1:8080`.

On startup, the app clones `GITHUB_REPO_URL` into `./repo` using `GITHUB_PAT`.
In development, changes in `cmd/**/*.go` and `web/templates/**/*.html` automatically rebuild and restart the server via Air.

`run.bat` does:

- `docker compose down`
- `docker compose up --build` (attached)

## Playwright MCP (Project Only)

This repository includes a workspace-only MCP server config at `.vscode/mcp.json`.
It does not install Playwright MCP globally.

1. Open this repository in VS Code.
2. Run `MCP: List Servers` and start `playwright` if it is not running.
3. Accept the trust prompt for the workspace server.

Server config used:

- `command`: `npx`
- `args`: `-y @playwright/mcp@0.0.76`
