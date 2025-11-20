.PHONY: typecheck
typecheck:
	uv run mypy src
	uv run mypy tests

.PHONY: upgrade
upgrade:
	uv sync --upgrade-package marimo --dev
