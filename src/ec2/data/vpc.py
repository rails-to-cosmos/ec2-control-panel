import attrs


@attrs.define(frozen=True, kw_only=True)
class VPC:
    id: str
