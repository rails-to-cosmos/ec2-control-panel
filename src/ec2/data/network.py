from __future__ import annotations

import attrs

from ec2.data.geo import Geo
from ec2.commands import run_command, InvalidOutput, ProcessOutput
from ec2.logger import logger


@attrs.define(frozen=True, kw_only=True)
class ENI:
    id: str
    geo: Geo

    @classmethod
    def create(cls, name: str, geo: Geo, security_group: str = "sg-0004eeb822745ac47") -> ENI:
        eni_id = run_command("aws", "ec2", "create-network-interface",
                             "--subnet-id", geo.subnet_id,
                             "--groups", security_group,
                             "--query", "NetworkInterface.NetworkInterfaceId",
                             "--region", geo.region,
                             "--output", "text").result()

        tags_creation = run_command("aws", "ec2", "create-tags",
                                    "--resources", eni_id,
                                    "--tags", f"Key=Name,Value={name}",
                                    "--region", geo.region)

        tags_creation.should_not_fail()

        return ENI(id=eni_id, geo=geo)

    @classmethod
    def get(cls, name: str, geo: Geo) -> ENI | None:
        filters = [f"Name=availability-zone,Values={geo.availability_zone}",
                   "Name=tag-key,Values=Name",
                   f"Name=tag-value,Values={name}"]

        getter = run_command("aws", "ec2", "describe-network-interfaces",
                             "--filters", *filters,
                             "--query", "NetworkInterfaces[0].NetworkInterfaceId",
                             "--region", geo.region,
                             "--output", "text")

        match getter.value:
            case str() as eni_id:
                return ENI(id=eni_id, geo=geo)
            case InvalidOutput():
                return None
            case RuntimeError() as exc:
                raise exc

    @classmethod
    def get_or_create(cls, name: str, geo: Geo) -> ENI:
        log = logger.getChild("network")

        match cls.get(name=name, geo=geo):
            case ENI() as result:
                return result
            case _:
                log.info("Network interface not found")
                log.info("Creating network interface...")
                return cls.create(name=name, geo=geo)

    def wait(self) -> ProcessOutput:
        return run_command("aws", "ec2", "wait", "network-interface-available",
                           "--network-interface-ids", self.id,
                           "--region", self.geo.region)
