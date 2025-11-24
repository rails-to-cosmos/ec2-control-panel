.PHONY: typecheck
typecheck:
	uv run mypy src
	uv run mypy tests

.PHONY: upgrade
upgrade:
	uv sync --upgrade-package marimo --dev

.PHONY: run
run:
	uv run marimo run app.py
