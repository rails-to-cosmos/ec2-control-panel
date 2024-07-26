import attrs


@attrs.define(frozen=True, kw_only=True)
class SecurityGroup:
    id: str = "sg-0004eeb822745ac47"
