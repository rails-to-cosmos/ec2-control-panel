from __future__ import annotations

import json
from pathlib import Path
from typing import Literal

import attrs

RequestType = Literal["spot", "ondemand"]

ALLOWED_OVERRIDES = {"name", "availability_zone", "instance_type", "volume_size", "request_type"}


@attrs.define(frozen=True, kw_only=True)
class InstanceConfig:
    name: str
    availability_zone: str | None = None
    instance_type: str | None = None
    volume_size: int | None = None
    request_type: RequestType | None = None


def _find_instances_file() -> Path:
    # Repo layout: src/ec2_control_panel/instances.py -> ../../instances.json
    candidate = Path(__file__).resolve().parents[2] / "instances.json"
    if candidate.is_file():
        return candidate
    fallback = Path.cwd() / "instances.json"
    if fallback.is_file():
        return fallback
    raise FileNotFoundError(
        f"instances.json not found at {candidate} or {fallback}. "
        "Create one at the repository root."
    )


def load_instances() -> dict[str, InstanceConfig]:
    path = _find_instances_file()
    with path.open() as f:
        raw = json.load(f)
    if not isinstance(raw, dict):
        raise ValueError(f"{path} must be a JSON object mapping name -> overrides")
    instances: dict[str, InstanceConfig] = {}
    for name, overrides in raw.items():
        if not isinstance(overrides, dict):
            raise ValueError(
                f"{path}: '{name}' must map to an object, got {type(overrides).__name__}"
            )
        unknown = set(overrides) - ALLOWED_OVERRIDES
        if unknown:
            raise ValueError(
                f"{path}: '{name}' has unknown override keys {sorted(unknown)}. "
                f"Allowed: {sorted(ALLOWED_OVERRIDES)}"
            )
        if (rt := overrides.get("request_type")) is not None and rt not in ("spot", "ondemand"):
            raise ValueError(
                f"{path}: '{name}' has invalid request_type '{rt}'. Must be 'spot' or 'ondemand'."
            )

        if "name" in overrides:
            instance_name = overrides["name"]
            instance_config = overrides
        else:
            instance_name = name
            instance_config = dict(name=name, **overrides)

        instances[instance_name] = InstanceConfig(**instance_config)
    return instances


def get_instance_config(session_id: str) -> InstanceConfig:
    instances = load_instances()
    if session_id not in instances:
        raise ValueError(
            f"Unknown instance '{session_id}'. Add it to instances.json."
        )
    return instances[session_id]
