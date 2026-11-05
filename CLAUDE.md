# CLAUDE.md

Instructions for AI agents working in this repo. See README.md for what the repo is and how to build and test it.

## Commits

- Don't add `Co-Authored-By:` trailers (or any other AI attribution) to commit messages. `.claude/settings.json` turns off Claude Code's default commit trailer; follow the same rule with any other tool.
- Commit to `main` directly. Use branches or worktrees only to parallelize work, then merge back to `main`.

## Public repo

This repo is public. Before pushing, check that nothing secret or private is staged: tokens (`smb_…`, `smc_…`), `.envrc`, private LAN addresses, node pubkeys of real hosts. `docs/live-testing.md` stays local (it's gitignored).

## Changes that touch the relay

The relay library is deployed by `dpup/sierra-data` (`cmd/mesh-relay`), which deploys from its `main`. Ship a relay change by tagging a release here, then bumping `github.com/dpup/sierra-meshrelay` in sierra-data through a pull request. Never push to sierra-data's `main`.
