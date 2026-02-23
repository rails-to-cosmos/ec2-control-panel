from __future__ import annotations

import json

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

    def attach(self, instance_id: str, device: str) -> ProcessOutput:
        return run_command("aws", "ec2", "attach-volume",
                           "--volume-id", self.id,
                           "--instance-id", instance_id,
                           "--device", device,
                           "--region", self.geo.region)

    def force_detach_if_stale(self) -> None:
        """Detach volume if it's attached to a non-running instance."""
        query = "Volumes[0].{State:State,InstanceId:Attachments[0].InstanceId}"

        result = run_command("aws", "ec2", "describe-volumes",
                             "--volume-ids", self.id,
                             "--query", query,
                             "--region", self.geo.region,
                             "--output", "json")

        info = json.loads(result.result())

        if info["State"] != "in-use":
            return

        instance_id = info["InstanceId"]

        instance_state_result = run_command(
            "aws", "ec2", "describe-instances",
            "--instance-ids", instance_id,
            "--query", "Reservations[0].Instances[0].State.Name",
            "--region", self.geo.region,
            "--output", "text",
        )

        state = instance_state_result.optional()
        if state in ("terminated", "stopped", "shutting-down"):
            print(f"Volume {self.id} attached to {state} instance {instance_id}, force-detaching")
            run_command("aws", "ec2", "detach-volume",
                        "--volume-id", self.id,
                        "--force",
                        "--region", self.geo.region).should_not_fail()
            self.wait_available()
