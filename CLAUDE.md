# Poly - Polymarket Telegram Trading Bot

## Coding Guidelines

Follow the Go conventions in [~/guidelines/golang.md](~/guidelines/golang.md).

## Test-Driven Development (TDD)

All new features and bug fixes MUST follow the TDD cycle:

1. **Red** — Write a failing test that defines the expected behavior before writing any implementation code
2. **Green** — Write the minimum code to make the test pass
3. **Refactor** — Clean up the implementation while keeping tests green

### Rules
- No production code without a corresponding test
- Run `go test ./...` before considering any change complete
- Tests go in `*_test.go` files using table-driven tests where appropriate
- Use `t.Helper()` for test helpers, `t.Parallel()` where safe
- For bug fixes: first write a test that reproduces the bug, then fix it
- Mock external dependencies (Telegram API, Polymarket API, Polygon RPC) — never call real services in tests
- Aim for meaningful coverage, not 100% — focus on business logic and edge cases

### Watching Changes
```bash
watch -n1 -c 'git diff --stat'          # live summary in a split pane
watch -n1 -c 'git diff --color=always'  # live full diff in a split pane
```

### Running Tests
```bash
go test ./...                          # all tests
go test -v ./internal/telegram/...     # single package
go test -run TestFunctionName ./...    # single test
go test -race ./...                    # with race detector
```

## Project Structure

```
internal/
  telegram/     - Bot handlers, callbacks, state management
  polymarket/   - Polymarket CLOB API client
  blockchain/   - Polygon RPC, contract interactions
  database/     - PostgreSQL connection and repositories/
  wallet/       - Key generation, encryption, proxy wallet mgmt
  live/         - Live trade feed (WebSocket), activity manager
  config/       - Environment config loading
docs/           - Architecture spec, deployment, feature docs
```

## Key Patterns

- **Dual wallet architecture**: EOA signs transactions, proxy wallet (Gnosis Safe) holds assets
- **Callback data format**: `action:param1:param2:...` (Telegram max 64 bytes)
- **UTF-8 safety**: Use `truncateUTF8()` for display string truncation — never byte-slice strings that may contain non-ASCII (market titles, etc.)
- **Market data**: Fetched from Polymarket CLOB API, cached with short TTLs
- **Encrypted keys**: AES-256-GCM, decrypted only for signing

## Deploy

### Release flow
1. Tag a new version: `git tag v0.0.XX && git push origin v0.0.XX`
2. CI (Docker Release workflow) builds multi-arch image and pushes to `cheshire42/poly`
3. Deploy on the Raspberry Pi:

```bash
cd ~/workspace/poly_deploy
# Update image tag in docker-compose.yml
docker compose down && docker compose up -d
docker compose logs --tail 30    # verify startup
```

### Production environment
- **Deploy dir**: `~/workspace/poly_deploy/` (separate from source repo)
- **Image**: `cheshire42/poly:<tag>` on Docker Hub
- **Infra**: Raspberry Pi, PostgreSQL and Redis on host, bot in Docker
- **Config**: `.env` file in deploy dir, DB/Redis connect via `host.docker.internal`

## Architecture

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full technical specification.
