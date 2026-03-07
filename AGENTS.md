# Repository Guidelines

## Project Structure & Module Organization
- `lib/` Ruby sources; CLI commands under `lib/confctl/cli`, Nix build helpers in `lib/confctl/nix*`, health checks in `lib/confctl/health_checks`.
- `bin/confctl` entrypoint; `libexec/` helper scripts; `template/` ERB for generated docs; rendered man pages live in `man/man8/`.
- `nix/` contains Nix modules and overlays; `example/` provides a sample cluster and swpins setup; `docs/` holds focused guides; release gems are stored in `pkg/`.

## Build, Test, and Development Commands
- Enter `nix develop` to get Ruby, bundler, nixfmt, and the `confctl` binstub wired to the local sources.
- `bundle exec rubocop` lints Ruby; pre-commit hook also runs `nixfmt` on Nix files.
- `bundle exec rspec` (or `bundle exec rake spec`) runs the automated Ruby tests.
- Format Nix code with `nixfmt` (or `nixfmt-tree` to format directories) from inside the shell.
- `bundle exec rake md2man:man` regenerates manpages; `bundle exec rake confctl-options` rebuilds the options reference from templates.
- Package the gem with `bundle exec rake build` (outputs to `pkg/`) and install locally with `bundle exec rake install`.

## Coding Style & Naming Conventions
- Ruby target version 3.1; use 2-space indents, LF endings, UTF-8 (`.editorconfig`).
- Snake_case for methods/variables, CamelCase for classes/modules, kebab-case for CLI commands/flags; keep option names consistent with existing Nix modules.
- RuboCop config is permissive on metrics—still prefer small, single-purpose methods and early returns; avoid introducing new global state.

## Testing Guidelines
- Run `bundle exec rspec` for automated coverage and `bundle exec rubocop` for linting.
- Keep focused manual runs of the commands you touch (build, deploy, swpins, health checks) against a local configuration such as `example/`.
- When fixing a bug, add a minimal regression check in RSpec if practical and document the manual steps you executed.

## Commit & Pull Request Guidelines
- Commit subjects are short and imperative. Without a prefix, capitalize the first word. With a `topic/component:` prefix, keep the first word after `:` lowercase. Avoid trailing periods and bundle related changes together.
- Flake input updates (`vpsadminos`) flow:
  1. Read current rev: `nix flake metadata --json . | jq -r '.locks.nodes.vpsadminos.locked.rev'`.
  2. Update input: `nix flake update vpsadminos` (or `nix flake lock --update-input vpsadminos`).
  3. Verify only `flake.lock` changed for this update commit.
  4. Commit with subject format: `flake: vpsadminos <old9> -> <new9>` (example: `flake: vpsadminos 2cab01000 -> 08bb11324`).
- Describe PRs clearly with motivation, scope, and manual verification notes; link relevant issues and update docs/manpages when behavior changes.
- Run rspec, rubocop, and nixfmt before pushing; ensure generated artifacts (`man/`, `pkg/` when releasing) reflect your changes.
