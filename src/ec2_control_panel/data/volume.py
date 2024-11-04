from __future__ import annotations

import attrs
from typing_extensions import Self

from ec2_control_panel.data.geo import Geo
from ec2_control_panel.commands import run_command, ProcessOutput


@attrs.define(kw_only=True, frozen=True)
class Volume:
    id: str
    name: str
    geo: Geo

    @classmethod
    def get(cls, name: str, geo: Geo) -> Self | None:
        query = "Volumes[0].VolumeId"

        filters = ["Name=tag-key,Values=Name",
                   f"Name=tag-value,Values={name}",
                   f"Name=availability-zone,Values={geo.availability_zone}",]

        get_volume = run_command("aws", "ec2", "describe-volumes",
                                 "--filters", *filters,
                                 "--query", query,
                                 "--region", geo.region,
                                 "--output", "text")

        match get_volume.optional():
            case None:
                return None
            case volume_id:
                return cls(id=volume_id,
                           name=name,
                           geo=geo)

    def wait_available(self) -> ProcessOutput:
        return run_command("aws", "ec2", "wait", "volume-available",
                           "--volume-ids", self.id,
                           "--region", self.geo.region)

    def wait_in_use(self) -> ProcessOutput:
        return run_command("aws", "ec2", "wait", "volume-in-use",
                           "--volume-ids", self.id,
                           "--region", self.geo.region)
