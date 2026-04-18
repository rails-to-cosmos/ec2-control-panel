import configparser
import os
from dataclasses import dataclass


@dataclass(frozen=True)
class InstanceConfig:
    session_id: str
    region: str
    availability_zone: str
    vpc_id: str
    security_group: str
    ami_id: str
    instance_type: str
    instance_role: str
    public_key: str
    request_type: str
    bid_price: str
    volume_size: int
    instance_volume_size: int


SEARCH_PATHS = [
    "/etc/ec2-control-panel/instances.conf",
    "./instances.conf",
]


class Config:
    def __init__(self, parser: configparser.ConfigParser, path: str):
        self._parser = parser
        self._path = path

    @classmethod
    def load(cls, path: str | None = None) -> "Config":
        if path is None:
            path = os.environ.get("EC2_CONFIG", "")
        if path:
            candidates = [path]
        else:
            candidates = SEARCH_PATHS
        parser = configparser.ConfigParser()
        for candidate in candidates:
            if parser.read(candidate):
                return cls(parser, candidate)
        tried = ", ".join(candidates)
        raise FileNotFoundError(f"Config file not found (searched: {tried})")

    def resolve(self, session_id: str) -> InstanceConfig:
        if not self._parser.has_section(session_id):
            raise KeyError(session_id)
        section = self._parser[session_id]
        return InstanceConfig(
            session_id=session_id,
            region=section["region"],
            availability_zone=section["availability_zone"],
            vpc_id=section["vpc_id"],
            security_group=section["security_group"],
            ami_id=section["ami_id"],
            instance_type=section["instance_type"],
            instance_role=section["instance_role"],
            public_key=section["public_key"],
            request_type=section["request_type"],
            bid_price=section["bid_price"],
            volume_size=int(section["volume_size"]),
            instance_volume_size=int(section["instance_volume_size"]),
        )

    def list_instances(self) -> list[str]:
        return self._parser.sections()

    def create(self, session_id: str, **overrides: str) -> None:
        if not os.path.exists(self._path):
            self._parser[configparser.DEFAULTSECT] = {}
        self._parser.add_section(session_id)
        for key, value in overrides.items():
            self._parser.set(session_id, key, str(value))
        with open(self._path, "w") as f:
            self._parser.write(f)
