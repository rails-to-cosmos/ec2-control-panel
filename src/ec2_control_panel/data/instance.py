from __future__ import annotations

import abc
import json
import functools

from typing import Optional
from typing_extensions import Self

import attrs
from jinja2 import Environment, FileSystemLoader

from ec2_control_panel.logger import logger
from ec2_control_panel.commands import ProcessOutput, run_command
from ec2_control_panel.data.geo import Geo
from ec2_control_panel.data.network import ENI
from ec2_control_panel.data.volume import Volume
from ec2_control_panel.data.user_data import UserData
from ec2_control_panel.data.efs import EFS
from ec2_control_panel.package import get_package_root


@attrs.define(kw_only=True)
class Instance(abc.ABC):
    eni: ENI
    geo: Geo
    volume: Volume
    id: str | None = None

    @abc.abstractmethod
    def terminate(self) -> None:
        ...

    @functools.cached_property
    def instance_type(self) -> str:
        if not self.id:
            raise NotImplementedError("Instance type unknown.")

        return run_command("aws", "ec2", "describe-instances",
                           "--instance-ids", self.id,
                           "--query", "Reservations[*].Instances[*].InstanceType",
                           "--region", self.geo.region,
                           "--output", "text").optional()

    @functools.cached_property
    def system_info(self) -> dict[str, str]:
        "Return vCPUs and memory."

        instance_system_info, = run_command("aws", "ec2", "describe-instance-types",
                                            "--instance-types", self.instance_type,
                                            "--query", "InstanceTypes[*].{InstanceType:InstanceType, vCPUs:VCpuInfo.DefaultVCpus, Memory:MemoryInfo.SizeInMiB}",
                                            "--region", self.geo.region,
                                            "--output", "json").json()

        return instance_system_info

    @functools.cached_property
    def private_ip(self) -> Optional[str]:
        if not self.id:
            return None

        return run_command("aws", "ec2", "describe-instances",
                           "--instance-ids", self.id,
                           "--query", "Reservations[*].Instances[*].PrivateIpAddress",
                           "--region", self.geo.region,
                           "--output", "text").optional()

    @property
    def status(self) -> Optional[str]:
        if not self.id:
            return None

        data = run_command("aws", "ec2", "describe-instance-status",
                           "--instance-ids", self.id,
                           "--query", "InstanceStatuses[0]",
                           "--region", self.geo.region,
                           "--output", "json").optional()

        if not data:
            return data

        json_data = json.loads(data)
        print_status = lambda val: f"{val['Status']} ({', '.join(state['Name'] + ' ' + state['Status'] for state in val['Details'])})"

        status = f"Status: {json_data['InstanceState']['Name']}" \
            f", instance: {print_status(json_data['InstanceStatus'])}" \
            f", system: {print_status(json_data['SystemStatus'])}"

        return status

    @classmethod
    def get(cls, eni: ENI, volume: Volume) -> Instance | None:
        geo = volume.geo

        filters = ["Name=tag-key,Values=Name",
                   f"Name=tag-value,Values={volume.name}",
                   f"Name=availability-zone,Values={geo.availability_zone}"]

        response = run_command("aws", "ec2", "describe-volumes",
                               "--filters", *filters,
                               "--query", "Volumes[0].Attachments[0].InstanceId",
                               "--region", geo.region,
                               "--output", "text")

        instance_id = response.optional()
        if not instance_id:
            return None

        filters = [f"Name=availability-zone,Values={geo.availability_zone}"]
        spec_raw = run_command("aws", "ec2", "describe-instances",
                               "--instance-ids", instance_id,
                               "--region", geo.region,
                               "--filters", *filters,
                               "--output", "json").result()

        spec = json.loads(spec_raw)

        try:
            instance_lifecycle = spec["Reservations"][0]["Instances"][0]["InstanceLifecycle"]
        except KeyError:
            instance_lifecycle = "ondemand"

        if instance_lifecycle == "ondemand":
            return OnDemand(eni=eni,
                            geo=geo,
                            volume=volume,
                            id=instance_id)

        request_id: str
        for tag in spec["Reservations"][0]["Instances"][0]["Tags"]:
            if tag["Key"] == "spot-request-id":
                request_id = tag["Value"]
                break
        else:
            raise ValueError(f"Unaable to determine spot-request-id in {spec}")

        return Spot(eni=eni,
                    geo=geo,
                    volume=volume,
                    id=instance_id,
                    request_id=request_id)

    def wait(self) -> ProcessOutput:
        assert self.id, "Unbound instance"

        return run_command("aws", "ec2", "wait", "instance-status-ok",
                           "--instance-ids", self.id,
                           "--region", self.geo.region)

    def mount(self, efs: EFS) -> ProcessOutput:
        assert self.id, "Unbound instance"

        return run_command("aws", "efs", "create-mount-target",
                           "--file-system-id", efs.id,
                           "--subnet-id", self.geo.subnet_id,
                           "--security-group", self.eni.security_group.id,
                           "--region", self.geo.region)


