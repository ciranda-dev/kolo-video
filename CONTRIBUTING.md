# Contributing to kolo-video

Thank you for helping with kolo-video (camera capture, raw frames, VP8/VP9 and optional H.264 for Kolo). Bug reports, feature ideas, documentation fixes and pull requests are all welcome. Everyone taking part follows the [Code of Conduct](CODE_OF_CONDUCT.md).

## Report a bug or ask for a feature

Use the issue forms: open a new issue and pick "Bug report" or "Feature request". Search the existing issues first. A bug report needs the version you use, your system, the steps to reproduce it and what you expected. Remove keys, tokens, names and addresses from any logs before you paste them. For a question, see [SUPPORT.md](SUPPORT.md).

Do not report a security problem in a public issue. Use the [private report](https://github.com/ciranda-dev/kolo-video/security/advisories/new) (see also [SECURITY.md](SECURITY.md)).

## Set up

You need the Go version in `go.mod` or later. Clone your fork and run the checks in the repository folder:

```sh
go fmt ./...
go vet ./...
go test ./...
go test -race ./...
CGO_ENABLED=0 go build ./...
GOOS=linux CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go vet ./...
GOOS=windows CGO_ENABLED=0 go vet ./...
GOOS=linux CGO_ENABLED=0 go vet -tags kolovideo_native ./...
build/verify.sh
staticcheck ./...
```

`staticcheck` is the [staticcheck](https://staticcheck.dev) tool: `go install honnef.co/go/tools/cmd/staticcheck@latest`. If your checkout sits inside a Go workspace (a `go.work` file in a parent folder), add `GOWORK=off` to test against the published versions of the other Kolo libraries. Read `AGENTS.md` for the details of this repository, and `README.md` for what it does.

## Conventions

- Commit messages and pull request titles follow [Conventional Commits](https://www.conventionalcommits.org): `type(scope): description`, in English, imperative and lowercase, with no period at the end. Types: `feat`, `fix`, `docs`, `refactor`, `test`, `perf`, `ci`, `chore`. A breaking change uses `feat!` (or another type with `!`) or a `BREAKING CHANGE:` footer.
- A change in behavior comes with a test.
- Write simple, idiomatic Go and use the standard library first. Every exported symbol has a doc comment that starts with its name.
- Keep comments short: a one-line doc comment on an exported symbol, and a comment elsewhere only for a non-obvious reason.
- Write English everywhere: code, comments, errors, tests, docs and commit messages. No em dashes.
- Do not add a dependency without asking first: open an issue.
- Never commit a `replace` directive in `go.mod`. For local work across Kolo repositories use a `go.work` file in the parent folder, which is not committed.
- Tests, examples and scripts never read or write outside a temporary directory, and tests use `127.0.0.1` only.
- Never commit key files (`*.key`), `.env` files, tokens or other secrets.

## Send a pull request

1. Fork the repository and create a branch named `type/short-description`, for example `fix/handle-empty-input` or `docs/clarify-install`.
2. Keep the pull request small and about one thing. For a big change, open an issue first and agree on the approach before you write the code.
3. Make the checks above pass.
4. Record every user-visible change in `CHANGELOG.md` under `[Unreleased]`.
5. Fill in the pull request template: the summary, the type of change, how you tested it and the checklist.
6. The type in the title of your pull request should match the box you checked under "Type of change": `fix` for a bug fix, `feat` for a new feature, `docs` for documentation only, `refactor` for a refactor or cleanup, `test` for tests only, `chore` for a dependency update, and `feat!` (or a `BREAKING CHANGE:` footer) for a breaking change.

## License

Unless you explicitly state otherwise, any contribution intentionally submitted for inclusion in the work by you, as defined in the license, shall be licensed as MIT OR Apache-2.0, without any additional terms or conditions.

## Status

kolo-video is in beta. The API may change until 1.0. Issues and pull requests are read as time allows, and a response time is not guaranteed.
