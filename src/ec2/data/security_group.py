import attrs


@attrs.define(frozen=True, kw_only=True)
class SecurityGroup:
    id: str