@attrs.define(kw_only=True)
class Spot(Instance):
    request_id: str

    def __str__(self) -> str:
        return f"Spot({self.id})"

    def persist_volume(self, persistent_name: str) -> Volume:
        if not self.id:
            raise ValueError("Instance is not running")

        make_volume_persistent = run_command("aws", "ec2", "modify-instance-attribute",
                                             "--instance-id", self.id,
                                             "--block-device-mappings", "[{\"DeviceName\": \"/dev/sda1\",\"Ebs\":{\"DeleteOnTermination\":false}}]",
                                             "--region", self.geo.region)

        make_volume_persistent.should_not_fail()

        get_volume_id = run_command("aws", "ec2", "describe-instances",
                                    "--instance-ids", self.id,
                                    "--query", "Reservations[*].Instances[0].BlockDeviceMappings[0].Ebs.VolumeId",
                                    "--output", "text",
                                    "--region", self.geo.region)

        volume_id = get_volume_id.result()

        create_tags = run_command("aws", "ec2", "create-tags",
                                  "--resources", volume_id,
                                  "--tags", f"Key=Name,Value={persistent_name}",
                                  "--region", self.geo.region)

        create_tags.should_not_fail()

        return Volume(id=volume_id,
                      name=persistent_name,
                      geo=self.geo)

    @classmethod
    def request(cls,
                name: str,
                ami_id: str,
                instance_type: str,
                pub_key: str,
                instance_role: str,
                eni: ENI,
                geo: Geo,
                user_data: UserData,
                volume_size: int,
                bid_price: str) -> Self:
        log = logger.getChild("spot")

        # SPEC
        root = get_package_root()
        template_dir = root / "templates"
        env = Environment(loader=FileSystemLoader(template_dir))
        spec = env.get_template(name="specs.json.tpl").render({
            "AMI_ID": ami_id,
            "INSTANCE_TYPE": instance_type,
            "PUB_KEY": pub_key,
            "AVAILABILITY_ZONE": geo.availability_zone,
            "INSTANCE_ROLE": instance_role,
            "VOLUME_SIZE": volume_size,
            "ENI_ID": eni.id,
            "USER_DATA": user_data.data,
        })
        # /SPEC

        request_id = run_command(
            "aws", "ec2", "request-spot-instances",
            "--launch-specification", spec,
            "--spot-price", str(bid_price),
            "--query", "SpotInstanceRequests[*].SpotInstanceRequestId",
            "--region", geo.region,
            "--output", "text").result()

        log.info(f"Spot request ID: {request_id}")
        log.info(f"Waiting for spot request {request_id} to fulfill...")
        spot_instance_request_fulfilled = run_command("aws", "ec2", "wait", "spot-instance-request-fulfilled",
                                                      "--spot-instance-request-ids", request_id,
                                                      "--region", geo.region)
        spot_instance_request_fulfilled.should_not_fail()

        get_instance_id = run_command("aws", "ec2", "describe-spot-instance-requests",
                                      "--spot-instance-request-ids", request_id,
                                      "--query", "SpotInstanceRequests[*].InstanceId",
                                      "--output", "text",
                                      "--region", geo.region)
        instance_id = get_instance_id.result()

        tags = [f"Key=Name,Value={name}",
                f"Key=spot-request-id,Value={request_id}",
                "Key=request-type,Value=spot"]

        create_tags = run_command("aws", "ec2", "create-tags",
                                  "--resources", instance_id,
                                  "--tags", *tags,
                                  "--region", geo.region)
        create_tags.should_not_fail()

        log.info(f"Waiting for spot instance {instance_id} to start up...")
        instance_running = run_command("aws", "ec2", "wait", "instance-running",
                                       "--instance-ids", instance_id,
                                       "--region", geo.region)
        instance_running.should_not_fail()
        log.info(f"Requested spot instance ID: {instance_id}")

        get_volume_id = run_command("aws", "ec2", "describe-instances",
                                    "--instance-ids", instance_id,
                                    "--query", "Reservations[*].Instances[*].BlockDeviceMappings[*].Ebs.VolumeId",
                                    "--output", "text",
                                    "--region", geo.region)

        volume_id = get_volume_id.result()

        volume = Volume(id=volume_id,
                        name=name,
                        geo=geo)

        return cls(eni=eni,
                   geo=geo,
                   id=instance_id,
                   volume=volume,
                   request_id=request_id)

    def cancel(self) -> ProcessOutput:
        return run_command("aws", "ec2", "cancel-spot-instance-requests",
                           "--spot-instance-request-ids", self.request_id,
                           "--region", self.geo.region)

    def terminate(self) -> None:
        if not self.id:
            raise ValueError("Instance not found")

        self.cancel().should_not_fail()

        terminate = run_command("aws", "ec2", "terminate-instances",
                                "--instance-ids", self.id,
                                "--region", self.geo.region)
        terminate.should_not_fail()

        self.volume.wait_available().should_not_fail()
        self.eni.wait().should_not_fail()


