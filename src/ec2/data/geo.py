import functools

import attrs

from ec2.commands import run_command


@attrs.define(frozen=True, kw_only=True)
class Geo:
    region: str
    availability_zone: str

    @functools.cached_property
    def subnet_id(self) -> str:
        filters = [f"Name=availability-zone,Values={self.availability_zone}",
                   "Name=vpc-id,Values=vpc-042628b8054e095ef"]

        get_subnet_id = run_command("aws", "ec2", "describe-subnets",
                                    "--query", "Subnets[0].SubnetId",
                                    "--region", self.region,
                                    "--filters", *filters,
                                    "--output", "text")

        return get_subnet_id.result()
