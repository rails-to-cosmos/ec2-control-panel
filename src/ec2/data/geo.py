import functools

import attrs

from ec2.commands import run_command
from ec2.data.vpc import VPC


@attrs.define(frozen=True, kw_only=True)
class Geo:
    region: str
    availability_zone: str
    vpc: VPC = attrs.field(factory=VPC)

    @functools.cached_property
    def subnet_id(self) -> str:
        filters = [f"Name=availability-zone,Values={self.availability_zone}",
                   f"Name=vpc-id,Values={self.vpc.id}"]

        get_subnet_id = run_command("aws", "ec2", "describe-subnets",
                                    "--query", "Subnets[0].SubnetId",
                                    "--region", self.region,
                                    "--filters", *filters,
                                    "--output", "text")

        return get_subnet_id.result()