class OnDemand(Instance):
    def __str__(self) -> str:
        return f"OnDemand({self.id})"

    @classmethod
    def request(cls,
                name: str,
                ami_id: str,
                instance_type: str,
                pub_key: str,
                instance_role: str,
                eni: ENI,
                geo: Geo,
                user_data: UserData,
                volume_size: int) -> Self:

        root = get_package_root()
        template_dir = root / "templates"
        env = Environment(loader=FileSystemLoader(template_dir))

        # Make spec
        spec = env.get_template(name="specs.json.tpl").render({
            "AMI_ID": ami_id,
            "INSTANCE_TYPE": instance_type,
            "PUB_KEY": pub_key,
            "AVAILABILITY_ZONE": geo.availability_zone,
            "INSTANCE_ROLE": instance_role,
            "VOLUME_SIZE": volume_size,
            "ENI_ID": eni.id,
            "USER_DATA": user_data.data,
        })

        create_launch_template = run_command("aws", "ec2", "create-launch-template",
                                             "--launch-template-name", name,
                                             "--version-description", "version1",
                                             "--launch-template-data", spec,
                                             "--region", geo.region,)

        try:
            create_launch_template.should_not_fail()
        except RuntimeError:
            delete_launch_template = run_command("aws", "ec2", "delete-launch-template",
                                                 "--launch-template-name", name,
                                                 "--region", geo.region)
            delete_launch_template.should_not_fail()
            create_launch_template = run_command("aws", "ec2", "create-launch-template",
                                                 "--launch-template-name", name,
                                                 "--version-description", "version1",
                                                 "--launch-template-data", spec,
                                                 "--region", geo.region)
            create_launch_template.should_not_fail()

        run_instance = run_command("aws", "ec2", "run-instances",
                                   "--placement", f"AvailabilityZone={geo.availability_zone}",
                                   "--launch-template", f"LaunchTemplateName={name}",
                                   "--region", geo.region)

        instance_data = run_instance.json()
        instance_id = instance_data["Instances"][0]["InstanceId"]

        tags = [f"Key=Name,Value={name}",
                "Key=request-type,Value=ondemand"]

        create_tags = run_command("aws", "ec2", "create-tags",
                                  "--resources", instance_id,
                                  "--tags", *tags,
                                  "--region", geo.region)

        create_tags.should_not_fail()

        get_volume_id = run_command("aws", "ec2", "describe-instances",
                                    "--instance-ids", instance_id,
                                    "--query", "Reservations[*].Instances[*].BlockDeviceMappings[*].Ebs.VolumeId",
                                    "--output", "text",
                                    "--region", geo.region)

        volume_id = get_volume_id.result()

        volume = Volume(id=volume_id,
                        name=name,
                        geo=geo)

        return cls(eni=eni,
                   geo=geo,
                   volume=volume,
                   id=instance_id)

    def terminate(self) -> None:
        if not self.id:
            raise ValueError("Instance not found")

        terminate = run_command("aws", "ec2", "terminate-instances",
                                "--instance-ids", self.id,
                                "--region", self.geo.region)

        terminate.should_not_fail()
        self.volume.wait_available().should_not_fail()
        self.eni.wait().should_not_fail()
