# knowledge-database
Data store to serve multiple services with bookmarks/favourites 

## Stack

- Go HTTP server
- HTMX
- Tailwind CSS + DaisyUI
- Docker + Docker Compose only (no host `go build` and no host `npm install`)

## Run

1. Start Docker Desktop.
2. Set values in `.env`:
	- `GITHUB_PAT`: GitHub personal access token
	- `GITHUB_REPO_URL`: repository to clone on startup
3. Run `run.bat` from this repository root.
4. Open `http://localhost:8080`.

On startup, the app clones `GITHUB_REPO_URL` into `./repo` using `GITHUB_PAT`.

`run.bat` does:

- `docker compose down`
- `docker compose up --build` (attached)
