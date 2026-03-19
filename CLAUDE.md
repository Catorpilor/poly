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

## Architecture

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full technical specification.
